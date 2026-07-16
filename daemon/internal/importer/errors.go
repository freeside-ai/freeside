package importer

import "errors"

// Sentinel fail-closed errors. Validators wrap these with %w and
// context, so callers match a class with errors.Is without string
// comparison. Each names an integrity invariant; policy outcomes are
// Findings on the Result, never errors.
var (
	// Manifest intake failures.
	ErrManifestUnreadable = errors.New("manifest cannot be read")
	ErrManifestInvalid    = errors.New("manifest failed validation")
	ErrManifestTooLarge   = errors.New("manifest exceeds an import cap")

	// Structural path-gate failures. An honest exporter cannot produce
	// either shape (a real filesystem cannot hold them), so both are
	// manifest forgery, not policy findings.
	ErrGitPathInjection = errors.New("path smuggles a git-metadata component")
	ErrPathConflict     = errors.New("one path is both a file and a directory")

	// Import invocation failures.
	ErrInvalidOptions = errors.New("import options are invalid")
)
