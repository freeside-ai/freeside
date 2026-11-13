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
	"time"
)

// The executor's host state (#2878) is installed, checked, and removed here,
// not in cmd/freesided, because every create, adopt, and delete decision is
// part of this Tier A package. A Host performs the side effects.
//
// Ownership rules:
//
//   - An account is a slot install may adopt, repair, or delete only when its
//     primary group id is the spec's and its home is <root>/homes/<name>. That
//     adopts the hand setup daemon/README.md once described and refuses a real
//     user who merely shares a name.
//   - The slot group is install's only when it has the spec's id and every
//     member, and every account with it as primary group, is a slot.
//   - The sudoers drop-in and LaunchDaemon are removed only when their bytes
//     are exactly what install writes; the executor root only when it holds a
//     slot-run helper.
//   - The Swift cache under the root is the operator's: install creates it
//     empty and takes over only one root owns, and removal deletes it, entries
//     and all, with the rest of the root.

// ErrInvalidSpec refuses an install spec the helper could not act on.
var ErrInvalidSpec = errors.New("macverify: invalid install spec")

// ErrInstallRefused refuses an install or removal before it changes anything.
var ErrInstallRefused = errors.New("macverify: install refused")

const (
	sudoersDir       = "/etc/sudoers.d"
	launchDaemonsDir = "/Library/LaunchDaemons"
	// anchorParent is the stock ruleset's anchor point: /etc/pf.conf declares
	// anchor "com.apple/*", so pf evaluates every child loaded under it.
	anchorParent = "com.apple"
	slotShell    = "/usr/bin/false"
	slotPassword = "*"
	maxSlots     = 64
)

