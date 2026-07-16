package importer

// FindingKind classifies one publish-blocking policy finding. The zero
// value "" is invalid by design.
type FindingKind string

const (
	// FindingNonRegularChange records a candidate adding, modifying, or
	// removing a non-regular entry (symlink, submodule, special file,
	// unusual mode). §5.6 names the whole class publish-blocking, and a
	// git tree cannot faithfully hold it, so it also blocks the commit.
	FindingNonRegularChange FindingKind = "non_regular_change"
	// FindingInvalidPathEntry records a manifest entry whose name is not
	// representable as a canonical UTF-8 path (the export contract's
	// invalid_path kind, raw bytes preserved in path_hex).
	FindingInvalidPathEntry FindingKind = "invalid_path_entry"
	// FindingBlobOmitted records a changed regular entry whose content
	// blob the exporter's caps omitted, so its content cannot enter the
	// tree (the contract's contained failure direction for a workspace
	// built to exceed the caps).
	FindingBlobOmitted FindingKind = "blob_omitted"
	// FindingAutomationControlPath records a change touching an
	// automation-control path (§5.5): CI executes those with implicit
	// GITHUB_TOKEN and OIDC authority even when no secret is named.
	FindingAutomationControlPath FindingKind = "automation_control_path"
	// FindingReviewerInstructionPath records a change touching a
	// reviewer-instruction path (§5.8): auto-review is not independent
	// review for a PR that modifies the instructions governing it.
	FindingReviewerInstructionPath FindingKind = "reviewer_instruction_path"
	// FindingGitMetadataPath records a change touching git metadata that
	// steers downstream checkout, diff, or submodule behaviour
	// (.gitmodules, .gitattributes).
	FindingGitMetadataPath FindingKind = "git_metadata_path"
	// FindingAllowlistViolation records a change outside the work unit's
	// declared path allowlist.
	FindingAllowlistViolation FindingKind = "allowlist_violation"
	// FindingSizeViolation records a changed file or change set
	// exceeding the import size policy.
	FindingSizeViolation FindingKind = "size_violation"
	// FindingPathCollision records candidate-introduced paths that
	// collide under case folding or Unicode normalization on a case- or
	// normalization-insensitive checkout (APFS, the reference
	// deployment).
	FindingPathCollision FindingKind = "path_collision"
	// FindingSecret records a best-effort secret-scan match (§5.4): a
	// high-signal token pattern in supported textual content.
	FindingSecret FindingKind = "secret"
)

// AllFindingKinds lists every valid FindingKind: the single place a new
// kind is registered, driving the table-driven tests.
var AllFindingKinds = []FindingKind{
	FindingNonRegularChange,
	FindingInvalidPathEntry,
	FindingBlobOmitted,
	FindingAutomationControlPath,
	FindingReviewerInstructionPath,
	FindingGitMetadataPath,
	FindingAllowlistViolation,
	FindingSizeViolation,
	FindingPathCollision,
	FindingSecret,
}

// valid is the validity predicate; as a predicate it uses default.
func (k FindingKind) valid() bool {
	switch k {
	case FindingNonRegularChange, FindingInvalidPathEntry,
		FindingBlobOmitted, FindingAutomationControlPath,
		FindingReviewerInstructionPath, FindingGitMetadataPath,
		FindingAllowlistViolation, FindingSizeViolation,
		FindingPathCollision, FindingSecret:
		return true
	default:
		return false
	}
}

// blocksCommit reports whether a finding of this kind withholds commit
// construction because the tree cannot faithfully represent the
// candidate. Policy-only findings return false: the commit exists for
// the §5.5 control-plane route, and the publication gate consumes the
// findings. The switch omits default so a new FindingKind must decide
// its blocking class; the trailing return covers the invalid zero
// value.
func (k FindingKind) blocksCommit() bool {
	switch k {
	case FindingNonRegularChange, FindingInvalidPathEntry, FindingBlobOmitted:
		return true
	case FindingAutomationControlPath, FindingReviewerInstructionPath,
		FindingGitMetadataPath, FindingAllowlistViolation,
		FindingSizeViolation, FindingPathCollision, FindingSecret:
		return false
	}
	return false
}

// Finding is one publish-blocking policy finding. Path names the
// affected canonical path; PathHex carries raw name bytes for
// invalid_path entries (the two are mutually exclusive, as in the
// manifest). Rule and Line locate secret findings by rule id and
// 1-based line. Detail is daemon-authored context; no field ever
// carries candidate content bytes.
type Finding struct {
	Path    string      `json:"path,omitempty"`
	PathHex string      `json:"path_hex,omitempty"`
	Kind    FindingKind `json:"kind"`
	Rule    string      `json:"rule,omitempty"`
	Line    int         `json:"line,omitempty"`
	Detail  string      `json:"detail,omitempty"`
}
