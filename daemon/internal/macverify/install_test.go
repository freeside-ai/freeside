package macverify

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
)

func testSpec() InstallSpec {
	return InstallSpec{
		Root: "/Library/Freeside/macverify", SlotPrefix: "fvslot", SlotCount: 2, Group: "fvslots", FirstID: 460,
		Operator: "benja", Anchor: "freeside-macverify", Label: "ai.freeside.macverify.pf",
		SudoersName: "freeside-slot-run",
	}
}

func installed(t *testing.T) (*fakeHost, InstallSpec) {
	t.Helper()
	h, spec := newFakeHost(), testSpec()
	if _, err := Install(spec, h); err != nil {
		t.Fatalf("Install: %v", err)
	}
	h.changes = nil
	return h, spec
}

func requireAllPresent(t *testing.T, spec InstallSpec, h *fakeHost) {
	t.Helper()
	findings, err := Inspect(spec, h)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		if f.State != StatePresent {
			t.Errorf("%s: %s %s", f.Piece, f.State, f.Detail)
		}
	}
}

func TestInstallSpecValidate(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*InstallSpec){
		"relative root":       func(s *InstallSpec) { s.Root = "Library/x" },
		"unclean root":        func(s *InstallSpec) { s.Root = "/Library/x/" },
		"slash root":          func(s *InstallSpec) { s.Root = "/" },
		"root with space":     func(s *InstallSpec) { s.Root = "/Library/a b" },
		"no slots":            func(s *InstallSpec) { s.SlotCount = 0 },
		"uppercase slot":      func(s *InstallSpec) { s.SlotPrefix = "FV" },
		"long slot":           func(s *InstallSpec) { s.SlotPrefix = strings.Repeat("a", 32) },
		"group is a slot":     func(s *InstallSpec) { s.Group = "fvslot1" },
		"zero id":             func(s *InstallSpec) { s.FirstID = 0 },
		"root operator":       func(s *InstallSpec) { s.Operator = "root" },
		"empty operator":      func(s *InstallSpec) { s.Operator = "" },
		"operator in sudoers": func(s *InstallSpec) { s.Operator = "ALL" + " " },
		"anchor with slash":   func(s *InstallSpec) { s.Anchor = "a/b" },
		"label with xml":      func(s *InstallSpec) { s.Label = "a<b" },
		"sudoers with dot":    func(s *InstallSpec) { s.SudoersName = "a.b" },
	} {
		spec := testSpec()
		mutate(&spec)
		if err := spec.validate(true); !errors.Is(err, ErrInvalidSpec) {
			t.Errorf("%s: validate = %v, want ErrInvalidSpec", name, err)
		}
	}
	if err := testSpec().validate(true); err != nil {
		t.Fatalf("default spec: %v", err)
	}
}

