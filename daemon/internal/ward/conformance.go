package ward

import (
	"bufio"
	"bytes"
	"slices"
	"sort"
	"strings"
)

const fixedContainerPathEnv = "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// validateAgentSpec verifies the generated writer spec against checks 1 and
// 2. It runs on the spec the gate itself built: the gate re-verifies its own
// construction instead of trusting it, so a future spec-builder change that
// violates the contract fails here, not in review.
//
// Check 1 (credential_separation): the workspace is its own named volume,
// mounted read-write at exactly the configured target; every credential is a
// different named volume, read-only, at its own absolute target outside the
// workspace. The spec vocabulary cannot place a credential in the root
// filesystem image; a mount that tries (target "/", or a path under the
// workspace) is rejected here.
//
// Check 2 (control_plane_isolation): no host bind of any kind, so no host
// CLI, runtime socket, daemon state, SSH agent, home directory, or registry
// credential can reach the VM; ContainerSpec cannot express SSH forwarding
// or published sockets at all. Environment content is not scanned: a
// credential smuggled into Env is not mechanically detectable, and §5.4
// scanning of the export is the honest downstream control.
func validateAgentSpec(cfg Config, spec ContainerSpec, workspaceVolume string) error {
	// A bare-key env entry makes the CLI inherit the host's value, pulling a
	// host credential into the writer VM (check 2); every entry must set an
	// explicit value.
	for _, e := range spec.Env {
		if envInherits(e) {
			return failf(CheckControlPlaneIsolation,
				"agent environment contains a host-inheriting entry; every variable must be key=value with a non-empty key")
		}
	}
	if spec.NetworkDisabled || spec.Network == "" || !cliSafe(spec.Network) {
		return failf(CheckControlPlaneIsolation, "agent spec does not declare one enforceable provider network")
	}
	if _, ok := environmentByKey(append([]string{fixedContainerPathEnv}, spec.Env...)); !ok {
		return failf(CheckControlPlaneIsolation, "agent environment contains a duplicate or malformed key")
	}

	seenTargets := make(map[string]bool, len(spec.Mounts))
	workspaceMounts := 0
	for _, m := range spec.Mounts {
		if !m.Type.valid() {
			return failf(CheckControlPlaneIsolation, "agent spec carries an unknown mount type")
		}
		if m.Type == MountBind {
			return failf(CheckControlPlaneIsolation, "agent spec carries a host bind")
		}
		// A comma or control character in a mount field would let the CLI
		// parse an injected mount option, realizing a topology this
		// spec-level check did not approve before the runtime re-inspection.
		if !cliSafe(m.Source) {
			return failf(CheckCredentialSeparation, "agent mount source carries a CLI delimiter")
		}
		if !cliSafe(m.Target) {
			return failf(CheckCredentialSeparation, "agent mount target carries a CLI delimiter")
		}
		if !cleanAbs(m.Target) {
			return failf(CheckCredentialSeparation, "agent mount target is not a clean absolute non-root path")
		}
		if seenTargets[m.Target] {
			return failf(CheckCredentialSeparation, "agent spec repeats a mount target")
		}
		seenTargets[m.Target] = true

		if m.Target == cfg.WorkspaceTarget {
			workspaceMounts++
			if m.Source != workspaceVolume {
				return failf(CheckCredentialSeparation, "workspace target mounts the wrong volume")
			}
			if m.ReadOnly {
				return failf(CheckCredentialSeparation,
					"workspace mount is read-only; the writer needs read-write")
			}
			continue
		}

		// Every non-workspace mount is a credential mount.
		if strings.HasPrefix(m.Target, cfg.WorkspaceTarget+"/") {
			return failf(CheckCredentialSeparation, "credential mount is inside the workspace")
		}
		if m.Source == workspaceVolume {
			return failf(CheckCredentialSeparation, "credential mount reuses the workspace volume")
		}
		if !m.ReadOnly {
			return failf(CheckCredentialSeparation, "credential mount is not read-only")
		}
	}
	if workspaceMounts != 1 {
		return failf(CheckCredentialSeparation, "agent spec does not carry exactly one workspace mount")
	}
	return nil
}

