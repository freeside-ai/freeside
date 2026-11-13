package macverify

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

type fakeFile struct {
	info FileInfo
	data []byte
}

// fakeHost keeps the directory service, filesystem, launchd, and pf in
// memory and logs every change, so a test can assert a run changed nothing.
type fakeHost struct {
	root     bool
	users    map[string]Account
	groups   map[string]Group
	files    map[string]fakeFile
	services map[string]bool
	anchors  map[string][]string
	pf       bool
	// live is the uids that own a process; stuck uids survive a reap.
	live, stuck map[int]bool
	changes     []string

	// bootPolls makes the boot job's pfctl load asynchronous: it lands
	// only after that many reads of the job's state. Zero loads at
	// bootstrap, as before.
	bootPolls int
	// bootExit is the boot job's exit code; bootNever keeps it running.
	bootExit  int
	bootNever bool
	// pending is the boot job's load still in flight.
	pending *[2]string
	// runs counts each loaded job's runs.
	runs map[string]int
	// anchorLoads counts LoadAnchor calls; inFlightLoads those made while
	// the boot job's own load was pending.
	anchorLoads, inFlightLoads int
}

func newFakeHost() *fakeHost {
	h := &fakeHost{
		root: true, users: map[string]Account{}, groups: map[string]Group{}, files: map[string]fakeFile{},
		services: map[string]bool{}, anchors: map[string][]string{}, live: map[int]bool{}, stuck: map[int]bool{},
		runs: map[string]int{},
	}
	for _, dir := range []string{"/", "/Library", launchDaemonsDir, "/etc", sudoersDir} {
		h.files[dir] = fakeFile{info: FileInfo{Kind: FileDir, Mode: 0o755}}
	}
	h.users["benja"] = Account{Name: "benja", UID: 501, PrimaryGID: 20, Home: "/Users/benja", Shell: "/bin/zsh"}
	h.users["root"] = Account{Name: "root", UID: 0, PrimaryGID: 0, Home: "/var/root", Shell: "/bin/sh"}
	h.groups["staff"] = Group{Name: "staff", GID: 20}
	return h
}

func (h *fakeHost) change(format string, args ...any) {
	h.changes = append(h.changes, fmt.Sprintf(format, args...))
}

func (h *fakeHost) Privileged() bool { return h.root }

func (h *fakeHost) User(name string) (Account, bool, error) {
	a, ok := h.users[name]
	return a, ok, nil
}

func (h *fakeHost) UserNameByID(uid int) (string, bool, error) {
	for _, a := range h.sortedUsers() {
		if a.UID == uid {
			return a.Name, true, nil
		}
	}
	return "", false, nil
}

func (h *fakeHost) sortedUsers() []Account {
	var users []Account
	for _, a := range h.users {
		users = append(users, a)
	}
	slices.SortFunc(users, func(a, b Account) int { return strings.Compare(a.Name, b.Name) })
	return users
}

func (h *fakeHost) UsersWithPrimaryGroup(gid int) ([]string, error) {
	var names []string
	for _, a := range h.sortedUsers() {
		if a.PrimaryGID == gid {
			names = append(names, a.Name)
		}
	}
	return names, nil
}

func (h *fakeHost) Group(name string) (Group, bool, error) {
	g, ok := h.groups[name]
	return g, ok, nil
}

func (h *fakeHost) GroupNameByID(gid int) (string, bool, error) {
	for name, g := range h.groups {
		if g.GID == gid {
			return name, true, nil
		}
	}
	return "", false, nil
}

func (h *fakeHost) CreateGroup(name string, gid int) error {
	h.change("create group %s", name)
	h.groups[name] = Group{Name: name, GID: gid}
	return nil
}

func (h *fakeHost) WriteUser(a Account) error {
	h.change("write account %s", a.Name)
	h.users[a.Name] = a
	return nil
}

func (h *fakeHost) DeleteUser(name string) error {
	h.change("delete account %s", name)
	delete(h.users, name)
	return nil
}

func (h *fakeHost) DeleteGroup(name string) error {
	h.change("delete group %s", name)
	delete(h.groups, name)
	return nil
}

func (h *fakeHost) ReapUser(uid int) error {
	h.change("reap %d", uid)
	if h.stuck[uid] {
		return errors.New("still live")
	}
	delete(h.live, uid)
	return nil
}

func (h *fakeHost) ClearUserFlags(name, path string) error {
	h.change("clear flags %s as %s", path, name)
	return nil
}

func (h *fakeHost) Stat(path string) (FileInfo, error) {
	if f, ok := h.files[path]; ok {
		return f.info, nil
	}
	return FileInfo{Kind: FileMissing}, nil
}

func (h *fakeHost) ReadFile(path string) ([]byte, error) {
	f, ok := h.files[path]
	if !ok || f.info.Kind != FileRegular {
		return nil, fmt.Errorf("read %s: %w", path, fs.ErrNotExist)
	}
	if !h.root && f.info.Mode&0o004 == 0 {
		return nil, fmt.Errorf("read %s: %w", path, fs.ErrPermission)
	}
	return slices.Clone(f.data), nil
}

func (h *fakeHost) ReadDir(path string) ([]string, error) {
	var names []string
	for p := range h.files {
		if filepath.Dir(p) == path && p != path {
			names = append(names, filepath.Base(p))
		}
	}
	slices.Sort(names)
	return names, nil
}