func TestInstallFresh(t *testing.T) {
	t.Parallel()
	h, spec := installed(t)
	requireAllPresent(t, spec, h)
	if a := h.users["fvslot2"]; a != (Account{
		Name: "fvslot2", UID: 462, PrimaryGID: 460, Home: "/Library/Freeside/macverify/homes/fvslot2",
		Shell: "/usr/bin/false", Password: "*", Hidden: true,
	}) {
		t.Errorf("fvslot2 = %+v", a)
	}
	if got := string(h.files["/Library/Freeside/macverify/bin/slot-run"].data); got != string(helperScript) {
		t.Error("helper is not the embedded script")
	}
	if got := string(h.files["/Library/Freeside/macverify/bin/slots.conf"].data); got != "fvslot1\nfvslot2\n" {
		t.Errorf("slots.conf = %q", got)
	}
	sudoers := h.files["/etc/sudoers.d/freeside-slot-run"]
	if !strings.HasSuffix(string(sudoers.data), "\nbenja ALL=(root) NOPASSWD: /Library/Freeside/macverify/bin/slot-run\n") ||
		sudoers.info.Mode != 0o440 {
		t.Errorf("sudoers = %v %q", sudoers.info.Mode, sudoers.data)
	}
	if got := h.anchors["com.apple/freeside-macverify"]; len(got) != 4 || got[0] != "block drop out quick all user = 461" {
		t.Errorf("loaded anchor = %q", got)
	}
	if home := h.files["/Library/Freeside/macverify/homes/fvslot1"].info; home.UID != 461 || home.Mode != 0o700 {
		t.Errorf("home = %+v", home)
	}
	for _, dir := range []string{"swift-cache", "go-cache"} {
		if cache := h.files["/Library/Freeside/macverify/"+dir].info; cache.Kind != FileDir || cache.UID != 501 ||
			cache.Mode != 0o755 {
			t.Errorf("%s = %+v, want the operator's directory at 0755", dir, cache)
		}
	}
	if !h.pf || !h.services["ai.freeside.macverify.pf"] {
		t.Errorf("pf enabled %v, job loaded %v", h.pf, h.services)
	}
	if got := spec.Config(); got.ExecutorRoot != spec.Root || !slices.Equal(got.Slots, []string{"fvslot1", "fvslot2"}) ||
		string(got.PacketFilterRules) != string(h.files[spec.anchorFilePath()].data) {
		t.Errorf("Config = %+v", got)
	}
}

// TestInstallWaitsForTheBootJobsLoad has the boot job's pfctl load land
// some polls after bootstrap, as launchd runs it, and checks install never
// loads the anchor itself, which failed with "Resource busy" on the
// reference host while the job's load was in flight (#3009). The wait
// sleeps on the real clock, so it runs in a synctest bubble.
func TestInstallWaitsForTheBootJobsLoad(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h, spec := newFakeHost(), testSpec()
		h.bootPolls = 5
		if _, err := Install(spec, h); err != nil {
			t.Fatalf("Install: %v", err)
		}
		if h.anchorLoads != 0 || h.inFlightLoads != 0 {
			t.Fatalf("install loaded the anchor %d times (%d in flight), want the boot job's load only",
				h.anchorLoads, h.inFlightLoads)
		}
		requireAllPresent(t, spec, h)
	})
}

func TestInstallFailsWhenTheBootJobFails(t *testing.T) {
	t.Parallel()
	for name, setup := range map[string]struct {
		host func(*fakeHost)
		want string
	}{
		"nonzero exit": {func(h *fakeHost) { h.bootPolls, h.bootExit = 2, 1 }, "exited 1"},
		"never ends":   {func(h *fakeHost) { h.bootNever = true }, "did not finish its first run"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				h, spec := newFakeHost(), testSpec()
				setup.host(h)
				_, err := Install(spec, h)
				if err == nil || !strings.Contains(err.Error(), "boot job "+spec.Label) ||
					!strings.Contains(err.Error(), setup.want) {
					t.Fatalf("Install error = %v, want the boot job's failure naming %q", err, setup.want)
				}
				if h.anchorLoads != 0 {
					t.Fatalf("install loaded the anchor %d times after the boot job failed", h.anchorLoads)
				}
			})
		})
	}
}

func TestInstallRerunChangesNothing(t *testing.T) {
	t.Parallel()
	h, spec := installed(t)
	if _, err := Install(spec, h); err != nil {
		t.Fatal(err)
	}
	if len(h.changes) != 0 {
		t.Fatalf("rerun changed %q", h.changes)
	}
}

func TestInstallCompletesPartialInstall(t *testing.T) {
	t.Parallel()
	h, spec := installed(t)
	delete(h.users, "fvslot2")
	delete(h.files, spec.path("homes", "fvslot2"))
	delete(h.files, spec.sudoersPath())
	delete(h.services, spec.Label)
	delete(h.anchors, spec.anchorPath())
	h.pf = false
	jobs := h.files[spec.path("jobs")]
	jobs.info.Mode = 0o777
	h.files[spec.path("jobs")] = jobs
	a := h.users["fvslot1"]
	a.Shell, a.Hidden = "/bin/zsh", false
	h.users["fvslot1"] = a

	findings, err := Inspect(spec, h)
	if err != nil {
		t.Fatal(err)
	}
	unhealthy := 0
	for _, f := range findings {
		if !f.Healthy() {
			unhealthy++
		}
	}
	if unhealthy != 8 {
		t.Errorf("Inspect found %d unhealthy pieces, want 8: %+v", unhealthy, findings)
	}
	if _, err := Install(spec, h); err != nil {
		t.Fatal(err)
	}
	requireAllPresent(t, spec, h)
}

