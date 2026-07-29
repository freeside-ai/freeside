package claude

import (
	"path"
	"strings"
	"testing"

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
	script := strings.Join(agentCommand("do the work", "session-1"), " ")
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
	if privateControl < stickyEvidence {
		t.Error("the control directory is created before its sticky parent, so its mode is not the one that survives")
	}
}