// verifyAgentAllowlist re-runs checks 1 and 2 against the runtime-realized
// writer before it receives credentials or executes. The generated spec is
// necessary intent evidence, but only inspect proves the VM topology the
// runtime will actually start.
func verifyAgentAllowlist(rep InspectReport, spec ContainerSpec) error {
	if rep.ID != spec.Name {
		return failf(CheckControlPlaneIsolation, "agent inspection identified the wrong container")
	}
	if !rep.AllowlistFieldsObserved {
		return failf(CheckControlPlaneIsolation, "agent inspection omitted required configuration")
	}
	if rep.State != StateStopped {
		return failf(CheckControlPlaneIsolation, "agent was not observed stopped before execution")
	}
	if !sameImage(spec.Image, rep.ImageReference) {
		return failf(CheckControlPlaneIsolation, "agent inspection reported the wrong image")
	}
	if rep.WorkingDirectory != "/" {
		return failf(CheckControlPlaneIsolation, "agent working directory is not the fixed image root")
	}
	if !slices.Equal(rep.Command, spec.Command) {
		return failf(CheckControlPlaneIsolation, "agent inspection reported the wrong command")
	}
	expectedEnv := append([]string{fixedContainerPathEnv}, spec.Env...)
	if !sameEnvironment(rep.Env, expectedEnv) {
		return failf(CheckControlPlaneIsolation, "agent inspection reported a different environment")
	}
	if !sameMounts(rep.Mounts, spec.Mounts) {
		return failf(CheckCredentialSeparation, "agent inspection reported a different mount topology")
	}
	if rep.SSH {
		return failf(CheckControlPlaneIsolation, "agent has SSH forwarding configured")
	}
	if len(rep.PublishedSockets) > 0 || len(rep.PublishedPorts) > 0 {
		return failf(CheckControlPlaneIsolation, "agent has a publication configured")
	}
	if !rep.NetworksObserved || !slices.Equal(rep.Networks, []string{spec.Network}) {
		return failf(CheckControlPlaneIsolation, "agent inspection reported a different network attachment")
	}
	return nil
}

func sameEnvironment(got, want []string) bool {
	gotByKey, gotOK := environmentByKey(got)
	wantByKey, wantOK := environmentByKey(want)
	if !gotOK || !wantOK || len(gotByKey) != len(wantByKey) {
		return false
	}
	for key, value := range wantByKey {
		if gotByKey[key] != value {
			return false
		}
	}
	return true
}

func environmentByKey(entries []string) (map[string]string, bool) {
	result := make(map[string]string, len(entries))
	for _, entry := range entries {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			return nil, false
		}
		if _, duplicate := result[key]; duplicate {
			return nil, false
		}
		result[key] = entry
	}
	return result, true
}

// sameImage reports whether an observed runtime image reference names the same
// digest-pinned image as the expected (spec) reference. Trust binds to the
// @sha256 digest; the name is compared tag-normalized as defense in depth. It
// fails closed unless both references carry a digest, so a tagless observed
// reference (no pin) never matches. The expected reference is guaranteed
// digest-pinned upstream (HandoffSpec/Config validation), but parsing it here
// too keeps the comparison self-contained and symmetric.
func sameImage(expected, observed string) bool {
	expName, expDigest, expOK := splitImageRef(expected)
	obsName, obsDigest, obsOK := splitImageRef(observed)
	return expOK && obsOK && expName == obsName && expDigest == obsDigest
}

func sameMounts(got, want []Mount) bool {
	if len(got) != len(want) {
		return false
	}
	counts := make(map[Mount]int, len(want))
	for _, m := range want {
		counts[m]++
	}
	for _, m := range got {
		if counts[m] == 0 {
			return false
		}
		counts[m]--
	}
	return true
}