// TestInstallAdoptsHandSetup seeds what daemon/README.md's hand setup made
// before install existed and checks install completes it in place.
func TestInstallAdoptsHandSetup(t *testing.T) {
	t.Parallel()
	h, spec := newFakeHost(), testSpec()
	h.groups["fvslots"] = Group{Name: "fvslots", GID: 460}
	for i, slot := range spec.Slots() {
		h.users[slot] = Account{
			Name: slot, UID: 461 + i, PrimaryGID: 460, Home: spec.homePath(slot),
			Shell: slotShell, Password: slotPassword, Hidden: true,
		}
	}
	for _, dir := range []string{
		"/Library/Freeside", spec.Root, spec.path("bin"), spec.path("jobs"),
		spec.path("homes"), spec.path("template"), spec.path("template", "home"),
	} {
		h.files[dir] = fakeFile{info: FileInfo{Kind: FileDir, Mode: 0o755}}
	}
	h.files[helperPath(spec.Root)] = fakeFile{
		info: FileInfo{Kind: FileRegular, Mode: 0o755},
		data: []byte("#!/bin/sh\n# slot-run: an older helper\n"),
	}
	h.files[spec.slotsConfPath()] = fakeFile{info: FileInfo{Kind: FileRegular, Mode: 0o644}, data: []byte("fvslot1\nfvslot2\n")}
	h.files[spec.sudoersPath()] = fakeFile{
		info: FileInfo{Kind: FileRegular, Mode: 0o440},
		data: []byte("benja ALL=(root) NOPASSWD: /Library/Freeside/macverify/bin/slot-run\n"),
	}

	if _, err := Install(spec, h); err != nil {
		t.Fatal(err)
	}
	requireAllPresent(t, spec, h)
	for _, change := range h.changes {
		if strings.HasPrefix(change, "write account") || strings.HasPrefix(change, "create group") {
			t.Errorf("install rewrote an adopted record: %s", change)
		}
	}
}

func TestInstallRewritesChangedHelper(t *testing.T) {
	t.Parallel()
	h, spec := installed(t)
	f := h.files[helperPath(spec.Root)]
	f.data = []byte("#!/bin/sh\n# slot-run: the previous daemon's helper\n")
	h.files[helperPath(spec.Root)] = f
	if _, err := Install(spec, h); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(h.changes, []string{"write " + helperPath(spec.Root)}) {
		t.Fatalf("changes = %q, want the helper rewritten only", h.changes)
	}
}

