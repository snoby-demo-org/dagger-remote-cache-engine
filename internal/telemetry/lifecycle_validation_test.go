package telemetry

import (
	"context"
	"testing"

	"github.com/dagger/dagger/dagql"
	"github.com/dagger/dagger/engine/telemetryattrs"
	"github.com/stretchr/testify/require"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
)

func holderKey(cache string, num uint64) dagql.HolderKey {
	return dagql.HolderKey{Cache: dagql.CacheID(cache), Number: num}
}

func TestRetainedOrdinarySpanSurvivesSessionEnd(t *testing.T) {
	ctx := context.Background()
	cache, ing, _ := newIngestor(t)
	attrs := append(ordinarySpanAttrs("7"), str(telemetryattrs.CacheRetainedAttr, "true"))
	require.NoError(t, ing.span(ctx, "cache-a", 1, "S", callSpan("call", attrs...)))
	require.NoError(t, ing.sessionEnd(ctx, "cache-a", "S"))
	info, ok := cache.RemoteHoldingInfo(holderKey("cache-a", 7))
	require.True(t, ok, "retained holding must survive session end")
	require.Zero(t, info.Holding.RetentionExpiresAtUnix, "retained without expiry must be infinite")
}

func TestLazyPartsDroppedWhenOrdinaryReplaces(t *testing.T) {
	ctx := context.Background()
	cache, ing, _ := newIngestor(t)
	lazy := []*commonpb.KeyValue{
		str(telemetryattrs.CacheContractAttr, telemetryattrs.CacheContractV1),
		str(telemetryattrs.CacheResultIDAttr, "7"),
		str(telemetryattrs.CacheReplacementsAttr, "1"),
		strs(telemetryattrs.CachePartsAttr, `{"part":"stale"}`),
	}
	require.NoError(t, ing.span(ctx, "cache-a", 1, "S", callSpan("lazy", lazy...)))
	attrs := append(ordinarySpanAttrs("7"), str(telemetryattrs.CacheReplacementsAttr, "2"))
	require.NoError(t, ing.span(ctx, "cache-a", 1, "S", callSpan("ordinary", attrs...)))
	info, ok := cache.RemoteHoldingInfo(holderKey("cache-a", 7))
	require.True(t, ok)
	for _, p := range info.Holding.Parts {
		require.NotEqual(t, dagql.PartKey("stale"), p.Part, "stale lazy part must not survive")
	}
}

func TestHigherReplacementResetsPartsAndExpiry(t *testing.T) {
	ctx := context.Background()
	cache, ing, _ := newIngestor(t)
	lazy := []*commonpb.KeyValue{
		str(telemetryattrs.CacheContractAttr, telemetryattrs.CacheContractV1),
		str(telemetryattrs.CacheResultIDAttr, "7"),
		str(telemetryattrs.CacheReplacementsAttr, "2"),
		strs(telemetryattrs.CachePartsAttr, `{"part":"fresh"}`),
	}
	require.NoError(t, ing.span(ctx, "cache-a", 1, "S", callSpan("lazy", lazy...)))
	attrs := append(ordinarySpanAttrs("7"), str(telemetryattrs.CacheReplacementsAttr, "1"))
	require.NoError(t, ing.span(ctx, "cache-a", 1, "S", callSpan("ordinary", attrs...)))
	info, ok := cache.RemoteHoldingInfo(holderKey("cache-a", 7))
	require.True(t, ok)
	require.Equal(t, uint64(2), info.Holding.Replacements)
	require.Len(t, info.Holding.Parts, 1)
	require.Equal(t, dagql.PartKey("fresh"), info.Holding.Parts[0].Part)
	require.Zero(t, info.Holding.ExpiresAtUnix)
}

func TestMalformedReplacementsError(t *testing.T) {
	ctx := context.Background()
	cache, ing, _ := newIngestor(t)
	attrs := append(ordinarySpanAttrs("7"), str(telemetryattrs.CacheReplacementsAttr, "not-a-number"))
	require.Error(t, ing.span(ctx, "cache-a", 1, "S", callSpan("bad", attrs...)))
	lazy := []*commonpb.KeyValue{
		str(telemetryattrs.CacheContractAttr, telemetryattrs.CacheContractV1),
		str(telemetryattrs.CacheResultIDAttr, "7"),
		str(telemetryattrs.CacheReplacementsAttr, "-1"),
	}
	require.Error(t, ing.span(ctx, "cache-b", 1, "S", callSpan("badlazy", lazy...)))
	_, ok := cache.RemoteHoldingInfo(holderKey("cache-a", 7))
	require.False(t, ok)
	_, ok = cache.RemoteHoldingInfo(holderKey("cache-b", 7))
	require.False(t, ok)
}

func TestEngineStartWipeRemovesOldCacheAndSessions(t *testing.T) {
	ctx := context.Background()
	cache, ing, _ := newIngestor(t)
	attrs := append(ordinarySpanAttrs("7"), str(telemetryattrs.CacheRetainedAttr, "true"))
	require.NoError(t, ing.span(ctx, "cache-a", 1, "S", callSpan("call", attrs...)))
	_, ok := cache.RemoteHoldingInfo(holderKey("cache-a", 7))
	require.True(t, ok)
	require.NoError(t, ing.Logs(ctx, logsReq(engineResource("cache-new"), telemetryattrs.EngineEventScope, eventLog(telemetryattrs.EngineEventStart, `{"engineVersion":"v1.0.0-beta.25","wipedCache":"cache-a"}`))))
	_, ok = cache.RemoteHoldingInfo(holderKey("cache-a", 7))
	require.False(t, ok, "prune must remove holding")
	require.NotContains(t, ing.sessions, "cache-a", "prune must remove sessions entry")
}