// verifyExporterAllowlist is check 4: the exporter's runtime-observed
// configuration, inspected before it ever executes, must match the generated
// allowlist exactly — one persistent mount (the expected workspace volume,
// read-only, at the expected target), no credential volume, no host bind, no
// SSH forwarding, no environment beyond the image PATH, and no published
// socket, port, or network attachment. It verifies the runtime's report, not
// the gate's own spec:
// what the VM would actually get, not what was asked for.
func verifyExporterAllowlist(cfg Config, rep InspectReport, exporterID, workspaceVolume string) error {
	if rep.ID != exporterID {
		return failf(CheckExporterAllowlist, "exporter inspection identified the wrong container")
	}
	if !rep.AllowlistFieldsObserved {
		return failf(CheckExporterAllowlist, "exporter inspection omitted required configuration")
	}
	if !sameImage(cfg.ExporterImage, rep.ImageReference) {
		return failf(CheckExporterAllowlist, "exporter inspection reported the wrong image")
	}
	if !slices.Equal(rep.Command, cfg.ExporterCommand) {
		return failf(CheckExporterAllowlist, "exporter inspection reported the wrong command")
	}
	if rep.WorkingDirectory != "/" {
		return failf(CheckExporterAllowlist, "exporter working directory is not the fixed image root")
	}
	// Inspect-before-execution: the report must describe a container that has
	// not run. On the reference runtime a created-not-started container is
	// stopped; any other observed state means the trusted payload may already
	// have executed before the gate approved its configuration.
	if rep.State != StateStopped {
		return failf(CheckExporterAllowlist, "exporter was not observed stopped before execution")
	}
	if n := len(rep.Mounts); n != 1 {
		return failf(CheckExporterAllowlist, "exporter does not carry exactly one persistent mount")
	}
	m := rep.Mounts[0]
	switch {
	case m.AccessConflict:
		return failf(CheckExporterAllowlist, "exporter workspace mount reports contradictory ro/rw access")
	case m.Type != MountVolume:
		return failf(CheckExporterAllowlist, "exporter persistent mount is not a volume")
	case m.Source != workspaceVolume:
		return failf(CheckExporterAllowlist, "exporter mounts the wrong volume")
	case m.Target != cfg.WorkspaceTarget:
		return failf(CheckExporterAllowlist, "exporter mounts the workspace at the wrong target")
	case !m.ReadOnly:
		return failf(CheckExporterAllowlist, "exporter workspace mount is not read-only")
	}
	if rep.SSH {
		return failf(CheckExporterAllowlist, "exporter has SSH forwarding configured")
	}
	if n := len(rep.PublishedSockets); n > 0 {
		return failf(CheckExporterAllowlist, "exporter publishes %d sockets, want 0", n)
	}
	if n := len(rep.PublishedPorts); n > 0 {
		return failf(CheckExporterAllowlist, "exporter publishes %d ports, want 0", n)
	}
	if !rep.NetworksObserved {
		return failf(CheckExporterAllowlist, "exporter inspection omitted network attachments")
	}
	if rep.NetworkAttachmentCount != 0 {
		return failf(CheckExporterAllowlist, "exporter has %d network attachments, want 0", rep.NetworkAttachmentCount)
	}
	if !sameEnvironment(rep.Env, []string{fixedContainerPathEnv}) {
		return failf(CheckExporterAllowlist, "exporter environment does not match the fixed PATH allowlist")
	}
	return nil
}

