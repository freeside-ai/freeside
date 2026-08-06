package claude

import (
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// The writer-outcome marker's integrity rests entirely on this command's
// filesystem topology, not on the nonce. An adversarial probe against the
// pinned image under Apple container confirmed both halves: pid 1's cmdline is
// readable at UID 1001, so the writer can always learn the nonce, while the
// forge itself fails at every step (writing, listing, removing, or renaming
// the root-owned 0700 control directory inside the sticky evidence directory;
// truncating the root-owned transcript; signalling pid 1; regaining privilege
// through a setuid copy). The nonce proves the marker is this run's, never
// that the writer did not author it.
//
// So this command string is a security control, and until this test it had
// none: an edit that reorders the chown sweep past the control directory's
// creation, or relaxes either mode, silently hands the writer the ability to
// report its own success. Ordering is asserted by position rather than by
// matching the whole script, so ordinary edits stay cheap.
func TestAgentCommandKeepsTheOutcomeMarkerOutOfWriterReach(t *testing.T) {
	t.Parallel()
	script := strings.Join(agentCommand("do the work", "session-1", "inv-1", nil), " ")
	evidenceDir := path.Dir(transcriptPath)
	controlDir := path.Dir(writerOutcomePath)

	if controlDir == evidenceDir {
		t.Fatalf("outcome marker sits directly in the agent-writable evidence directory %q", evidenceDir)
	}
	if path.Dir(controlDir) != evidenceDir {
		t.Fatalf("control directory %q is not inside the exported evidence directory %q",
			controlDir, evidenceDir)
	}

	at := func(needle string) int {
		t.Helper()
		i := strings.Index(script, needle)
		if i < 0 {
			t.Fatalf("agent command omits %q:\n%s", needle, script)
		}
		return i
	}

	// Root owns both directories the writer must not control, and the sticky
	// bit is what stops an unprivileged writer unlinking or renaming a
	// root-owned entry out of a world-writable directory.
	stickyEvidence := at("mkdir -p '" + evidenceDir + "'; chown 0:0 '" + evidenceDir +
		"'; chmod 1777 '" + evidenceDir + "'")
	privateControl := at("mkdir -p '" + controlDir + "'; chown 0:0 '" + controlDir +
		"'; chmod 0700 '" + controlDir + "'")
	stickyWorkspace := at("chown 0:0 '" + workspaceDir + "'; chmod 1777 '" + workspaceDir + "'")

	// The writer owns the repository it edits and nothing else.
	dropWorkspace := at("chown -hR " + agentUID + ":" + agentGID)
	drop := at("setpriv --reuid=" + agentUID + " --regid=" + agentGID)
	for _, flag := range []string{
		"--clear-groups", "--inh-caps=-all", "--ambient-caps=-all",
		"--bounding-set=-all", "--no-new-privs",
	} {
		if !strings.Contains(script, flag) {
			t.Errorf("privilege drop omits %q", flag)
		}
	}

	marker := at("> '" + writerOutcomePath + "'")
	dependencyCleanup := at("rm -rf -- '" + workspaceDir + "/node_modules'")
	if !strings.Contains(script, ward.WriterNoncePlaceholder) {
		t.Error("agent command carries no writer nonce placeholder for ward to substitute")
	}

	// Every mode must be in place before the writer exists, and the marker
	// must be written after it: a control directory created after the drop, or
	// a chown sweep that runs after it, would leave a window the writer owns.
	if stickyWorkspace > drop || stickyEvidence > drop ||
		privateControl > drop || dropWorkspace > drop {
		t.Error("the workspace, evidence, and control boundaries are not all established before the privilege drop")
	}
	if marker < drop {
		t.Error("the outcome marker is written before the writer runs")
	}
	if dependencyCleanup < drop || dependencyCleanup > marker {
		t.Error("the runtime dependency tree is not removed after the writer and before its outcome marker")
	}
	if privateControl < stickyEvidence {
		t.Error("the control directory is created before its sticky parent, so its mode is not the one that survives")
	}
	descriptor := at("> '" + transcriptDescriptorPath + "'")
	if descriptor > drop {
		t.Error("the transcript evidence descriptor is not fixed before the writer runs")
	}
	for _, field := range []string{
		export.EvidenceSourceVersion, `"label":"agent-transcript"`,
		`"path":"` + transcriptEvidencePath + `"`,
		`"sensitivity_class":"sensitive"`,
		`"producer_invocation_id":"inv-1"`,
	} {
		if !strings.Contains(script, field) {
			t.Errorf("transcript descriptor omits %q", field)
		}
	}
}

// The implementer hydration (#522) is a security-relevant ordering: it must
// run as root before the ownership sweep so the hydrated tree is dropped to
// the agent with everything else, a nonzero exit must skip the agent behind a
// distinct pre-agent sentinel, and the tree must be removed before the outcome
// marker so the git-blind export never sees it.
func TestAgentCommandHydratesBeforeTheOwnershipDrop(t *testing.T) {
	t.Parallel()
	prepare := []string{"/usr/local/bin/freeside-project-prepare"}
	script := strings.Join(agentCommand("do the work", "session-1", "inv-1", prepare), " ")

	at := func(needle string) int {
		t.Helper()
		i := strings.Index(script, needle)
		if i < 0 {
			t.Fatalf("agent command omits %q:\n%s", needle, script)
		}
		return i
	}

	// The preparation runs in the workspace with the verifier's HOME and
	// LC_ALL, so the implementer resolves the same lockfile-pinned toolchain.
	hydrate := at("( cd '" + workspaceDir + "' && HOME='" + prepareHome +
		"' LC_ALL=C '/usr/local/bin/freeside-project-prepare' )")
	prepareStatus := at("prepare_status=$?")
	sweep := at("chown -hR " + agentUID + ":" + agentGID)
	drop := at("setpriv --reuid=" + agentUID)
	marker := at("> '" + writerOutcomePath + "'")
	cleanup := at("rm -rf -- '" + workspaceDir + "/node_modules'")

	if hydrate > sweep {
		t.Error("hydration runs after the ownership sweep, so the hydrated tree is not dropped to the agent")
	}
	if prepareStatus > sweep {
		t.Error("the preparation exit status is captured after the ownership sweep")
	}

	// A nonzero preparation exit writes the distinct sentinel and gates the
	// agent behind the elif, so the agent never launches on a failed hydrate.
	guard := at("if [ \"$prepare_status\" -ne 0 ]; then status=" +
		strconv.Itoa(writerOutcomePrepareFailed) + "; elif [ -s '" + credentialTokenPath + "' ]")
	if guard > drop {
		t.Error("the preparation-failure guard is not established before the agent launch")
	}
	if cleanup < drop || cleanup > marker {
		t.Error("the hydrated tree is not removed after the writer and before its outcome marker")
	}
	if writerOutcomePrepareFailed == 86 {
		t.Error("the preparation sentinel collides with the token-missing status")
	}
}

// The attended launch command carries no hydration when no preparation is
// configured, so the 1A.0 conversation-turn path is byte-for-byte unchanged.
func TestAgentCommandWithoutPreparationIsUnchanged(t *testing.T) {
	t.Parallel()
	base := strings.Join(agentCommand("do the work", "session-1", "inv-1", nil), " ")
	empty := strings.Join(agentCommand("do the work", "session-1", "inv-1", []string{}), " ")
	if base != empty {
		t.Fatal("nil and empty preparation produce different launch commands")
	}
	for _, marker := range []string{"prepare_status", prepareHome, "LC_ALL=C"} {
		if strings.Contains(base, marker) {
			t.Errorf("launch command carries hydration text %q without a preparation command", marker)
		}
	}
	if !strings.Contains(base, "status=86; if [ -s '"+credentialTokenPath+"' ]") {
		t.Error("the token guard is not the plain single-branch form without a preparation command")
	}
}

func TestRuntimeDependencyCleanupDoesNotFollowReplacementSymlink(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(workspace, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o750); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(outside, "sentinel")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	dependencies := filepath.Join(workspace, "node_modules")
	if err := os.Symlink(outside, dependencies); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command( //nolint:gosec // G204: fixed shell snippet with a test-owned temp path
		"sh", "-c", `rm -rf -- "$1"`, "sh", dependencies,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("dependency cleanup: %v: %s", err, output)
	}
	if _, err := os.Lstat(dependencies); !os.IsNotExist(err) {
		t.Fatalf("replacement symlink survived cleanup: %v", err)
	}
	body, err := os.ReadFile(sentinel) //nolint:gosec // G304: test-owned path under t.TempDir
	if err != nil || string(body) != "keep" {
		t.Fatalf("cleanup followed replacement symlink: body=%q err=%v", body, err)
	}
}
