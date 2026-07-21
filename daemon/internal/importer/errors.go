package importer

import "errors"

// Sentinel fail-closed errors. Validators wrap these with %w and
// context, so callers match a class with errors.Is without string
// comparison. Each names an integrity invariant; policy outcomes are
// Findings on the Result, never errors.
var (
	// Manifest intake failures. ErrManifestTooLarge covers both channels'
	// intake caps (manifest bytes, entry count, and stored-blob size).
	ErrManifestUnreadable = errors.New("manifest cannot be read")
	ErrManifestInvalid    = errors.New("manifest failed validation")
	ErrManifestTooLarge   = errors.New("manifest exceeds an import cap")

	// Evidence-channel intake failures (the second §5.6 workspace-exit
	// channel). Evidence is optional, but a present evidence.json is untrusted:
	// a malformed manifest, a forged producer class or provenance, or an
	// injected trust field fails the import closed the same way the repo
	// channel does. ErrEvidenceMediaMismatch covers an unlisted or forged
	// media_type and content that does not match its declared type (§5.15
	// rule 3). The shared blob-store sentinels below (ErrMissingBlob,
	// ErrOrphanBlob, ErrDigestMismatch, ErrSizeMismatch, ErrBlobTooLarge) name
	// the invariant, not the channel, so both channels reuse them.
	ErrEvidenceUnreadable    = errors.New("evidence manifest cannot be read")
	ErrEvidenceInvalid       = errors.New("evidence manifest failed validation")
	ErrEvidenceMediaMismatch = errors.New("evidence blob does not match its declared media type")
	ErrCommitPlanUnreadable  = errors.New("commit plan cannot be read")
	ErrCommitPlanCollision   = errors.New("reserved commit-plan namespace collision")

	// Structural path-gate failures. An honest exporter cannot produce
	// either shape (a real filesystem cannot hold them), so both are
	// manifest forgery, not policy findings.
	ErrGitPathInjection = errors.New("path smuggles a git-metadata component")
	ErrPathConflict     = errors.New("one path is both a file and a directory")

	// Blob-store failures. The audit is exact in both directions;
	// content binding is bytes-verified, never trusted from the
	// manifest.
	ErrHandoffUnreadable = errors.New("handoff content cannot be read")
	ErrMissingBlob       = errors.New("manifest references a blob the handoff does not hold")
	ErrOrphanBlob        = errors.New("handoff holds content the manifest does not reference")
	ErrDigestMismatch    = errors.New("blob content does not match its manifest digest")
	ErrSizeMismatch      = errors.New("blob size does not match its manifest size")
	ErrBlobTooLarge      = errors.New("manifest declares a stored blob beyond the import size cap")

	// Checkout and construction failures.
	ErrBaseMismatch    = errors.New("checkout is not at the enforced base commit")
	ErrUnsupportedRepo = errors.New("checkout repository is not supported")
	ErrGitPlumbing     = errors.New("git plumbing failed")
	ErrTreeMismatch    = errors.New("built tree does not match the derived change set")

	// Import invocation failures.
	ErrInvalidOptions = errors.New("import options are invalid")
)