// verifySeedRoleAllowlist approves one seeding-role container (the seeder or
// the observer) from its runtime-observed configuration, before it executes.
//
// It is deliberately a separate function rather than a generalization of
// verifyExporterAllowlist. That verifier is check 4's frozen contract surface,
// proven on the reference runtime and pinned by a golden; refactoring it into a
// shared body would put regression risk on it to save duplication that costs
// nothing. The two are expected to read almost identically, and any divergence
// between them should be a deliberate, reviewed one.
//
// The single difference from the exporter's rules is the workspace mount's
// access: the seeder holds it read-write to place the checkout, the observer
// read-only to attest it. Everything else is the same allowlist, because these
// containers run in the same credential-free, network-free position.
func verifySeedRoleAllowlist(cfg Config, rep InspectReport, spec ContainerSpec, workspaceVolume string, c Check) error {
	if len(spec.Mounts) != 1 {
		// The gate generates these specs, so a caller cannot reach this. It is
		// asserted rather than assumed so the mount indexing below is total.
		return failf(c, "seeding container spec does not carry exactly one mount")
	}
	if rep.ID != spec.Name {
		return failf(c, "seeding container inspection identified the wrong container")
	}
	if !rep.AllowlistFieldsObserved {
		return failf(c, "seeding container inspection omitted required configuration")
	}
	if !sameImage(spec.Image, rep.ImageReference) {
		return failf(c, "seeding container inspection reported the wrong image")
	}
	if !slices.Equal(rep.Command, spec.Command) {
		return failf(c, "seeding container inspection reported the wrong command")
	}
	if rep.WorkingDirectory != "/" {
		return failf(c, "seeding container working directory is not the fixed image root")
	}
	// Inspect-before-execution, as for the agent and exporter: a container
	// observed in any other state may already have run before the gate approved
	// what it would run as.
	if rep.State != StateStopped {
		return failf(c, "seeding container was not observed stopped before execution")
	}
	if n := len(rep.Mounts); n != 1 {
		return failf(c, "seeding container does not carry exactly one persistent mount")
	}
	m := rep.Mounts[0]
	switch {
	case m.AccessConflict:
		return failf(c, "seeding container workspace mount reports contradictory ro/rw access")
	case m.Type != MountVolume:
		return failf(c, "seeding container persistent mount is not a volume")
	case m.Source != workspaceVolume:
		return failf(c, "seeding container mounts the wrong volume")
	case m.Target != cfg.WorkspaceTarget:
		return failf(c, "seeding container mounts the workspace at the wrong target")
	case m.ReadOnly != spec.Mounts[0].ReadOnly:
		return failf(c, "seeding container workspace mount access does not match its approved access")
	}
	if rep.SSH {
		return failf(c, "seeding container has SSH forwarding configured")
	}
	if n := len(rep.PublishedSockets); n > 0 {
		return failf(c, "seeding container publishes %d sockets, want 0", n)
	}
	if n := len(rep.PublishedPorts); n > 0 {
		return failf(c, "seeding container publishes %d ports, want 0", n)
	}
	if !rep.NetworksObserved {
		return failf(c, "seeding container inspection omitted network attachments")
	}
	if rep.NetworkAttachmentCount != 0 {
		return failf(c, "seeding container has %d network attachments, want 0", rep.NetworkAttachmentCount)
	}
	if !sameEnvironment(rep.Env, []string{fixedContainerPathEnv}) {
		return failf(c, "seeding container environment does not match the fixed PATH allowlist")
	}
	return nil
}

