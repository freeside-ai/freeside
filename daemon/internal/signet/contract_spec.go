package signet

import (
	_ "embed"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

//go:embed openapi.yaml
var embeddedOpenAPISpec []byte

// APIContractDigest identifies the api/openapi.yaml this daemon was built
// from: "sha256:" plus the lowercase hex SHA-256 of the spec's exact bytes.
// It is computed once from the embedded copy of api/openapi.yaml rather than
// committed as a constant, so unrelated spec edits do not collide on a hash
// line during rebase. The daemon reports it on GET /health to name its
// contract.
var APIContractDigest = contentaddr.Sum(embeddedOpenAPISpec)
