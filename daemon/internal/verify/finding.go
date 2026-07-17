package verify

// FindingKind classifies one risk-flag finding. Verifier findings are
// identified and risk-flagged, never execution-blocking (§5.6): the
// publication gate downstream consumes them. The zero value "" is
// invalid by design.
type FindingKind string

// FindingRecipeDivergence records a candidate head whose in-tree copy
// of the recipe path differs from the trusted recipe that was actually
// executed (§5.8: workspace copies are data). The trusted source, not
// the candidate copy, always governs execution; the finding makes the
// attempted swap visible.
const FindingRecipeDivergence FindingKind = "recipe_divergence"

// FindingVerificationControlPath records a candidate change touching a
// verification-control path (§5.6): a file that steers what the
// recipe's commands actually execute or check (dependency pins, build
// entrypoints, lint config, the recipe path itself).
const FindingVerificationControlPath FindingKind = "verification_control_path"

// AllFindingKinds lists every valid FindingKind: the single place a new
// kind is registered, driving the table-driven tests.
var AllFindingKinds = []FindingKind{
	FindingRecipeDivergence,
	FindingVerificationControlPath,
}

// valid is the validity predicate; as a predicate it uses default.
func (k FindingKind) valid() bool {
	switch k {
	case FindingRecipeDivergence, FindingVerificationControlPath:
		return true
	default:
		return false
	}
}

// Finding is one risk-flag finding. Path names the affected canonical
// path; PathHex carries raw name bytes when the path is not
// representable as canonical UTF-8 (the two are mutually exclusive, as
// in the importer's account). Detail is daemon-authored context; no
// field ever carries candidate content bytes.
type Finding struct {
	Path    string      `json:"path,omitempty"`
	PathHex string      `json:"path_hex,omitempty"`
	Kind    FindingKind `json:"kind"`
	Detail  string      `json:"detail,omitempty"`
}
