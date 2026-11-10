//go:build darwin

package macverify

import (
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/procbound"
)

const (
	dsclPath        = "/usr/bin/dscl"
	dseditgroupPath = "/usr/sbin/dseditgroup"
	visudoPath      = "/usr/sbin/visudo"
	pfctlPath       = "/sbin/pfctl"
	launchctlPath   = "/bin/launchctl"
	pkillPath       = "/usr/bin/pkill"
	chflagsPath     = "/usr/bin/chflags"
	// launchctlNotFound is launchctl print's status for an unknown service.
	launchctlNotFound = 113
)

// systemHost acts on this Mac through the standard administration tools,
// each by absolute path.
type systemHost struct{}

// NewSystemHost returns the Host that acts on this Mac.
func NewSystemHost() (Host, error) { return systemHost{}, nil }

func run(name string, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(context.Background(), name, args...) //nolint:gosec // G204: fixed tool paths; arguments are validated spec values
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := procbound.Run(cmd, 0); err != nil {
		return stdout.Bytes(), fmt.Errorf("%s %s: %w: %s", filepath.Base(name), strings.Join(args, " "), err,
			strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func (systemHost) Privileged() bool { return os.Geteuid() == 0 }

// readRecord reads a local directory-service record's attributes. A missing
// record reports false; any other failure is an error.
func readRecord(path string, attrs ...string) (map[string][]string, bool, error) {
	out, err := run(dsclPath, append([]string{"-plist", ".", "-read", path}, attrs...)...)
	if err != nil {
		if strings.Contains(err.Error(), "eDSRecordNotFound") {
			return nil, false, nil
		}
		return nil, false, err
	}
	record, err := parseDSPlist(out)
	return record, err == nil, err
}

// parseDSPlist reads dscl -plist output: a dict of attribute name to an array
// of strings. dscl prefixes a name it maps to a standard attribute with
// "dsAttrTypeStandard:" and any other with "dsAttrTypeNative:" (IsHidden is
// native), so both prefixes are stripped and callers ask by the bare name.
func parseDSPlist(data []byte) (map[string][]string, error) {
	record := make(map[string][]string)
	dec := xml.NewDecoder(bytes.NewReader(data))
	var key, element string
	inArray := false
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return record, nil
		}
		if err != nil {
			return nil, fmt.Errorf("parse dscl output: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			element = t.Name.Local
			if element == "array" {
				inArray = true
				record[key] = []string{}
			}
		case xml.EndElement:
			if t.Name.Local == "array" {
				inArray = false
			}
			element = ""
		case xml.CharData:
			switch {
			case element == "key":
				key = strings.TrimPrefix(strings.TrimPrefix(string(t), "dsAttrTypeStandard:"), "dsAttrTypeNative:")
			case element == "string" && inArray:
				record[key] = append(record[key], string(t))
			}
		}
	}
}

func single(record map[string][]string, attr string) string {
	if values := record[attr]; len(values) > 0 {
		return values[0]
	}
	return ""
}

// singleInt reads a numeric attribute, unsetID when the record has none.
func singleInt(record map[string][]string, attr string) (int, error) {
	if len(record[attr]) == 0 {
		return unsetID, nil
	}
	n, err := strconv.Atoi(single(record, attr))
	if err != nil {
		return 0, fmt.Errorf("attribute %s: %w", attr, err)
	}
	return n, nil
}

func (systemHost) User(name string) (Account, bool, error) {
	record, ok, err := readRecord("/Users/"+name,
		"UniqueID", "PrimaryGroupID", "NFSHomeDirectory", "UserShell", "Password", "IsHidden")
	if err != nil || !ok {
		return Account{}, ok, err
	}
	a, err := accountFromRecord(name, record)
	return a, err == nil, err
}

// accountFromRecord reads the attributes User asks dscl for.
func accountFromRecord(name string, record map[string][]string) (Account, error) {
	a := Account{
		Name: name, Home: single(record, "NFSHomeDirectory"), Shell: single(record, "UserShell"),
		Password: single(record, "Password"), Hidden: single(record, "IsHidden") == "1",
	}
	var err error
	if a.UID, err = singleInt(record, "UniqueID"); err != nil {
		return Account{}, fmt.Errorf("account %s: %w", name, err)
	}
	if a.PrimaryGID, err = singleInt(record, "PrimaryGroupID"); err != nil {
		return Account{}, fmt.Errorf("account %s: %w", name, err)
	}
	return a, nil
}

// listByAttribute reads `dscl . -list <dir> <attr>`: one record name and its
// value per line.
func listByAttribute(dir, attr string) ([][2]string, error) {
	out, err := run(dsclPath, ".", "-list", dir, attr)
	if err != nil {
		return nil, err
	}
	var rows [][2]string
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 {
			rows = append(rows, [2]string{fields[0], fields[len(fields)-1]})
		}
	}
	return rows, scanner.Err()
}

func namesWith(dir, attr string, value int) ([]string, error) {
	rows, err := listByAttribute(dir, attr)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, row := range rows {
		if row[1] == strconv.Itoa(value) {
			names = append(names, row[0])
		}
	}
	return names, nil
}

func firstName(names []string, err error) (string, bool, error) {
	if err != nil || len(names) == 0 {
		return "", false, err
	}
	return names[0], true, nil
}

func (systemHost) UserNameByID(uid int) (string, bool, error) {
	return firstName(namesWith("/Users", "UniqueID", uid))
}

func (systemHost) UsersWithPrimaryGroup(gid int) ([]string, error) {
	return namesWith("/Users", "PrimaryGroupID", gid)
}

func (systemHost) Group(name string) (Group, bool, error) {
	record, ok, err := readRecord("/Groups/"+name, "PrimaryGroupID", "GroupMembership")
	if err != nil || !ok {
		return Group{}, ok, err
	}
	gid, err := singleInt(record, "PrimaryGroupID")
	if err != nil {
		return Group{}, false, fmt.Errorf("group %s: %w", name, err)
	}
	return Group{Name: name, GID: gid, Members: record["GroupMembership"]}, true, nil
}

func (systemHost) GroupNameByID(gid int) (string, bool, error) {
	return firstName(namesWith("/Groups", "PrimaryGroupID", gid))
}

func (systemHost) CreateGroup(name string, gid int) error {
	_, err := run(dseditgroupPath, "-o", "create", "-i", strconv.Itoa(gid), name)
	return err
}

func (systemHost) WriteUser(a Account) error {
	path := "/Users/" + a.Name
	hidden := "0"
	if a.Hidden {
		hidden = "1"
	}
	steps := [][]string{
		{"-create", path},
		{"-create", path, "UniqueID", strconv.Itoa(a.UID)},
		{"-create", path, "PrimaryGroupID", strconv.Itoa(a.PrimaryGID)},
		{"-create", path, "UserShell", a.Shell},
		{"-create", path, "NFSHomeDirectory", a.Home},
		{"-create", path, "Password", a.Password},
		{"-create", path, "IsHidden", hidden},
	}
	for _, step := range steps {
		if _, err := run(dsclPath, append([]string{"."}, step...)...); err != nil {
			return err
		}
	}
	return nil
}

func (systemHost) DeleteUser(name string) error {
	_, err := run(dsclPath, ".", "-delete", "/Users/"+name)
	return err
}

func (systemHost) DeleteGroup(name string) error {
	_, err := run(dseditgroupPath, "-o", "delete", name)
	return err
}

// ReapUser kills by real and effective uid and boots out the uid's launchd
// domain, which would otherwise respawn a job, as slot-run's reap does.
func (systemHost) ReapUser(uid int) error {
	id := strconv.Itoa(uid)
	for range 50 {
		_, _ = run(pkillPath, "-9", "-U", id)
		_, _ = run(pkillPath, "-9", "-u", id)
		_, _ = run(launchctlPath, "bootout", "user/"+id)
		live, err := uidOwnsProcess(uid)
		if err != nil {
			return err
		}
		if !live {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("uid %d still owns a process after reaping", uid)
}

// ClearUserFlags runs chflags as the account. A file the account does not own
// keeps its flags, so chflags reporting failure is expected and ignored; the
// deletion after it fails loudly if a flag remains.
func (systemHost) ClearUserFlags(name, path string) error {
	_, err := run(sudoPath, "-n", "-u", name, chflagsPath, "-R", "-P", "nouchg,nouappnd", path)
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return nil
	}
	return err
}

func (systemHost) Stat(path string) (FileInfo, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return FileInfo{Kind: FileMissing}, nil
	}
	if err != nil {
		return FileInfo{}, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return FileInfo{}, fmt.Errorf("stat %s: no owner", path)
	}
	kind := FileOther
	switch {
	case info.Mode().IsDir():
		kind = FileDir
	case info.Mode().IsRegular():
		kind = FileRegular
	}
	return FileInfo{Kind: kind, UID: int(st.Uid), GID: int(st.Gid), Mode: info.Mode().Perm()}, nil
}

func (systemHost) ReadFile(path string) ([]byte, error) {
	return os.ReadFile(path) //nolint:gosec // G304: install's own fixed paths
}

func (systemHost) ReadDir(path string) ([]string, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return names, nil
}

func (systemHost) WriteFile(path string, data []byte, uid, gid int, mode fs.FileMode) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(f.Name())
		}
	}()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Chown(uid, gid); err != nil {
		return err
	}
	if err = f.Chmod(mode); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func (h systemHost) Mkdir(path string, uid, gid int, mode fs.FileMode) error {
	if err := os.Mkdir(path, 0o700); err != nil {
		return err
	}
	return h.SetOwnerMode(path, uid, gid, mode)
}

