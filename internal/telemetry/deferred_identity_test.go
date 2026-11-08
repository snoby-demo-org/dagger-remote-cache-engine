package telemetry

import (
	"context"
	"github.com/dagger/dagger/dagql"
	"github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestDeferredUpdatesKeepEveryContentIdentity(t *testing.T) {
	ctx := context.Background()
	cache, ing, _ := newIngestor(t)
	key := holderKey("cache-a", 7)
	ing.deferUpdate(key, dagql.RemoteHoldingUpdate{Replacements: 2, ContentDigest: digest.Digest("sha256:older"), Parts: []dagql.PersistedPartAddress{{Part: "stale"}}})
	ing.deferUpdate(key, dagql.RemoteHoldingUpdate{Replacements: 3, ContentDigest: digest.Digest("sha256:newer"), Parts: []dagql.PersistedPartAddress{{Part: "fresh"}}})
	require.NoError(t, ing.span(ctx, "cache-a", 1, "S", callSpan("call", ordinarySpanAttrs("7")...)))
	for _, d := range []string{"sha256:abc", "sha256:older", "sha256:newer"} {
		require.Contains(t, cache.EquivalentHolders(d), key, "lost observed content identity %s", d)
	}
	info, ok := cache.RemoteHoldingInfo(key)
	require.True(t, ok)
	require.Equal(t, uint64(3), info.Holding.Replacements)
	require.Equal(t, []dagql.PersistedPartAddress{{Part: "fresh"}}, info.Holding.Parts)
}