// verifyBaseProof reads the observer's proof file and returns the base the
// workspace was observed to hold.
//
// It is modeled on verifyProof and holds the same line: the proof comes out of
// an archive nothing has scanned, so its content is parsed strictly and never
// echoed. Every required key must appear exactly once, no unknown key is
// tolerated, the fixed-value keys must carry their fixed values, and the SHA
// must be a full lowercase commit. A proof that omits a key is rejected rather
// than defaulted, so a truncated write cannot pass as a partial observation.
//
// The nonce must equal this invocation's unpredictable ownership token. That is
// what makes the proof this run's rather than a file the image shipped with or
// an earlier run left behind.
func verifyBaseProof(data []byte, nonce, treeDigest string) (string, error) {
	fixed := map[string]string{
		baseProofNonceKey:    nonce,
		baseProofGitDirKey:   "present",
		baseProofDetachedKey: "yes",
		// The observer hashes raw worktree bytes without clean filters and
		// compares those blob IDs and executable modes directly with HEAD. A
		// legitimate empty commit is clean; an unmaterialized non-empty
		// checkout is dirty.
		baseProofWorktreeKey: "clean",
		// Replacement refs and legacy grafts change the object graph Git shows
		// the writer. The observer disables them while proving HEAD and also
		// requires that the copied repository carries none.
		baseProofReplacementsKey: "absent",
		// The host refuses symlinks and every other non-regular kind in the
		// source; this is that refusal observed on the volume, so a source
		// mutated between the walk and the copy cannot slip one past both.
		baseProofIrregularKey: "absent",
		// The tree the host verified, recomputed by the observer over the
		// volume. HEAD is a pointer and proves nothing about content; this is
		// what makes the attestation cover what the writer will actually see.
		baseProofTreeKey: treeDigest,
	}
	seen := map[string]bool{}
	var observed string
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return "", failf(CheckObservedBaseIdentity, "base proof carries a line that is not key=value")
		}
		if seen[key] {
			return "", failf(CheckObservedBaseIdentity, "base proof repeats a required key")
		}
		seen[key] = true
		if key == baseProofSHAKey {
			if !commitSHAPattern.MatchString(value) {
				return "", failf(CheckObservedBaseIdentity, "base proof reports a value that is not a full lowercase commit SHA")
			}
			observed = value
			continue
		}
		want, known := fixed[key]
		if !known {
			return "", failf(CheckObservedBaseIdentity, "base proof carries an unknown key")
		}
		if value != want {
			// Deliberately categorical, including for the nonce: naming which
			// key mismatched would confirm a guessed token to whoever produced
			// the file.
			return "", failf(CheckObservedBaseIdentity, "base proof reports an unexpected value for a required key")
		}
	}
	if err := sc.Err(); err != nil {
		return "", failf(CheckObservedBaseIdentity, "base proof unreadable")
	}
	for _, key := range []string{
		baseProofNonceKey, baseProofGitDirKey, baseProofDetachedKey,
		baseProofSHAKey, baseProofWorktreeKey, baseProofReplacementsKey,
		baseProofIrregularKey, baseProofTreeKey,
	} {
		if !seen[key] {
			return "", failf(CheckObservedBaseIdentity, "base proof omits a required key")
		}
	}
	return observed, nil
}

// requiredProof is check 5's contract with the conformance probe
// (probeInExporterVerification): the probe runs inside the exporter image and
// its observations land as exactly these key=value lines in the proof file.
// Check 5 is attested by that dedicated probe, not the per-handoff exporter,
// which now runs only the trusted helper. These expected markers are trusted,
// but observed proof content comes from the unscanned archive and is never
// echoed in failure reasons.
var requiredProof = map[string]string{
	// /proc/mounts reports the workspace mounted read-only.
	"workspace_mounted": "ro",
	// A write probe into the workspace failed.
	"workspace_write": "blocked",
	// The expected credential path does not exist in the exporter.
	"credentials": "absent",
	// No host home directory is visible in the exporter.
	"host_home": "absent",
}

// verifyProof is check 5: the proof file collected from the probe's exported
// rootfs must contain exactly the required observations — every required key,
// the required value, no duplicates, no unknown keys, nothing else. Any
// deviation, including a missing or empty file, is a conformance failure; the
// probe's exit status is not consulted because a stopped container exports
// regardless of how its command exited.
func verifyProof(data []byte) error {
	seen := make(map[string]bool, len(requiredProof))
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return failf(CheckInExporterVerification, "proof carries a line that is not key=value")
		}
		want, known := requiredProof[key]
		if !known {
			return failf(CheckInExporterVerification, "proof carries an unknown key")
		}
		if seen[key] {
			return failf(CheckInExporterVerification, "proof repeats a required key")
		}
		seen[key] = true
		if value != want {
			return failf(CheckInExporterVerification, "proof reports an unexpected value for a required key")
		}
	}
	if err := sc.Err(); err != nil {
		return failf(CheckInExporterVerification, "proof unreadable")
	}
	keys := make([]string, 0, len(requiredProof))
	for key := range requiredProof {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if !seen[key] {
			return failf(CheckInExporterVerification, "proof is missing a required key")
		}
	}
	return nil
}