func (systemHost) SetOwnerMode(path string, uid, gid int, mode fs.FileMode) error {
	if err := os.Lchown(path, uid, gid); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

func (systemHost) RemoveAll(path string) error { return os.RemoveAll(path) }

func (systemHost) RemoveEmptyDir(path string) (bool, error) {
	err := os.Remove(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTEMPTY), errors.Is(err, syscall.EEXIST):
		return false, nil
	}
	return false, err
}

func (systemHost) CheckSudoers(content []byte) error {
	f, err := os.CreateTemp("", "freeside-sudoers-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	_, err = run(visudoPath, "-cf", f.Name())
	return err
}

func (systemHost) ServiceLoaded(label string) (bool, error) {
	_, err := run(launchctlPath, "print", "system/"+label)
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == launchctlNotFound {
		return false, nil
	}
	return err == nil, err
}

func (systemHost) BootstrapService(plistPath string) error {
	_, err := run(launchctlPath, "bootstrap", "system", plistPath)
	return err
}

func (systemHost) BootoutService(label string) error {
	_, err := run(launchctlPath, "bootout", "system/"+label)
	return err
}

func (systemHost) AnchorRules(anchor string) ([]string, error) {
	out, err := run(pfctlPath, "-a", anchor, "-s", "rules")
	if err != nil {
		return nil, err
	}
	var rules []string
	for line := range strings.Lines(string(out)) {
		if line = strings.TrimSpace(line); line != "" {
			rules = append(rules, line)
		}
	}
	return rules, nil
}

func (systemHost) LoadAnchor(anchor, path string) error {
	_, err := run(pfctlPath, "-a", anchor, "-f", path)
	return err
}

func (systemHost) FlushAnchor(anchor string) error {
	_, err := run(pfctlPath, "-a", anchor, "-F", "rules")
	return err
}

func (systemHost) PacketFilterEnabled() (bool, error) {
	out, err := run(pfctlPath, "-s", "info")
	if err != nil {
		return false, err
	}
	return bytes.HasPrefix(out, []byte("Status: Enabled")), nil
}

func (systemHost) EnablePacketFilter() error {
	_, err := run(pfctlPath, "-E")
	return err
}