func TestInstallRefusesWithoutChanging(t *testing.T) {
	t.Parallel()
	for name, seed := range map[string]func(*fakeHost){
		"slot name is a real user": func(h *fakeHost) {
			h.users["fvslot1"] = Account{Name: "fvslot1", UID: 502, PrimaryGID: 20, Home: "/Users/fvslot1"}
		},
		"slot uid is taken": func(h *fakeHost) {
			h.users["other"] = Account{Name: "other", UID: 462, PrimaryGID: 20, Home: "/Users/other"}
		},
		"slot-shaped account with another uid": func(h *fakeHost) {
			h.users["fvslot1"] = Account{
				Name: "fvslot1", UID: 499, PrimaryGID: 460,
				Home: "/Library/Freeside/macverify/homes/fvslot1",
			}
		},
		"group name has another gid": func(h *fakeHost) { h.groups["fvslots"] = Group{Name: "fvslots", GID: 999} },
		"gid is taken":               func(h *fakeHost) { h.groups["other"] = Group{Name: "other", GID: 460} },
		"group has a foreign member": func(h *fakeHost) {
			h.groups["fvslots"] = Group{Name: "fvslots", GID: 460, Members: []string{"benja"}}
		},
		"group is a foreign primary group": func(h *fakeHost) {
			h.users["guest2"] = Account{Name: "guest2", UID: 600, PrimaryGID: 460, Home: "/Users/guest2"}
		},
		"ancestor not root-owned": func(h *fakeHost) {
			h.files["/Library"] = fakeFile{info: FileInfo{Kind: FileDir, UID: 501, Mode: 0o755}}
		},
		"ancestor writable by others": func(h *fakeHost) {
			h.files["/Library/Freeside"] = fakeFile{info: FileInfo{Kind: FileDir, Mode: 0o777}}
		},
		"foreign sudoers file": func(h *fakeHost) {
			h.files["/etc/sudoers.d/freeside-slot-run"] = fakeFile{
				info: FileInfo{Kind: FileRegular, Mode: 0o440},
				data: []byte("benja ALL=(ALL) NOPASSWD: ALL\n"),
			}
		},
		"foreign LaunchDaemon": func(h *fakeHost) {
			h.files["/Library/LaunchDaemons/ai.freeside.macverify.pf.plist"] = fakeFile{
				info: FileInfo{Kind: FileRegular, Mode: 0o644}, data: []byte("<plist/>"),
			}
		},
		"root is a file": func(h *fakeHost) {
			h.files["/Library/Freeside"] = fakeFile{info: FileInfo{Kind: FileDir, Mode: 0o755}}
			h.files["/Library/Freeside/macverify"] = fakeFile{info: FileInfo{Kind: FileRegular, Mode: 0o644}}
		},
		"root is an existing tree": func(h *fakeHost) {
			h.files["/Library/Freeside"] = fakeFile{info: FileInfo{Kind: FileDir, Mode: 0o755}}
			h.files["/Library/Freeside/macverify"] = fakeFile{info: FileInfo{Kind: FileDir, Mode: 0o755}}
			h.files["/Library/Freeside/macverify/lib"] = fakeFile{info: FileInfo{Kind: FileDir, Mode: 0o755}}
		},
		"root owned by the operator": func(h *fakeHost) {
			h.files["/Library/Freeside"] = fakeFile{info: FileInfo{Kind: FileDir, Mode: 0o755}}
			h.files["/Library/Freeside/macverify"] = fakeFile{info: FileInfo{Kind: FileDir, UID: 501, Mode: 0o755}}
		},
		"template home not empty": func(h *fakeHost) {
			for _, dir := range []string{
				"/Library/Freeside", "/Library/Freeside/macverify",
				"/Library/Freeside/macverify/template", "/Library/Freeside/macverify/template/home",
			} {
				h.files[dir] = fakeFile{info: FileInfo{Kind: FileDir, Mode: 0o755}}
			}
			h.files["/Library/Freeside/macverify/template/home/.ssh"] = fakeFile{info: FileInfo{Kind: FileDir, Mode: 0o700}}
		},
		"Swift cache owned by another user": func(h *fakeHost) {
			seedRoot(h)
			h.users["other"] = Account{Name: "other", UID: 502, PrimaryGID: 20, Home: "/Users/other"}
			h.files["/Library/Freeside/macverify/swift-cache"] = fakeFile{info: FileInfo{Kind: FileDir, UID: 502, Mode: 0o755}}
		},
		"Swift cache is a file": func(h *fakeHost) {
			seedRoot(h)
			h.files["/Library/Freeside/macverify/swift-cache"] = fakeFile{info: FileInfo{Kind: FileRegular, UID: 501, Mode: 0o644}}
		},
		"Go cache is not a directory": func(h *fakeHost) {
			seedRoot(h)
			h.files["/Library/Freeside/macverify/go-cache"] = fakeFile{info: FileInfo{Kind: FileOther, UID: 501, Mode: 0o755}}
		},
		"slot is an administrator": func(h *fakeHost) {
			h.groups["admin"] = Group{Name: "admin", GID: 80, Members: []string{"benja", "fvslot1"}}
			h.groups["fvslots"] = Group{Name: "fvslots", GID: 460}
			h.users["fvslot1"] = Account{
				Name: "fvslot1", UID: 461, PrimaryGID: 460,
				Home: "/Library/Freeside/macverify/homes/fvslot1",
			}
		},
		"unknown operator": func(h *fakeHost) { delete(h.users, "benja") },
		"operator is uid 0": func(h *fakeHost) {
			h.users["benja"] = Account{Name: "benja", UID: 0, PrimaryGID: 0, Home: "/var/root"}
		},
		"not root": func(h *fakeHost) { h.root = false },
	} {
		h := newFakeHost()
		seed(h)
		if _, err := Install(testSpec(), h); !errors.Is(err, ErrInstallRefused) {
			t.Errorf("%s: Install = %v, want ErrInstallRefused", name, err)
		}
		if len(h.changes) != 0 {
			t.Errorf("%s: refused install changed %q", name, h.changes)
		}
	}
}

