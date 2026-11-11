package main

import (
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// credentialIdentityLabel is the label auth list shows for an identity.
func credentialIdentityLabel(identity domain.AuthIdentity) string {
	return maskAccountBinding(identity.AccountBinding)
}