func (h *fakeHost) requireParent(path string) error {
	if h.files[filepath.Dir(path)].info.Kind != FileDir {
		return fmt.Errorf("%s: parent is not a directory", path)
	}
	return nil
}

func (h *fakeHost) WriteFile(path string, data []byte, uid, gid int, mode fs.FileMode) error {
	h.change("write %s", path)
	if err := h.requireParent(path); err != nil {
		return err
	}
	h.files[path] = fakeFile{info: FileInfo{Kind: FileRegular, UID: uid, GID: gid, Mode: mode}, data: slices.Clone(data)}
	return nil
}

func (h *fakeHost) Mkdir(path string, uid, gid int, mode fs.FileMode) error {
	h.change("mkdir %s", path)
	if err := h.requireParent(path); err != nil {
		return err
	}
	if _, ok := h.files[path]; ok {
		return fmt.Errorf("mkdir %s: %w", path, fs.ErrExist)
	}
	h.files[path] = fakeFile{info: FileInfo{Kind: FileDir, UID: uid, GID: gid, Mode: mode}}
	return nil
}

func (h *fakeHost) SetOwnerMode(path string, uid, gid int, mode fs.FileMode) error {
	h.change("chown/chmod %s", path)
	f := h.files[path]
	f.info.UID, f.info.GID, f.info.Mode = uid, gid, mode
	h.files[path] = f
	return nil
}

func (h *fakeHost) RemoveAll(path string) error {
	h.change("remove %s", path)
	for p := range h.files {
		if p == path || strings.HasPrefix(p, path+"/") {
			delete(h.files, p)
		}
	}
	return nil
}

func (h *fakeHost) RemoveEmptyDir(path string) (bool, error) {
	for p := range h.files {
		if strings.HasPrefix(p, path+"/") {
			return false, nil
		}
	}
	if _, ok := h.files[path]; !ok {
		return false, nil
	}
	h.change("rmdir %s", path)
	delete(h.files, path)
	return true, nil
}

func (h *fakeHost) CheckSudoers(content []byte) error {
	if !strings.HasSuffix(string(content), "\n") {
		return errors.New("visudo: missing newline")
	}
	return nil
}

func (h *fakeHost) ServiceLoaded(label string) (bool, error) { return h.services[label], nil }

var plistString = regexp.MustCompile(`<string>([^<]*)</string>`)

// BootstrapService loads the job and, as RunAtLoad does, runs its pfctl.
func (h *fakeHost) BootstrapService(plistPath string) error {
	h.change("bootstrap %s", plistPath)
	f, ok := h.files[plistPath]
	if !ok {
		return errors.New("no plist")
	}
	var args []string
	for _, m := range plistString.FindAllStringSubmatch(string(f.data), -1) {
		args = append(args, m[1])
	}
	h.services[args[0]] = true
	h.runs[args[0]] = 1
	if i := slices.Index(args, "-a"); i > 0 && args[i+2] == "-f" {
		if h.bootPolls > 0 || h.bootNever {
			h.pending = &[2]string{args[i+1], args[i+3]}
			return nil
		}
		h.pf = true
		return h.loadAnchor(args[i+1], args[i+3])
	}
	return nil
}

// ServiceRunState reports the job running until its pending load lands.
func (h *fakeHost) ServiceRunState(label string) (ServiceRun, error) {
	if !h.services[label] {
		return ServiceRun{}, errors.New("launchctl print: service not found")
	}
	if h.pending != nil {
		if h.bootNever || h.bootPolls > 0 {
			h.bootPolls--
			return ServiceRun{Running: true, Runs: h.runs[label]}, nil
		}
		h.pf = true
		if err := h.loadAnchor(h.pending[0], h.pending[1]); err != nil {
			return ServiceRun{}, err
		}
		h.pending = nil
	}
	return ServiceRun{Runs: h.runs[label], LastExitCode: h.bootExit}, nil
}

func (h *fakeHost) BootoutService(label string) error {
	h.change("bootout %s", label)
	delete(h.services, label)
	return nil
}

func (h *fakeHost) AnchorRules(anchor string) ([]string, error) { return h.anchors[anchor], nil }

// LoadAnchor resolves each rule's user to a uid, as pf does, and prints it
// as pfctl -s rules would.
func (h *fakeHost) LoadAnchor(anchor, path string) error {
	h.anchorLoads++
	if h.pending != nil {
		h.inFlightLoads++
	}
	return h.loadAnchor(anchor, path)
}

func (h *fakeHost) loadAnchor(anchor, path string) error {
	h.change("load anchor %s", anchor)
	f, ok := h.files[path]
	if !ok {
		return errors.New("no anchor file")
	}
	var rules []string
	for line := range strings.Lines(string(f.data)) {
		fields := strings.Fields(line)
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		a, ok := h.users[fields[len(fields)-1]]
		if !ok {
			return fmt.Errorf("unknown user %s", fields[len(fields)-1])
		}
		// pfctl prints "<rule> user <name>" as "<rule> all user = <uid>".
		rules = append(rules, strings.Join(fields[:len(fields)-2], " ")+" all user = "+strconv.Itoa(a.UID))
	}
	h.anchors[anchor] = rules
	return nil
}

func (h *fakeHost) FlushAnchor(anchor string) error {
	h.change("flush anchor %s", anchor)
	delete(h.anchors, anchor)
	return nil
}

func (h *fakeHost) PacketFilterEnabled() (bool, error) { return h.pf, nil }

func (h *fakeHost) EnablePacketFilter() error {
	h.change("enable pf")
	h.pf = true
	return nil
}