var (
	// accountNamePattern is slot-run's slot-name rule.
	accountNamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	operatorPattern    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,63}$`)
	// rootPattern is slot-run's executor-root character rule.
	rootPattern   = regexp.MustCompile(`^[A-Za-z0-9/._-]+$`)
	anchorPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	labelPattern  = regexp.MustCompile(`^[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)*$`)
	// sudoersNamePattern has no dot: sudo skips an includedir file whose name
	// contains one.
	sudoersNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// InstallSpec names the executor's host state. Slots are <SlotPrefix>1 to
// <SlotPrefix><SlotCount>; the group's id is FirstID and slot n's uid is
// FirstID+n.
type InstallSpec struct {
	Root       string
	SlotPrefix string
	SlotCount  int
	Group      string
	FirstID    int
	// Operator is the user the daemon runs as, granted the helper in sudoers.
	Operator string
	// Anchor is the pf anchor's name under com.apple/.
	Anchor string
	// Label is the boot LaunchDaemon's label.
	Label string
	// SudoersName is the drop-in's file name under /etc/sudoers.d.
	SudoersName string
}

func (s InstallSpec) validate(requireOperator bool) error {
	if !filepath.IsAbs(s.Root) || filepath.Clean(s.Root) != s.Root || s.Root == "/" ||
		!rootPattern.MatchString(s.Root) {
		return fmt.Errorf("%w: root %q must be a clean absolute path other than / using [A-Za-z0-9/._-]",
			ErrInvalidSpec, s.Root)
	}
	if s.SlotCount < 1 || s.SlotCount > maxSlots {
		return fmt.Errorf("%w: slot count %d must be 1 to %d", ErrInvalidSpec, s.SlotCount, maxSlots)
	}
	for _, name := range append(s.Slots(), s.Group) {
		if !accountNamePattern.MatchString(name) {
			return fmt.Errorf("%w: account or group name %q must match %s", ErrInvalidSpec, name, accountNamePattern)
		}
	}
	if slices.Contains(s.Slots(), s.Group) {
		return fmt.Errorf("%w: group %q is also a slot name", ErrInvalidSpec, s.Group)
	}
	if s.FirstID < 1 || s.FirstID+s.SlotCount > 1<<31-1 {
		return fmt.Errorf("%w: first id %d out of range", ErrInvalidSpec, s.FirstID)
	}
	if requireOperator {
		if !operatorPattern.MatchString(s.Operator) {
			return fmt.Errorf("%w: operator %q must match %s", ErrInvalidSpec, s.Operator, operatorPattern)
		}
		if s.Operator == "root" || slices.Contains(s.Slots(), s.Operator) {
			return fmt.Errorf("%w: operator %q may not be root or a slot", ErrInvalidSpec, s.Operator)
		}
	}
	if !anchorPattern.MatchString(s.Anchor) {
		return fmt.Errorf("%w: anchor %q must match %s", ErrInvalidSpec, s.Anchor, anchorPattern)
	}
	if !labelPattern.MatchString(s.Label) {
		return fmt.Errorf("%w: label %q must match %s", ErrInvalidSpec, s.Label, labelPattern)
	}
	if !sudoersNamePattern.MatchString(s.SudoersName) {
		return fmt.Errorf("%w: sudoers name %q must match %s", ErrInvalidSpec, s.SudoersName, sudoersNamePattern)
	}
	return nil
}

// Slots lists the slot account names in id order.
func (s InstallSpec) Slots() []string {
	slots := make([]string, s.SlotCount)
	for i := range slots {
		slots[i] = s.SlotPrefix + strconv.Itoa(i+1)
	}
	return slots
}

// Config returns the macverify.Config fields this install determines:
// ExecutorRoot, Slots (sorted), and PacketFilterRules.
func (s InstallSpec) Config() Config {
	slots := s.Slots()
	slices.Sort(slots)
	return Config{ExecutorRoot: s.Root, Slots: slots, PacketFilterRules: PacketFilterRules(slots)}
}

func (s InstallSpec) path(elem ...string) string {
	return filepath.Join(append([]string{s.Root}, elem...)...)
}

func (s InstallSpec) slotsConfPath() string  { return s.path("bin", "slots.conf") }
func (s InstallSpec) anchorFilePath() string { return s.path("pf", "anchor.conf") }
func (s InstallSpec) anchorPath() string     { return anchorParent + "/" + s.Anchor }
func (s InstallSpec) sudoersPath() string    { return filepath.Join(sudoersDir, s.SudoersName) }
func (s InstallSpec) plistPath() string      { return filepath.Join(launchDaemonsDir, s.Label+".plist") }

func (s InstallSpec) homePath(slot string) string {
	return s.path("homes", slot)
}

// rootEntries are the names install creates directly under the root, in
// removal order. The caches go first: the operator can make them fail to
// delete (an immutable file), and the helper in bin is what lets a rerun
// removal recognize the root as install's.
var rootEntries = []string{swiftCacheDirName, goCacheDirName, "bin", "jobs", "homes", "template", "pf"}

func (s InstallSpec) treeDirs() []string {
	return []string{
		s.Root, s.path("bin"), s.path("jobs"), s.path("homes"),
		s.path("template"), s.path("template", "home"), s.path("pf"),
	}
}

// treeEntries names what each tree directory may hold. Install refuses a
// directory holding anything else, so it never adopts an existing tree (a
// mistyped -root such as /usr/local) that removal would later delete.
func (s InstallSpec) treeEntries(dir string) []string {
	switch dir {
	case s.Root:
		return rootEntries
	case s.path("bin"):
		return []string{"slot-run", "slots.conf"}
	case s.path("jobs"), s.path("homes"):
		return s.Slots()
	case s.path("template"):
		return []string{"home"}
	case s.path("pf"):
		return []string{"anchor.conf"}
	}
	return nil
}

func (s InstallSpec) bootPlist() []byte {
	return renderBootPlist(s.Label, s.anchorPath(), s.anchorFilePath())
}

// isSlotShaped is the adoption rule: an account install may adopt, repair,
// or delete as a slot.
func (s InstallSpec) isSlotShaped(a Account) bool {
	return a.UID > 0 && a.PrimaryGID == s.FirstID && a.Home == s.homePath(a.Name)
}

// State is what check found for one piece.
type State string

const (
	StatePresent       State = "present"
	StateMissing       State = "missing"
	StateMisconfigured State = "misconfigured"
	// StateForeign is a name, id, or path held by something install did not
	// create. Install refuses while any piece is foreign.
	StateForeign State = "foreign"
	// StateUnread is a piece this process lacks the rights to read.
	StateUnread State = "unread"
)

// Finding is one piece's state, with the reason when it is not present.
type Finding struct {
	Piece  string
	State  State
	Detail string
}

// Healthy reports whether the piece needs no repair.
func (f Finding) Healthy() bool { return f.State == StatePresent || f.State == StateUnread }

type piece struct {
	name    string
	inspect func() (State, string, error)
	repair  func() error
}

type installer struct {
	spec InstallSpec
	host Host
}

func (in installer) pieces() []piece {
	s := in.spec
	// The tree, helper, and slot list come before the accounts, so an
	// install that stops partway leaves a slots.conf naming every account it
	// may have created, and removal can find them.
	ps := []piece{in.ancestorsPiece()}
	for _, dir := range s.treeDirs() {
		ps = append(ps, in.dirPiece(dir))
	}
	ps = append(ps,
		in.operatorCachePiece("Swift cache", swiftCacheDirName),
		in.operatorCachePiece("Go cache", goCacheDirName),
		in.filePiece("helper", helperPath(s.Root), helperScript, 0o755, nil),
		in.filePiece("slot list", s.slotsConfPath(), renderSlotsConf(s.Slots()), 0o644, nil),
		in.groupPiece(),
	)
	for i, slot := range s.Slots() {
		ps = append(ps, in.accountPiece(slot, s.FirstID+i+1))
	}
	for i, slot := range s.Slots() {
		ps = append(ps, in.homePiece(slot, s.FirstID+i+1))
	}
	ps = append(ps,
		in.sudoersPiece(),
		in.filePiece("anchor file", s.anchorFilePath(), PacketFilterRules(s.Slots()), 0o644, nil),
		in.filePiece("boot LaunchDaemon", s.plistPath(), s.bootPlist(), 0o644, in.bootPlistOwned),
		in.servicePiece(),
		in.anchorRulesPiece(),
		in.packetFilterPiece(),
	)
	return ps
}

// ancestorsPiece holds every directory above the root to the helper's
// standard: root-owned and not group- or world-writable. Install creates a
// missing ancestor but never changes an existing one.
func (in installer) ancestorsPiece() piece {
	var ancestors []string
	for dir := filepath.Dir(in.spec.Root); ; dir = filepath.Dir(dir) {
		ancestors = append(ancestors, dir)
		if dir == "/" {
			break
		}
	}
	slices.Reverse(ancestors)
	var missing []string
	return piece{
		name: "ancestors of " + in.spec.Root,
		inspect: func() (State, string, error) {
			missing = nil
			for _, dir := range ancestors {
				info, err := in.host.Stat(dir)
				if err != nil {
					return "", "", err
				}
				switch {
				case info.Kind == FileMissing:
					missing = append(missing, dir)
				case info.Kind != FileDir:
					return StateForeign, dir + " is not a directory", nil
				case info.UID != 0:
					return StateForeign, dir + " is not owned by root", nil
				case info.Mode&0o022 != 0:
					return StateForeign, fmt.Sprintf("%s is writable by group or others (%v)", dir, info.Mode), nil
				}
			}
			if len(missing) > 0 {
				return StateMissing, strings.Join(missing, ", "), nil
			}
			return StatePresent, "", nil
		},
		repair: func() error {
			for _, dir := range missing {
				if err := in.host.Mkdir(dir, 0, 0, 0o755); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func (in installer) groupPiece() piece {
	s := in.spec
	return piece{
		name: "group " + s.Group,
		inspect: func() (State, string, error) {
			primaries, err := in.host.UsersWithPrimaryGroup(s.FirstID)
			if err != nil {
				return "", "", err
			}
			for _, name := range primaries {
				if !slices.Contains(s.Slots(), name) {
					return StateForeign, fmt.Sprintf("account %s has gid %d as its primary group but is not a slot",
						name, s.FirstID), nil
				}
			}
			g, ok, err := in.host.Group(s.Group)
			if err != nil {
				return "", "", err
			}
			if !ok {
				holder, held, err := in.host.GroupNameByID(s.FirstID)
				if err != nil {
					return "", "", err
				}
				if held {
					return StateForeign, fmt.Sprintf("gid %d belongs to group %s", s.FirstID, holder), nil
				}
				return StateMissing, "", nil
			}
			if g.GID != s.FirstID {
				return StateForeign, fmt.Sprintf("group %s has gid %d, want %d", s.Group, g.GID, s.FirstID), nil
			}
			for _, member := range g.Members {
				if !slices.Contains(s.Slots(), member) {
					return StateForeign, fmt.Sprintf("member %s is not a slot", member), nil
				}
			}
			return StatePresent, "", nil
		},
		repair: func() error { return in.host.CreateGroup(s.Group, s.FirstID) },
	}
}

func (in installer) accountPiece(slot string, uid int) piece {
	s := in.spec
	want := Account{
		Name: slot, UID: uid, PrimaryGID: s.FirstID, Home: s.homePath(slot),
		Shell: slotShell, Password: slotPassword, Hidden: true,
	}
	return piece{
		name: "account " + slot,
		inspect: func() (State, string, error) {
			a, ok, err := in.host.User(slot)
			if err != nil {
				return "", "", err
			}
			if !ok {
				holder, held, err := in.host.UserNameByID(uid)
				if err != nil {
					return "", "", err
				}
				if held {
					return StateForeign, fmt.Sprintf("uid %d belongs to account %s", uid, holder), nil
				}
				return StateMissing, "", nil
			}
			if isIncomplete(a, want) {
				return StateMisconfigured, "an incomplete record, as a stopped install leaves", nil
			}
			if !s.isSlotShaped(a) {
				return StateForeign, fmt.Sprintf("existing account has primary gid %d and home %q, not a slot's",
					a.PrimaryGID, a.Home), nil
			}
			if a.UID != uid {
				return StateForeign, fmt.Sprintf("slot account has uid %d, want %d; install does not renumber an account",
					a.UID, uid), nil
			}
			admin, ok, err := in.host.Group("admin")
			if err != nil {
				return "", "", err
			}
			if ok && slices.Contains(admin.Members, slot) {
				return StateForeign, "slot account is an administrator", nil
			}
			var wrong []string
			if a.Shell != want.Shell {
				wrong = append(wrong, fmt.Sprintf("shell %q, want %q", a.Shell, want.Shell))
			}
			if a.Password != want.Password {
				wrong = append(wrong, "password is not "+want.Password)
			}
			if !a.Hidden {
				wrong = append(wrong, "not hidden")
			}
			if len(wrong) > 0 {
				return StateMisconfigured, strings.Join(wrong, "; "), nil
			}
			return StatePresent, "", nil
		},
		repair: func() error { return in.host.WriteUser(want) },
	}
}

// isIncomplete reports a record that dscl stopped writing partway: each of
// its uid, primary gid, and home is unset or install's, and one is unset. A
// real account has all three, so this adopts only what install began.
func isIncomplete(a, want Account) bool {
	uid, gid, home := a.UID == unsetID, a.PrimaryGID == unsetID, a.Home == ""
	return (uid || a.UID == want.UID) && (gid || a.PrimaryGID == want.PrimaryGID) &&
		(home || a.Home == want.Home) && (uid || gid || home)
}

// dirPiece holds a tree directory root-owned at 0755 holding only what
// install puts there. A directory some other user owns is foreign: install
// takes over no one's directory.
func (in installer) dirPiece(dir string) piece {
	p := in.ownedDirPiece("directory "+dir, dir, 0, 0o755, true)
	inspect := p.inspect
	p.inspect = func() (State, string, error) {
		state, detail, err := inspect()
		if err != nil || state == StateMissing || state == StateForeign {
			return state, detail, err
		}
		info, err := in.host.Stat(dir)
		if err != nil {
			return "", "", err
		}
		if info.UID != 0 {
			return StateForeign, fmt.Sprintf("owned by uid %d, not root", info.UID), nil
		}
		entries, err := in.host.ReadDir(dir)
		if err != nil {
			return "", "", err
		}
		allowed := in.spec.treeEntries(dir)
		for _, entry := range entries {
			if !slices.Contains(allowed, entry) && !isTempEntry(entry, allowed) {
				return StateForeign, fmt.Sprintf("holds %s, which install does not write", entry), nil
			}
		}
		return state, detail, nil
	}
	return p
}

// isTempEntry matches the temporary file an interrupted WriteFile leaves
// beside one of the allowed names.
func isTempEntry(entry string, allowed []string) bool {
	for _, name := range allowed {
		if strings.HasPrefix(entry, "."+name+".tmp-") {
			return true
		}
	}
	return false
}

// operatorCachePiece holds a cache directory under the root (swift-cache or
// go-cache) to what populate fills as the operator and slot-run reads from:
// a directory the operator owns at 0755. The caches are the tree directories
// root does not own. A root-owned one, as an interrupted Mkdir leaves, is
// install's to repair; one another user owns is foreign, so install never
// hands that user's entries to the operator.
func (in installer) operatorCachePiece(name, dirName string) piece {
	s := in.spec
	dir := s.path(dirName)
	var exists bool
	var uid int
	return piece{
		name: name + " " + dir,
		inspect: func() (State, string, error) {
			operator, ok, err := in.host.User(s.Operator)
			if err != nil {
				return "", "", err
			}
			if !ok || operator.UID == 0 {
				return StateMisconfigured, fmt.Sprintf("operator %s is unknown or root", s.Operator), nil
			}
			uid = operator.UID
			info, err := in.host.Stat(dir)
			if err != nil {
				return "", "", err
			}
			exists = info.Kind != FileMissing
			switch {
			case !exists:
				return StateMissing, "", nil
			case info.Kind != FileDir:
				return StateForeign, "not a directory", nil
			case info.UID != uid && info.UID != 0:
				return StateForeign, fmt.Sprintf("owned by uid %d, not operator %s (uid %d); delete it for install to recreate",
					info.UID, s.Operator, uid), nil
			case info.UID != uid || info.Mode != 0o755:
				return StateMisconfigured, fmt.Sprintf("owner %d mode %v, want %d %v",
					info.UID, info.Mode, uid, fs.FileMode(0o755)), nil
			}
			return StatePresent, "", nil
		},
		repair: func() error {
			if exists {
				return in.host.SetOwnerMode(dir, uid, 0, 0o755)
			}
			return in.host.Mkdir(dir, uid, 0, 0o755)
		},
	}
}

// homePiece holds a slot's home to what slot-run's reset leaves: the slot's,
// at 0700. Its group is whatever the template copy gave it, so only the owner
// and mode are checked; its contents are reset before every job.
func (in installer) homePiece(slot string, uid int) piece {
	return in.ownedDirPiece("home "+slot, in.spec.homePath(slot), uid, 0o700, false)
}

func (in installer) ownedDirPiece(name, dir string, uid int, mode fs.FileMode, checkGroup bool) piece {
	var exists bool
	return piece{
		name: name,
		inspect: func() (State, string, error) {
			info, err := in.host.Stat(dir)
			if err != nil {
				return "", "", err
			}
			exists = info.Kind != FileMissing
			switch {
			case !exists:
				return StateMissing, "", nil
			case info.Kind != FileDir:
				return StateForeign, "not a directory", nil
			case info.UID != uid || (checkGroup && info.GID != 0) || info.Mode != mode:
				return StateMisconfigured, fmt.Sprintf("owner %d:%d mode %v, want %d:0 %v",
					info.UID, info.GID, info.Mode, uid, mode), nil
			}
			return StatePresent, "", nil
		},
		repair: func() error {
			if exists {
				return in.host.SetOwnerMode(dir, uid, 0, mode)
			}
			return in.host.Mkdir(dir, uid, 0, mode)
		},
	}
}

// filePiece holds a root-owned file to exact bytes and mode. owned, when
// set, says whether differing content is install's to overwrite; without it,
// any regular file at the path is.
func (in installer) filePiece(name, path string, want []byte, mode fs.FileMode, owned func([]byte) bool) piece {
	return piece{
		name: name + " " + path,
		inspect: func() (State, string, error) {
			return in.inspectFile(path, mode, func(got []byte) (State, string) {
				if string(got) == string(want) {
					return StatePresent, ""
				}
				if owned != nil && !owned(got) {
					return StateForeign, "not written by install"
				}
				return StateMisconfigured, "content differs from what this freesided writes"
			})
		},
		repair: func() error {
			if path == in.spec.plistPath() {
				// A loaded job keeps its old definition until booted out.
				if err := in.bootout(); err != nil {
					return err
				}
			}
			return in.host.WriteFile(path, want, 0, 0, mode)
		},
	}
}

func (in installer) bootPlistOwned(content []byte) bool {
	return bootPlistShaped(content, in.spec.Label)
}

// inspectFile reads a root-owned regular file and judges its content.
func (in installer) inspectFile(path string, mode fs.FileMode, judge func([]byte) (State, string)) (State, string, error) {
	info, err := in.host.Stat(path)
	if err != nil {
		return "", "", err
	}
	switch {
	case info.Kind == FileMissing:
		return StateMissing, "", nil
	case info.Kind != FileRegular:
		return StateForeign, "not a regular file", nil
	}
	got, err := in.host.ReadFile(path)
	if errors.Is(err, fs.ErrPermission) {
		return StateUnread, "content needs root to read", nil
	}
	if err != nil {
		return "", "", err
	}
	if state, detail := judge(got); state != StatePresent {
		return state, detail, nil
	}
	if info.UID != 0 || info.GID != 0 || info.Mode != mode {
		return StateMisconfigured, fmt.Sprintf("owner %d:%d mode %v, want 0:0 %v", info.UID, info.GID, info.Mode, mode), nil
	}
	return StatePresent, "", nil
}

func (in installer) sudoersPiece() piece {
	s := in.spec
	want := renderSudoers(s.Operator, s.Root)
	return piece{
		name: "sudoers file " + s.sudoersPath(),
		inspect: func() (State, string, error) {
			return in.inspectFile(s.sudoersPath(), 0o440, func(got []byte) (State, string) {
				operator, owned := sudoersOperator(got, s.Root)
				switch {
				case !owned:
					return StateForeign, "not written by install"
				case operator != s.Operator:
					return StateMisconfigured, fmt.Sprintf("grants %s, want %s", operator, s.Operator)
				case string(got) != string(want):
					return StateMisconfigured, "the hand setup's rule, without install's header"
				}
				return StatePresent, ""
			})
		},
		repair: func() error {
			if err := in.host.CheckSudoers(want); err != nil {
				return fmt.Errorf("check sudoers drop-in: %w", err)
			}
			return in.host.WriteFile(s.sudoersPath(), want, 0, 0, 0o440)
		},
	}
}

func (in installer) bootout() error {
	loaded, err := in.host.ServiceLoaded(in.spec.Label)
	if err != nil || !loaded {
		return err
	}
	return in.host.BootoutService(in.spec.Label)
}

func (in installer) servicePiece() piece {
	s := in.spec
	return piece{
		name: "boot job " + s.Label,
		inspect: func() (State, string, error) {
			loaded, err := in.host.ServiceLoaded(s.Label)
			if err != nil || loaded {
				return StatePresent, "", err
			}
			return StateMissing, "not loaded in launchd", nil
		},
		repair: func() error {
			if err := in.host.BootstrapService(s.plistPath()); err != nil {
				return err
			}
			return in.awaitBootJob()
		},
	}
}

// bootJobTimeout bounds the wait for the boot job's first run; its one
// pfctl call takes well under a second.
const (
	bootJobTimeout = 30 * time.Second
	bootJobPoll    = 100 * time.Millisecond
)

// awaitBootJob waits for the job bootstrapping just started to finish its
// first run. RunAtLoad runs its pfctl -f on the anchor, and a second load
// while that one is in flight fails with "Resource busy" (#3009), so the
// anchor piece, which comes next, must see the job's result rather than
// load the anchor itself. A run that exits nonzero fails install: the job
// is what loads the rules at every boot.
func (in installer) awaitBootJob() error {
	deadline := time.Now().Add(bootJobTimeout)
	for {
		state, err := in.host.ServiceRunState(in.spec.Label)
		if err != nil {
			return fmt.Errorf("read the boot job's state: %w", err)
		}
		if !state.Running && state.Runs > 0 {
			if state.LastExitCode != 0 {
				return fmt.Errorf("boot job %s exited %d loading the anchor", in.spec.Label, state.LastExitCode)
			}
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("boot job %s did not finish its first run within %v (running %t, runs %d)",
				in.spec.Label, bootJobTimeout, state.Running, state.Runs)
		}
		time.Sleep(bootJobPoll)
	}
}

// anchorRulesPiece compares the loaded anchor with the slot set. pfctl prints
// a rule's user by uid, so each slot must have one in and one out rule naming
// its uid or its name, and nothing else may be loaded there.
func (in installer) anchorRulesPiece() piece {
	s := in.spec
	return piece{
		name: "loaded rules " + s.anchorPath(),
		inspect: func() (State, string, error) {
			if !in.host.Privileged() {
				return StateUnread, "reading pf needs root", nil
			}
			rules, err := in.host.AnchorRules(s.anchorPath())
			if err != nil {
				return "", "", err
			}
			if len(rules) == 0 {
				return StateMissing, "no rules loaded", nil
			}
			want := make(map[string]bool)
			uids := make(map[string]string)
			for i, slot := range s.Slots() {
				uids[slot] = strconv.Itoa(s.FirstID + i + 1)
				for _, dir := range []string{"in", "out"} {
					want[dir+" "+uids[slot]] = true
				}
			}
			seen := make(map[string]bool)
			for _, rule := range rules {
				dir, who, ok := anchorRuleKey(rule)
				if uid, named := uids[who]; named {
					who = uid
				}
				key := dir + " " + who
				if !ok || !want[key] {
					return StateMisconfigured, fmt.Sprintf("unexpected rule %q", rule), nil
				}
				seen[key] = true
			}
			if len(rules) != 2*s.SlotCount || len(seen) != 2*s.SlotCount {
				return StateMisconfigured, fmt.Sprintf("%d rules loaded, want %d", len(rules), 2*s.SlotCount), nil
			}
			return StatePresent, "", nil
		},
		repair: func() error { return in.host.LoadAnchor(s.anchorPath(), s.anchorFilePath()) },
	}
}

// anchorRuleKey reads "block drop <dir> quick ... user = <who>".
func anchorRuleKey(rule string) (dir, who string, ok bool) {
	fields := strings.Fields(rule)
	if len(fields) < 7 || fields[0] != "block" || fields[1] != "drop" || fields[3] != "quick" ||
		fields[len(fields)-3] != "user" || fields[len(fields)-2] != "=" {
		return "", "", false
	}
	return fields[2], fields[len(fields)-1], true
}

func (in installer) packetFilterPiece() piece {
	return piece{
		name: "packet filter",
		inspect: func() (State, string, error) {
			if !in.host.Privileged() {
				return StateUnread, "reading pf needs root", nil
			}
			enabled, err := in.host.PacketFilterEnabled()
			if err != nil || enabled {
				return StatePresent, "", err
			}
			return StateMissing, "pf is disabled", nil
		},
		repair: in.host.EnablePacketFilter,
	}
}

func inspectPieces(pieces []piece) ([]Finding, error) {
	findings := make([]Finding, 0, len(pieces))
	for _, p := range pieces {
		state, detail, err := p.inspect()
		if err != nil {
			return findings, fmt.Errorf("inspect %s: %w", p.name, err)
		}
		findings = append(findings, Finding{Piece: p.name, State: state, Detail: detail})
	}
	return findings, nil
}

// Inspect reports every piece of the install as present, missing,
// misconfigured, foreign, or unread. Without root it cannot read pf or the
// sudoers drop-in, and says so.
func Inspect(spec InstallSpec, host Host) ([]Finding, error) {
	if err := spec.validate(true); err != nil {
		return nil, err
	}
	return inspectPieces(installer{spec: spec, host: host}.pieces())
}

// Install creates or completes the executor's host state and returns the
// findings it verified afterwards. It refuses, changing nothing, unless it
// runs as root for a known operator other than root, and while any piece is
// foreign: a name or id held by something install did not create, or an
// ancestor of the root that is not root-owned or is writable by group or
// others. It repairs only what it owns: missing pieces, modes and owners in
// the root tree, files whose bytes differ, and the attributes of an account
// the adoption rule marks as a slot.
func Install(spec InstallSpec, host Host) ([]Finding, error) {
	if err := spec.validate(true); err != nil {
		return nil, err
	}
	if !host.Privileged() {
		return nil, fmt.Errorf("%w: install needs root (run it with sudo)", ErrInstallRefused)
	}
	operator, ok, err := host.User(spec.Operator)
	if err != nil {
		return nil, fmt.Errorf("look up operator %s: %w", spec.Operator, err)
	}
	if !ok || operator.UID == 0 {
		return nil, fmt.Errorf("%w: operator %s is unknown or root", ErrInstallRefused, spec.Operator)
	}
	pieces := installer{spec: spec, host: host}.pieces()
	findings, err := inspectPieces(pieces)
	if err != nil {
		return findings, err
	}
	var foreign []string
	for _, f := range findings {
		if f.State == StateForeign {
			foreign = append(foreign, f.Piece+": "+f.Detail)
		}
	}
	if len(foreign) > 0 {
		return findings, fmt.Errorf("%w: %s", ErrInstallRefused, strings.Join(foreign, "; "))
	}
	// Each piece is inspected again just before its repair, since an earlier
	// repair can satisfy it: bootstrapping the job loads the anchor, and the
	// boot job's repair waits for that load so the anchor piece finds the
	// rules instead of racing the job with a load of its own.
	for _, p := range pieces {
		state, _, err := p.inspect()
		if err != nil {
			return nil, fmt.Errorf("inspect %s: %w", p.name, err)
		}
		if state == StateForeign {
			return nil, fmt.Errorf("%w: %s became foreign during install", ErrInstallRefused, p.name)
		}
		if state == StateMissing || state == StateMisconfigured {
			if err := p.repair(); err != nil {
				return nil, fmt.Errorf("install %s: %w", p.name, err)
			}
		}
	}
	findings, err = inspectPieces(pieces)
	if err != nil {
		return findings, err
	}
	for _, f := range findings {
		if !f.Healthy() {
			return findings, fmt.Errorf("install left %s %s: %s", f.Piece, f.State, f.Detail)
		}
	}
	return findings, nil
}

// RemoveReport lists what removal deleted and what it left, with why.
type RemoveReport struct {
	Removed []string
	Left    []string
}

// Remove deletes what install created and nothing else: the sudoers drop-in
// and boot LaunchDaemon only when their bytes are exactly install's, each
// account listed in the root's slots.conf that the adoption rule marks as a
// slot, the group once nothing names it, and the executor root, Swift cache
// included, only when it holds a slot-run helper. Anything else is reported in Left. An absent
// install is a no-op. The sudoers rule goes first, so the daemon starts no
// job while removal runs.
func Remove(spec InstallSpec, host Host) (RemoveReport, error) {
	var r RemoveReport
	if err := spec.validate(false); err != nil {
		return r, err
	}
	if !host.Privileged() {
		return r, fmt.Errorf("%w: remove needs root (run it with sudo)", ErrInstallRefused)
	}
	in := installer{spec: spec, host: host}
	left := func(format string, args ...any) { r.Left = append(r.Left, fmt.Sprintf(format, args...)) }

	sudoers, present, err := in.readIfRegular(spec.sudoersPath())
	if err != nil {
		return r, err
	}
	if _, owned := sudoersOperator(sudoers, spec.Root); owned {
		if err := in.removePath(&r, spec.sudoersPath()); err != nil {
			return r, err
		}
	} else if present {
		left("%s: not what install writes", spec.sudoersPath())
	}

	plist, present, err := in.readIfRegular(spec.plistPath())
	if err != nil {
		return r, err
	}
	plistOwned := present && string(plist) == string(spec.bootPlist())
	if present && !plistOwned {
		left("%s: not what install writes", spec.plistPath())
	}
	if plistOwned {
		if err := in.bootout(); err != nil {
			return r, fmt.Errorf("boot out %s: %w", spec.Label, err)
		}
		if err := in.removePath(&r, spec.plistPath()); err != nil {
			return r, err
		}
	}

	// The root is deleted only when it is a root-owned directory below
	// ancestors install would accept, so no one but root can swap a path
	// under the deletion.
	ancestors, _, err := in.ancestorsPiece().inspect()
	if err != nil {
		return r, err
	}
	rootInfo, err := host.Stat(spec.Root)
	if err != nil {
		return r, err
	}
	helper, _, err := in.readIfRegular(helperPath(spec.Root))
	if err != nil {
		return r, err
	}
	rootOwned := ancestors == StatePresent && rootInfo.Kind == FileDir && rootInfo.UID == 0 && helperShaped(helper)

	// Slots stop before the anchor is flushed, so no slot process outlives
	// the pf layer.
	confTrusted, deleted, err := in.removeSlots(&r, left)
	if err != nil {
		return r, err
	}
	// Once a slot account is gone its uid can be reused, so its rules go too.
	if plistOwned || rootOwned || deleted > 0 {
		if err := host.FlushAnchor(spec.anchorPath()); err != nil {
			return r, fmt.Errorf("flush anchor %s: %w", spec.anchorPath(), err)
		}
		r.Removed = append(r.Removed, "rules in anchor "+spec.anchorPath())
	}
	// The group goes only with evidence of an install, so remove never
	// deletes some other group that merely has the name and id it was given.
	if confTrusted || rootOwned {
		if err := in.removeGroup(&r, left); err != nil {
			return r, err
		}
	}

	switch {
	case rootOwned:
		if err := in.removeRoot(&r, left); err != nil {
			return r, err
		}
	case rootInfo.Kind != FileMissing:
		left("%s: not a root-owned directory holding a slot-run helper below ancestors install accepts", spec.Root)
	}
	return r, nil
}

// removeRoot deletes the entries install names under the root, then the root
// and /Library/Freeside only if that leaves each empty.
func (in installer) removeRoot(r *RemoveReport, left func(string, ...any)) error {
	s := in.spec
	for _, entry := range rootEntries {
		info, err := in.host.Stat(s.path(entry))
		if err != nil {
			return err
		}
		if info.Kind != FileMissing {
			if err := in.removePath(r, s.path(entry)); err != nil {
				return err
			}
		}
	}
	removed, err := in.host.RemoveEmptyDir(s.Root)
	if err != nil {
		return err
	}
	if !removed {
		left("%s: holds entries install did not write", s.Root)
		return nil
	}
	r.Removed = append(r.Removed, s.Root)
	if parent := filepath.Dir(s.Root); filepath.Base(parent) == "Freeside" {
		removed, err := in.host.RemoveEmptyDir(parent)
		if err != nil {
			return err
		}
		if removed {
			r.Removed = append(r.Removed, parent)
		}
	}
	return nil
}

// readIfRegular returns a regular file's bytes. present reports whether
// anything is at path, so a directory there reads as present with no bytes.
func (in installer) readIfRegular(path string) (content []byte, present bool, err error) {
	info, err := in.host.Stat(path)
	if err != nil || info.Kind == FileMissing {
		return nil, false, err
	}
	if info.Kind != FileRegular {
		return nil, true, nil
	}
	content, err = in.host.ReadFile(path)
	return content, true, err
}

func (in installer) removePath(r *RemoveReport, path string) error {
	if err := in.host.RemoveAll(path); err != nil {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	r.Removed = append(r.Removed, path)
	return nil
}

// removeSlots deletes each account slots.conf lists that is shaped as a slot,
// after killing its processes and, acting as the slot, clearing the flags it
// may have set on files it wrote, so the root tree can be deleted. trusted
// reports whether it read a root-owned slots.conf.
func (in installer) removeSlots(r *RemoveReport, left func(string, ...any)) (trusted bool, deleted int, err error) {
	s := in.spec
	info, err := in.host.Stat(s.slotsConfPath())
	if err != nil || info.Kind == FileMissing {
		return false, 0, err
	}
	if info.Kind != FileRegular || info.UID != 0 || info.Mode&0o022 != 0 {
		left("%s: not a root-owned file; no account read from it", s.slotsConfPath())
		return false, 0, nil
	}
	conf, err := in.host.ReadFile(s.slotsConfPath())
	if err != nil {
		return false, 0, err
	}
	for _, name := range strings.Split(strings.TrimSuffix(string(conf), "\n"), "\n") {
		if !accountNamePattern.MatchString(name) {
			left("slot %q in %s: not an account name", name, s.slotsConfPath())
			continue
		}
		a, ok, err := in.host.User(name)
		if err != nil {
			return true, deleted, err
		}
		if !ok {
			continue
		}
		if !s.isSlotShaped(a) {
			left("account %s: primary gid %d and home %q are not a slot's", name, a.PrimaryGID, a.Home)
			continue
		}
		if err := in.host.ReapUser(a.UID); err != nil {
			return true, deleted, fmt.Errorf("stop the processes of %s: %w", name, err)
		}
		for _, dir := range []string{s.homePath(name), s.path("jobs", name)} {
			info, err := in.host.Stat(dir)
			if err != nil {
				return true, deleted, err
			}
			if info.Kind == FileDir {
				if err := in.host.ClearUserFlags(name, dir); err != nil {
					return true, deleted, fmt.Errorf("clear flags under %s as %s: %w", dir, name, err)
				}
			}
		}
		if err := in.host.DeleteUser(name); err != nil {
			return true, deleted, fmt.Errorf("delete account %s: %w", name, err)
		}
		deleted++
		r.Removed = append(r.Removed, "account "+name)
	}
	return true, deleted, nil
}

func (in installer) removeGroup(r *RemoveReport, left func(string, ...any)) error {
	s := in.spec
	g, ok, err := in.host.Group(s.Group)
	if err != nil || !ok {
		return err
	}
	if g.GID != s.FirstID {
		left("group %s: gid %d, not %d", s.Group, g.GID, s.FirstID)
		return nil
	}
	primaries, err := in.host.UsersWithPrimaryGroup(s.FirstID)
	if err != nil {
		return err
	}
	if len(g.Members) > 0 || len(primaries) > 0 {
		left("group %s: still names %s", s.Group, strings.Join(append(slices.Clone(g.Members), primaries...), ", "))
		return nil
	}
	if err := in.host.DeleteGroup(s.Group); err != nil {
		return fmt.Errorf("delete group %s: %w", s.Group, err)
	}
	r.Removed = append(r.Removed, "group "+s.Group)
	return nil
}