// seedRoot gives the fake host an empty, root-owned executor root.
func seedRoot(h *fakeHost) {
	for _, dir := range []string{"/Library/Freeside", "/Library/Freeside/macverify"} {
		h.files[dir] = fakeFile{info: FileInfo{Kind: FileDir, Mode: 0o755}}
	}
}

// Install repairs the Swift cache to the operator's at 0755: a root-owned one
// is what an install interrupted between creating it and handing it over
// leaves, and slot-run refuses to seed from a group-writable one.
// operatorCaches are the cache directories install hands to the operator.
var operatorCaches = []string{swiftCacheDirName, goCacheDirName}

func TestInstallRepairsOperatorCaches(t *testing.T) {
	t.Parallel()
	for _, dir := range operatorCaches {
		for name, info := range map[string]FileInfo{
			"root-owned":            {Kind: FileDir, Mode: 0o700},
			"group-writable":        {Kind: FileDir, UID: 501, Mode: 0o775},
			"closed to other users": {Kind: FileDir, UID: 501, Mode: 0o700},
		} {
			h, spec := installed(t)
			cache := spec.path(dir)
			h.files[cache] = fakeFile{info: info}
			if _, err := Install(spec, h); err != nil {
				t.Fatalf("%s %s: %v", dir, name, err)
			}
			if !slices.Equal(h.changes, []string{"chown/chmod " + cache}) {
				t.Errorf("%s %s: changes = %q, want the cache repaired only", dir, name, h.changes)
			}
			if got := h.files[cache].info; got.UID != 501 || got.Mode != 0o755 {
				t.Errorf("%s %s: cache = %+v", dir, name, got)
			}
		}
	}
}

// Install leaves what populate wrote in the operator's cache alone.
func TestInstallKeepsOperatorCacheEntries(t *testing.T) {
	t.Parallel()
	h, spec := installed(t)
	entries := []string{
		spec.path(swiftCacheDirName, strings.Repeat("a", 64)),
		spec.path(goCacheDirName, strings.Repeat("a", 64)),
		spec.path(goCacheDirName, "go"+goToolchainVersion),
	}
	for _, entry := range entries {
		h.files[entry] = fakeFile{info: FileInfo{Kind: FileDir, UID: 501, Mode: 0o755}}
	}
	if _, err := Install(spec, h); err != nil {
		t.Fatal(err)
	}
	if len(h.changes) != 0 {
		t.Fatalf("rerun changed %q", h.changes)
	}
	for _, entry := range entries {
		if _, ok := h.files[entry]; !ok {
			t.Errorf("install deleted %s", entry)
		}
	}
}

