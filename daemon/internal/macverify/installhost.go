package macverify

import "io/fs"

// unsetID is an Account's UID or PrimaryGID when the record has none, as a
// record dscl stopped writing partway may not.
const unsetID = -1

// Account is a local directory-service user record, as install reads and
// writes it.
type Account struct {
	Name       string
	UID        int
	PrimaryGID int
	Home       string
	Shell      string
	Password   string
	Hidden     bool
}

// Group is a local directory-service group record.
type Group struct {
	Name    string
	GID     int
	Members []string
}

// FileKind is what a path holds, read without following a final symlink.
type FileKind string

const (
	FileMissing FileKind = "missing"
	FileDir     FileKind = "directory"
	FileRegular FileKind = "regular file"
	FileOther   FileKind = "other"
)

// FileInfo is a path's kind, owner, and permission bits.
type FileInfo struct {
	Kind FileKind
	UID  int
	GID  int
	Mode fs.FileMode
}

// ServiceRun is a launchd job's run state: whether it is running, how many
// times it has started since it was loaded, and how its last run exited.
type ServiceRun struct {
	Running      bool
	Runs         int
	LastExitCode int
}

// Host is every side effect install, check, and removal have on the machine.
// The decisions stay in this package; a Host only reads and acts, so a fake
// can stand in for the directory service, the filesystem, launchd, and pf.
type Host interface {
	// Privileged reports whether the process runs as root.
	Privileged() bool

	User(name string) (Account, bool, error)
	UserNameByID(uid int) (string, bool, error)
	UsersWithPrimaryGroup(gid int) ([]string, error)
	Group(name string) (Group, bool, error)
	GroupNameByID(gid int) (string, bool, error)
	CreateGroup(name string, gid int) error
	// WriteUser creates the account or sets every attribute of an existing
	// one to a's values.
	WriteUser(a Account) error
	DeleteUser(name string) error
	DeleteGroup(name string) error
	// ReapUser kills every process uid owns and fails unless none is left.
	ReapUser(uid int) error
	// ClearUserFlags clears the user immutable and append-only flags under
	// path, acting as the account, so a hard link to another owner's file
	// keeps its flags.
	ClearUserFlags(name, path string) error

	Stat(path string) (FileInfo, error)
	ReadFile(path string) ([]byte, error)
	// ReadDir lists a directory's entry names.
	ReadDir(path string) ([]string, error)
	// WriteFile replaces path atomically with data, owned and moded as given.
	WriteFile(path string, data []byte, uid, gid int, mode fs.FileMode) error
	Mkdir(path string, uid, gid int, mode fs.FileMode) error
	SetOwnerMode(path string, uid, gid int, mode fs.FileMode) error
	RemoveAll(path string) error
	// RemoveEmptyDir removes path when it is an empty directory and reports
	// whether it did.
	RemoveEmptyDir(path string) (bool, error)

	// CheckSudoers validates a sudoers drop-in's syntax before it lands.
	CheckSudoers(content []byte) error

	ServiceLoaded(label string) (bool, error)
	BootstrapService(plistPath string) error
	// ServiceRunState reads a loaded job's run state. Output it cannot read
	// is an error, never a finished run.
	ServiceRunState(label string) (ServiceRun, error)
	BootoutService(label string) error

	// AnchorRules lists the rules loaded in a pf anchor, as pfctl prints
	// them. It needs root.
	AnchorRules(anchor string) ([]string, error)
	LoadAnchor(anchor, path string) error
	FlushAnchor(anchor string) error
	PacketFilterEnabled() (bool, error)
	// EnablePacketFilter takes a pf enable reference.
	EnablePacketFilter() error
}
