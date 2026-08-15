package main

import (
	"context"

	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/topicstore"
)

const topicKeySuffix = topicstore.KeySuffix

var (
	errTopicKeyPermissions = topicstore.ErrKeyPermissions
	errTopicKeyMalformed   = topicstore.ErrKeyMalformed
	errTopicKeyMissing     = topicstore.ErrKeyMissing
)

func openStoreWithTopicKey(
	ctx context.Context, dbPath string, opts store.Options,
) (*store.Store, []byte, error) {
	return topicstore.Open(ctx, dbPath, opts)
}

func loadOrCreateTopicKey(dbPath string, storePreexisting bool) ([]byte, error) {
	return topicstore.LoadOrCreateKey(dbPath, storePreexisting)
}