func TestInspectOperatorCachesNeedAKnownOperator(t *testing.T) {
	t.Parallel()
	h, spec := installed(t)
	delete(h.users, spec.Operator)
	findings, err := Inspect(spec, h)
	if err != nil {
		t.Fatal(err)
	}
	for _, piece := range []string{"Swift cache " + spec.path(swiftCacheDirName), "Go cache " + spec.path(goCacheDirName)} {
		i := slices.IndexFunc(findings, func(f Finding) bool { return f.Piece == piece })
		if i < 0 {
			t.Fatalf("no %s finding", piece)
		}
		if f := findings[i]; f.State != StateMisconfigured {
			t.Errorf("%s with no operator account: %s %s, want misconfigured", piece, f.State, f.Detail)
		}
	}
}

func TestInspectNamesEachMissingPiece(t *testing.T) {
	t.Parallel()
	findings, err := Inspect(testSpec(), newFakeHost())
	if err != nil {
		t.Fatal(err)
	}
	var missing []string
	for _, f := range findings {
		if f.State != StateMissing {
			t.Errorf("%s: %s, want missing", f.Piece, f.State)
		}
		missing = append(missing, f.Piece)
	}
	want := []string{
		"ancestors of /Library/Freeside/macverify",
		"directory /Library/Freeside/macverify", "directory /Library/Freeside/macverify/bin",
		"directory /Library/Freeside/macverify/jobs", "directory /Library/Freeside/macverify/homes",
		"directory /Library/Freeside/macverify/template", "directory /Library/Freeside/macverify/template/home",
		"directory /Library/Freeside/macverify/pf", "Swift cache /Library/Freeside/macverify/swift-cache", "Go cache /Library/Freeside/macverify/go-cache",
		"helper /Library/Freeside/macverify/bin/slot-run", "slot list /Library/Freeside/macverify/bin/slots.conf",
		"group fvslots", "account fvslot1", "account fvslot2", "home fvslot1", "home fvslot2",
		"sudoers file /etc/sudoers.d/freeside-slot-run", "anchor file /Library/Freeside/macverify/pf/anchor.conf",
		"boot LaunchDaemon /Library/LaunchDaemons/ai.freeside.macverify.pf.plist",
		"boot job ai.freeside.macverify.pf", "loaded rules com.apple/freeside-macverify", "packet filter",
	}
	if !slices.Equal(missing, want) {
		t.Fatalf("pieces =\n%q\nwant\n%q", missing, want)
	}
}

func TestInspectUnprivilegedReportsUnread(t *testing.T) {
	t.Parallel()
	h, spec := installed(t)
	h.root = false
	findings, err := Inspect(spec, h)
	if err != nil {
		t.Fatal(err)
	}
	var unread []string
	for _, f := range findings {
		switch f.State {
		case StatePresent:
		case StateUnread:
			unread = append(unread, f.Piece)
		default:
			t.Errorf("%s: %s %s", f.Piece, f.State, f.Detail)
		}
	}
	want := []string{"sudoers file /etc/sudoers.d/freeside-slot-run", "loaded rules com.apple/freeside-macverify", "packet filter"}
	if !slices.Equal(unread, want) {
		t.Fatalf("unread = %q, want %q", unread, want)
	}
}

