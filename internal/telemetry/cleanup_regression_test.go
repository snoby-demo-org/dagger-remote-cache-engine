package telemetry

import (
	"context"
	"github.com/dagger/dagger/engine/telemetryattrs"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestPruneCollectsAfterEndedSession(t *testing.T) {
	ctx := context.Background()
	cache, ing, _ := newIngestor(t)
	attrs := append(ordinarySpanAttrs("7"), str(telemetryattrs.CacheRetainedAttr, "true"))
	require.NoError(t, ing.span(ctx, "cache-a", 1, "S", callSpan("call", attrs...)))
	require.NoError(t, ing.sessionEnd(ctx, "cache-a", "S"))
	_, ok := cache.RemoteHoldingInfo(holderKey("cache-a", 7))
	require.True(t, ok)
	require.NoError(t, ing.Logs(ctx, logsReq(engineResource("cache-a"), telemetryattrs.EngineEventScope, eventLog(telemetryattrs.EngineEventPrune, `{"drops":[{"resultID":7,"droppedAtUnixNano":100}]}`))))
	_, ok = cache.RemoteHoldingInfo(holderKey("cache-a", 7))
	require.False(t, ok)
	require.NotContains(t, ing.recipes, holderKey("cache-a", 7))
}

func TestEndCollectionClearsRecipes(t *testing.T) {
	for _, stop := range []bool{false, true} {
		t.Run(map[bool]string{false: "session", true: "process"}[stop], func(t *testing.T) {
			ctx := context.Background()
			cache, ing, _ := newIngestor(t)
			require.NoError(t, ing.span(ctx, "cache-a", 1, "S", callSpan("call", ordinarySpanAttrs("7")...)))
			if stop {
				require.NoError(t, ing.stop(ctx, "cache-a"))
			} else {
				require.NoError(t, ing.sessionEnd(ctx, "cache-a", "S"))
			}
			_, ok := cache.RemoteHoldingInfo(holderKey("cache-a", 7))
			require.False(t, ok)
			require.NotContains(t, ing.recipes, holderKey("cache-a", 7))
		})
	}
}
