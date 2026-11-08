package telemetry

import (
	"context"
	"testing"

	"github.com/dagger/dagger/engine/telemetryattrs"
	"github.com/stretchr/testify/require"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
)

// Pinned dagql/otelprof_lazy.go:endOTelLazyOp stamps result ID and parts,
// but does not stamp cache.contract or cache.outcome.
func TestContractlessLazyCarrierUpdatesKnownHolding(t *testing.T) {
	ctx := context.Background()
	_, ing, events := newIngestor(t)
	require.NoError(t, ing.span(ctx, "cache-a", 1, "S", callSpan("ordinary", ordinarySpanAttrs("7")...)))
	attrs := []*commonpb.KeyValue{
		str(telemetryattrs.CacheResultIDAttr, "7"),
		strs(telemetryattrs.CachePartsAttr, `{"part":"fs"}`),
	}
	require.NoError(t, ing.span(ctx, "cache-a", 1, "S", callSpan("lazy", attrs...)))
	got := events()
	require.Len(t, got, 2, "the actual contractless lazy carrier was discarded")
	require.Len(t, got[1].Parts, 1)
	require.Equal(t, "fs", string(got[1].Parts[0].Part))
	require.Equal(t, "xxh3:recipe1", got[1].Recipe)
}

func TestOutcomeStillRequiresSupportedContract(t *testing.T) {
	_, ing, events := newIngestor(t)
	var attrs []*commonpb.KeyValue
	for _, a := range ordinarySpanAttrs("7") {
		if a.Key != telemetryattrs.CacheContractAttr {
			attrs = append(attrs, a)
		}
	}
	require.NoError(t, ing.span(context.Background(), "cache-a", 1, "S", callSpan("ordinary", attrs...)))
	require.Empty(t, events())
}
