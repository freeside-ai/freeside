package inference

import (
	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// RolePrompt is a judgment role's refinable prompt: the name and content
// digest a lineup line records for it, and the bytes a call sends. It is the
// part of a call's instructions an operator can change without changing what
// the site asks for; the site's own instruction is the site contract
// (Site.ContractDigest) and is recorded apart from it.
type RolePrompt struct {
	Name   string
	Digest domain.Digest
	Body   []byte
}

// codeOwnedRolePromptVersion suffixes a role's name to give its code-owned
// prompt name. Bump it when a code-owned body changes; the lineup lines that
// name the old version then stop resolving until they are re-adopted.
const codeOwnedRolePromptVersion = "_v1"

// CodeOwnedRolePrompt returns the prompt of a judgment role whose refinable
// prompt the daemon owns, as ward.ProductionReviewPromptIdentity does for
// review. Every such role's refinable prompt is empty today: all of its
// instruction is its site contract. The identity is therefore a versioned
// name over the digest of an empty body, which is an honest statement that
// nothing refinable was sent. The publication author is not code-owned (its
// prompt is the operator's file, OperatorRolePrompt), and a role with no
// built site has no prompt. #1428 moves these to prompts/ files.
func CodeOwnedRolePrompt(role domain.RoleName) (RolePrompt, bool) {
	if len(role.Sites()) == 0 || role == domain.RolePublicationAuthor {
		return RolePrompt{}, false
	}
	return RolePrompt{
		Name:   string(role) + codeOwnedRolePromptVersion,
		Digest: domain.Digest(contentaddr.Sum(nil)),
	}, true
}

// OperatorRolePrompt returns the prompt of a role whose refinable prompt is a
// file the operator configures: the role's name, as the ward roles' prompt
// packages are named, over the content digest of the file's bytes.
func OperatorRolePrompt(role domain.RoleName, body []byte) RolePrompt {
	return RolePrompt{
		Name: string(role), Digest: domain.Digest(contentaddr.Sum(body)), Body: body,
	}
}