func TestRemoveDeletesTheInstall(t *testing.T) {
	t.Parallel()
	h, spec := installed(t)
	h.live[461] = true
	// Populated entries go with the caches: populate can fill them again.
	for _, dir := range operatorCaches {
		entry := spec.path(dir, strings.Repeat("a", 64))
		h.files[entry] = fakeFile{info: FileInfo{Kind: FileDir, UID: 501, Mode: 0o755}}
		h.files[entry+"/record.json"] = fakeFile{info: FileInfo{Kind: FileRegular, UID: 501, Mode: 0o644}}
	}
	report, err := Remove(spec, h)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Left) != 0 {
		t.Errorf("left %q", report.Left)
	}
	if h.live[461] {
		t.Error("fvslot1's process survived removal")
	}
	for path := range h.files {
		if strings.HasPrefix(path, "/Library/Freeside") || path == spec.sudoersPath() || path == spec.plistPath() {
			t.Errorf("%s survived removal", path)
		}
	}
	if len(h.users) != 2 || len(h.groups) != 1 || len(h.services) != 0 || len(h.anchors) != 0 {
		t.Errorf("users %v, groups %v, services %v, anchors %v remain", h.users, h.groups, h.services, h.anchors)
	}
	// The sudoers rule goes before any slot is touched.
	if !strings.HasPrefix(h.changes[0], "remove "+spec.sudoersPath()) {
		t.Errorf("first change %q, want the sudoers file removed", h.changes[0])
	}
	// The caches go before the helper, so a cache that will not delete
	// leaves a root a rerun removal still recognizes.
	for _, dir := range operatorCaches {
		if cache, helper := slices.Index(h.changes, "remove "+spec.path(dir)),
			slices.Index(h.changes, "remove "+spec.path("bin")); cache < 0 || cache > helper {
			t.Errorf("changes %q, want %s removed before bin", h.changes, dir)
		}
	}
}

func TestRemoveAbsentInstallIsNoOp(t *testing.T) {
	t.Parallel()
	h := newFakeHost()
	report, err := Remove(testSpec(), h)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.changes) != 0 || len(report.Removed) != 0 || len(report.Left) != 0 {
		t.Fatalf("changes %q, report %+v", h.changes, report)
	}
}

func TestRemoveLeavesWhatInstallDidNotWrite(t *testing.T) {
	t.Parallel()
	h, spec := installed(t)
	conf := h.files[spec.slotsConfPath()]
	conf.data = []byte("fvslot1\nfvslot2\nbenja\nBad Name\n")
	h.files[spec.slotsConfPath()] = conf
	sudoers := h.files[spec.sudoersPath()]
	sudoers.data = append(sudoers.data, "benja ALL=(ALL) ALL\n"...)
	h.files[spec.sudoersPath()] = sudoers
	plist := h.files[spec.plistPath()]
	plist.data = append(plist.data, "<!-- edited -->\n"...)
	h.files[spec.plistPath()] = plist

	report, err := Remove(spec, h)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := h.users["benja"]; !ok {
		t.Error("removal deleted the operator account listed in slots.conf")
	}
	for _, path := range []string{spec.sudoersPath(), spec.plistPath()} {
		if _, ok := h.files[path]; !ok {
			t.Errorf("removal deleted %s, which install did not write", path)
		}
	}
	if len(report.Left) != 4 {
		t.Errorf("left %q, want the sudoers file, the plist, benja, and the bad name", report.Left)
	}
	if _, ok := h.users["fvslot1"]; ok {
		t.Error("fvslot1 survived")
	}
}

func TestRemoveKeepsRootWithoutHelper(t *testing.T) {
	t.Parallel()
	h, spec := installed(t)
	delete(h.files, helperPath(spec.Root))
	report, err := Remove(spec, h)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := h.files[spec.Root]; !ok {
		t.Error("removal deleted a root holding no helper")
	}
	if len(report.Left) != 1 || !strings.HasPrefix(report.Left[0], spec.Root) {
		t.Errorf("left %q", report.Left)
	}
}

func TestRemoveStopsWhenASlotWillNotDie(t *testing.T) {
	t.Parallel()
	h, spec := installed(t)
	h.stuck[461] = true
	if _, err := Remove(spec, h); err == nil {
		t.Fatal("Remove succeeded with a slot process alive")
	}
	if _, ok := h.users["fvslot1"]; !ok {
		t.Error("removal deleted an account whose process lives")
	}
	if _, ok := h.files[spec.Root]; !ok {
		t.Error("removal deleted the root while a slot process lives")
	}
}

