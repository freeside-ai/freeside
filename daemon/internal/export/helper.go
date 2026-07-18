package export

// The fixed helper-in-image interface (plan §5.6): freeside-export ships
// inside ward's digest-pinned exporter image at a contracted path and is
// invoked with a daemon-owned fixed command. These constants are the single
// declaration point both consuming lanes bind to; ward (image build and
// conformance, #170) and gauntlet (import, #167) reference them and never
// restate the literals, so the two lanes cannot drift apart on the shipped
// interface.
const (
	// HelperPath is the contracted location of the static freeside-export
	// binary inside the exporter image.
	HelperPath = "/usr/local/bin/freeside-export"
	// HelperWorkspaceDir is the read-only mount point of the agent workspace
	// inside the exporter context, and the helper's -workspace default.
	HelperWorkspaceDir = "/workspace"
	// HelperHandoffDir is the writable handoff mount the helper emits the
	// manifests and blobs into, and the helper's -out default.
	HelperHandoffDir = "/handoff"
)

// HelperCommand returns the daemon-owned fixed argv that runs the trusted
// export helper against the contracted mounts. It allocates a fresh slice on
// every call so no caller can mutate another's view of the fixed command.
func HelperCommand() []string {
	return []string{HelperPath, "-workspace", HelperWorkspaceDir, "-out", HelperHandoffDir}
}