func TestSudoersOperator(t *testing.T) {
	t.Parallel()
	root := "/Library/Freeside/macverify"
	for content, want := range map[string]bool{
		string(renderSudoers("ops.user", root)):                                  true,
		"benja ALL=(root) NOPASSWD: /Library/Freeside/macverify/bin/slot-run\n":  true,
		"benja ALL=(root) NOPASSWD: /Library/Freeside/other/bin/slot-run\n":      false,
		string(renderSudoers("benja", root)) + "benja ALL=(ALL) ALL\n":           false,
		"%admin ALL=(root) NOPASSWD: /Library/Freeside/macverify/bin/slot-run\n": false,
	} {
		if _, got := sudoersOperator([]byte(content), root); got != want {
			t.Errorf("sudoersOperator(%q) = %v, want %v", content, got, want)
		}
	}
}

func TestInstallCompletesAnIncompleteAccount(t *testing.T) {
	t.Parallel()
	h, spec := newFakeHost(), testSpec()
	h.users["fvslot1"] = Account{Name: "fvslot1", UID: unsetID, PrimaryGID: unsetID}
	if _, err := Install(spec, h); err != nil {
		t.Fatal(err)
	}
	requireAllPresent(t, spec, h)
}

func TestInspectCountsAnchorRulesPerSlot(t *testing.T) {
	t.Parallel()
	h, spec := installed(t)
	// Every rule names a slot, but each slot misses a direction.
	h.anchors[spec.anchorPath()] = []string{
		"block drop in quick all user = 461", "block drop in quick all user = fvslot1",
		"block drop out quick all user = 462", "block drop out quick all user = fvslot2",
	}
	findings, err := Inspect(spec, h)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		if f.Piece == "loaded rules "+spec.anchorPath() && f.State != StateMisconfigured {
			t.Fatalf("mixed anchor = %s, want misconfigured", f.State)
		}
	}
}

func TestRemoveStopsSlotsBeforeFlushingTheAnchor(t *testing.T) {
	t.Parallel()
	h, spec := installed(t)
	if _, err := Remove(spec, h); err != nil {
		t.Fatal(err)
	}
	reaped := slices.Index(h.changes, "reap 462")
	flushed := slices.Index(h.changes, "flush anchor "+spec.anchorPath())
	if reaped < 0 || flushed < reaped {
		t.Fatalf("changes %q: want every slot reaped before the flush", h.changes)
	}
}

// Without a slots.conf or helper, remove has no evidence of an install, so
// it leaves even an empty group with the given name and id.
func TestRemoveLeavesGroupWithoutAnInstall(t *testing.T) {
	t.Parallel()
	h, spec := newFakeHost(), testSpec()
	spec.Group, spec.FirstID = "everyone", 12
	h.groups["everyone"] = Group{Name: "everyone", GID: 12}
	if _, err := Remove(spec, h); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.groups["everyone"]; !ok || len(h.changes) != 0 {
		t.Fatalf("changes %q; want the group left", h.changes)
	}
}

// A partial install that stopped after its accounts still leaves slots.conf,
// so removal finds and deletes them.
func TestRemoveFindsAccountsOfAPartialInstall(t *testing.T) {
	t.Parallel()
	h, spec := installed(t)
	delete(h.files, helperPath(spec.Root))
	if _, err := Remove(spec, h); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.users["fvslot1"]; ok {
		t.Error("fvslot1 survived")
	}
	if _, ok := h.groups["fvslots"]; ok {
		t.Error("the slot group survived")
	}
	if _, ok := h.anchors[spec.anchorPath()]; ok {
		t.Error("the deleted slots' anchor rules survived")
	}
}

func TestRemoveKeepsEntriesInstallDidNotWrite(t *testing.T) {
	t.Parallel()
	h, spec := installed(t)
	h.files[spec.path("notes")] = fakeFile{info: FileInfo{Kind: FileRegular, Mode: 0o644}}
	report, err := Remove(spec, h)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := h.files[spec.path("notes")]; !ok {
		t.Error("removal deleted an entry install did not write")
	}
	if _, ok := h.files[spec.path("bin")]; ok {
		t.Error("removal kept bin/")
	}
	if len(report.Left) != 1 || !strings.Contains(report.Left[0], "entries install did not write") {
		t.Errorf("left %q", report.Left)
	}
}
