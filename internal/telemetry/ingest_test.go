package telemetry

import (
	"context"
	"testing"

	"github.com/dagger/dagger/dagql"
	"github.com/dagger/dagger/dagql/call/callpbv1"
	"github.com/dagger/dagger/engine/telemetryattrs"
	"github.com/stretchr/testify/require"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

func str(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

func strs(k string, vs ...string) *commonpb.KeyValue {
	vals := make([]*commonpb.AnyValue, 0, len(vs))
	for _, v := range vs {
		vals = append(vals, &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}})
	}
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: vals}}}}
}

func engineResource(cacheID string) *resourcepb.Resource {
	return &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
		str(telemetryattrs.EngineCacheAttr, cacheID+"/1"),
		str("service.instance.id", "inst-1"),
	}}
}

func sessionResource(cacheID, session string) *resourcepb.Resource {
	return &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
		str(telemetryattrs.EngineCacheAttr, cacheID+"/1"),
		str(telemetryattrs.EngineSessionAttr, session),
		str("service.instance.id", "inst-1"),
	}}
}

func traceReq(res *resourcepb.Resource, spans ...*tracepb.Span) *coltracepb.ExportTraceServiceRequest {
	return &coltracepb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{
		Resource:   res,
		ScopeSpans: []*tracepb.ScopeSpans{{Spans: spans}},
	}}}
}

func callSpan(name string, attrs ...*commonpb.KeyValue) *tracepb.Span {
	return &tracepb.Span{Name: name, Attributes: attrs}
}

func eventLog(kind, body string) *logspb.LogRecord {
	return &logspb.LogRecord{
		Body:       &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: body}},
		Attributes: []*commonpb.KeyValue{str(telemetryattrs.EngineEventAttr, kind)},
	}
}

func logsReq(res *resourcepb.Resource, scope string, recs ...*logspb.LogRecord) *collogspb.ExportLogsServiceRequest {
	return &collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
		Resource:  res,
		ScopeLogs: []*logspb.ScopeLogs{{Scope: &commonpb.InstrumentationScope{Name: scope}, LogRecords: recs}},
	}}}
}

func newIngestor(t *testing.T) (*dagql.Cache, *Ingestor, func() []Event) {
	t.Helper()
	ctx := context.Background()
	cache, err := dagql.NewCache(ctx, "", nil, nil, dagql.WithBlobStore())
	require.NoError(t, err)
	var events []Event
	ing := New(cache, func(e Event) { events = append(events, e) })
	return cache, ing, func() []Event { return events }
}

// held reports whether the holding is currently held by session (and so exists).
// It restores every released key so later release/stop assertions remain valid.
func held(t *testing.T, cache *dagql.Cache, cacheID, session string, number uint64) bool {
	t.Helper()
	ctx := context.Background()
	keys := cache.ReleaseRemoteSession(ctx, dagql.CacheID(cacheID), session)
	found := false
	for _, k := range keys {
		if k.Number == number {
			found = true
		}
		require.NoError(t, cache.AddRemoteHold(ctx, k, session))
	}
	return found
}

// ordinarySpanAttrs is the complete snapshot shape of the captured
// Query.container span (agents/discovery-capture.json line 5).
func ordinarySpanAttrs(number string) []*commonpb.KeyValue {
	return []*commonpb.KeyValue{
		str(telemetryattrs.CacheContractAttr, telemetryattrs.CacheContractV1),
		str(telemetryattrs.CacheOutcomeAttr, telemetryattrs.CacheOutcomeExecuted),
		str(dagOutputAttr, "xxh3:recipe1"),
		str(dagDigestAttr, "xxh3:req1"),
		str(telemetryattrs.CacheSelfDigestAttr, "xxh3:self1"),
		strs(telemetryattrs.CacheStructuralInputsAttr, "xxh3:in1", "xxh3:in2"),
		str(telemetryattrs.CacheResultIDAttr, number),
		str(telemetryattrs.CacheTypeAttr, "Container"),
		strs(telemetryattrs.CacheDepsAttr, "3"),
		strs(telemetryattrs.CachePartsAttr, `{"part":"fs"}`, `{"outputPath":[{"field":"out"}],"part":"meta"}`),
		str(telemetryattrs.CacheExpiresAttr, "1234"),
		str(telemetryattrs.CacheOutputContentDigestAttr, "sha256:abc"),
	}
}

func TestOrdinarySpanAttachesAndEmitsCallback(t *testing.T) {
	ctx := context.Background()
	cache, ing, events := newIngestor(t)

	err := ing.Traces(ctx, traceReq(sessionResource("cache-a", "SESS1"),
		callSpan("Container.from", ordinarySpanAttrs("7")...)))
	require.NoError(t, err)

	got := events()
	require.Len(t, got, 1)
	ev := got[0]
	require.Equal(t, dagql.HolderKey{Cache: "cache-a", Number: 7}, ev.Key)
	require.Equal(t, "xxh3:recipe1", ev.Recipe)
	require.Equal(t, "SESS1", ev.Session)
	require.True(t, ev.Executed)
	require.Len(t, ev.Parts, 2)
	require.Equal(t, dagql.PersistedPartAddress{Part: "fs"}, ev.Parts[0])
	require.Equal(t, dagql.PersistedPartAddress{OutputPath: dagql.PersistedRefPath{{Field: "out"}}, Part: "meta"}, ev.Parts[1])

	// The holding exists and is held by the session; releasing it lets the
	// service collect it.
	require.True(t, held(t, cache, "cache-a", "SESS1", 7))
	keys := cache.ReleaseRemoteSession(ctx, "cache-a", "SESS1")
	collected, err := cache.CollectRemoteHoldings(ctx, keys)
	require.NoError(t, err)
	require.Len(t, collected, 1)
	require.Equal(t, uint64(7), collected[0].Number)
}

func TestLazyBeforeCallIsReplayedAfterAttach(t *testing.T) {
	ctx := context.Background()
	cache, ing, events := newIngestor(t)

	lazy := callSpan("Container.from",
		str(telemetryattrs.CacheContractAttr, telemetryattrs.CacheContractV1),
		str(telemetryattrs.CacheResultIDAttr, "7"),
		strs(telemetryattrs.CachePartsAttr, `{"part":"lazyPart"}`),
		strs(telemetryattrs.CacheDepsAttr, "3"),
	)
	require.NoError(t, ing.Traces(ctx, traceReq(sessionResource("cache-a", "SESS1"), lazy)))
	require.Empty(t, events(), "lazy span alone must not emit")
	require.False(t, held(t, cache, "cache-a", "SESS1", 7), "lazy span alone must not create a holding")

	require.NoError(t, ing.Traces(ctx, traceReq(sessionResource("cache-a", "SESS1"),
		callSpan("Container.from", ordinarySpanAttrs("7")...))))

	got := events()
	require.Len(t, got, 1, "the pending lazy update is merged into the attach event")
	require.True(t, got[0].Executed, "attach event is complete")
	require.Equal(t, "xxh3:recipe1", got[0].Recipe)
	require.Len(t, got[0].Parts, 3, "attach parts plus the replayed lazy part")
	require.Equal(t, dagql.PersistedPartAddress{Part: "fs"}, got[0].Parts[0])
	require.Equal(t, dagql.PersistedPartAddress{OutputPath: dagql.PersistedRefPath{{Field: "out"}}, Part: "meta"}, got[0].Parts[1])
	require.Equal(t, dagql.PersistedPartAddress{Part: "lazyPart"}, got[0].Parts[2])
	require.True(t, held(t, cache, "cache-a", "SESS1", 7))
}

func TestLazyAloneNeverCreates(t *testing.T) {
	ctx := context.Background()
	cache, ing, events := newIngestor(t)

	lazy := callSpan("Container.from",
		str(telemetryattrs.CacheContractAttr, telemetryattrs.CacheContractV1),
		str(telemetryattrs.CacheResultIDAttr, "11"),
		strs(telemetryattrs.CachePartsAttr, `{"part":"p"}`),
	)
	require.NoError(t, ing.Traces(ctx, traceReq(sessionResource("cache-a", "SESS1"), lazy)))
	require.Empty(t, events())
	require.False(t, held(t, cache, "cache-a", "SESS1", 11))
}

func TestShareAloneNeverCreates(t *testing.T) {
	ctx := context.Background()
	cache, ing, events := newIngestor(t)

	body := `{"parts":[{"resultId":9,"part":"{\"part\":\"snap\"}","deps":[1]}]}`
	err := ing.Logs(ctx, logsReq(engineResource("cache-b"), telemetryattrs.EngineEventScope,
		eventLog(telemetryattrs.EngineEventShare, body)))
	require.NoError(t, err)
	require.Empty(t, events())
	require.False(t, held(t, cache, "cache-b", "SESS1", 9))
}

func TestShareUpdatesExistingHolding(t *testing.T) {
	ctx := context.Background()
	_, ing, events := newIngestor(t)

	require.NoError(t, ing.Traces(ctx, traceReq(sessionResource("cache-b", "SESS1"),
		callSpan("Container.from", ordinarySpanAttrs("2")...))))
	require.Len(t, events(), 1)

	body := `{"parts":[{"resultId":2,"part":"{\"part\":\"snapshot\"}","deps":[3],"replacements":1}]}`
	require.NoError(t, ing.Logs(ctx, logsReq(engineResource("cache-b"), telemetryattrs.EngineEventScope,
		eventLog(telemetryattrs.EngineEventShare, body))))

	got := events()
	require.Len(t, got, 2)
	require.Equal(t, dagql.HolderKey{Cache: "cache-b", Number: 2}, got[1].Key)
	require.Equal(t, "xxh3:recipe1", got[1].Recipe)
	require.Len(t, got[1].Parts, 1)
	require.Equal(t, dagql.PersistedPartAddress{Part: "snapshot"}, got[1].Parts[0])
}

func TestUnsupportedContractIgnored(t *testing.T) {
	ctx := context.Background()
	cache, ing, events := newIngestor(t)

	attrs := ordinarySpanAttrs("7")
	attrs[0] = str(telemetryattrs.CacheContractAttr, "2")
	require.NoError(t, ing.Traces(ctx, traceReq(sessionResource("cache-a", "SESS1"), callSpan("Container.from", attrs...))))
	require.Empty(t, events())
	require.False(t, held(t, cache, "cache-a", "SESS1", 7))
}

func TestMalformedValuesRefused(t *testing.T) {
	ctx := context.Background()
	_, ing, events := newIngestor(t)

	cases := []struct {
		name  string
		attrs []*commonpb.KeyValue
	}{
		{"result id", ordinarySpanAttrs("7x")},
		{"parts", func() []*commonpb.KeyValue {
			attrs := ordinarySpanAttrs("7")
			for i, a := range attrs {
				if a.Key == telemetryattrs.CachePartsAttr {
					attrs[i] = strs(telemetryattrs.CachePartsAttr, "{bad json")
				}
			}
			return attrs
		}()},
		{"deps", func() []*commonpb.KeyValue {
			attrs := ordinarySpanAttrs("7")
			for i, a := range attrs {
				if a.Key == telemetryattrs.CacheDepsAttr {
					attrs[i] = strs(telemetryattrs.CacheDepsAttr, "x1")
				}
			}
			return attrs
		}()},
		{"recipe", func() []*commonpb.KeyValue {
			attrs := ordinarySpanAttrs("7")
			for i, a := range attrs {
				if a.Key == dagOutputAttr {
					attrs[i] = str(dagOutputAttr, "")
				}
			}
			return attrs
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Error(t, ing.Traces(ctx, traceReq(sessionResource("cache-a", "SESS1"), callSpan("Container.from", tc.attrs...))))
		})
	}
	require.Empty(t, events())

	t.Run("engine cache attr", func(t *testing.T) {
		res := &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
			str(telemetryattrs.EngineCacheAttr, "cache-a/notanumber"),
		}}
		require.Error(t, ing.Traces(ctx, traceReq(res, callSpan("Container.from", ordinarySpanAttrs("7")...))))
	})
}

func TestUint64Beyond2e53Preserved(t *testing.T) {
	ctx := context.Background()
	cache, ing, events := newIngestor(t)

	const big = uint64(9007199254740993) // 2^53 + 1
	require.NoError(t, ing.Traces(ctx, traceReq(sessionResource("cache-a", "SESS1"),
		callSpan("Container.from", ordinarySpanAttrs("9007199254740993")...))))

	got := events()
	require.Len(t, got, 1)
	require.Equal(t, big, got[0].Key.Number, "result id must not pass through float64")
	require.True(t, held(t, cache, "cache-a", "SESS1", big))
}

func TestDepsOwnAndPartsOrderRetained(t *testing.T) {
	ctx := context.Background()
	cache, ing, events := newIngestor(t)

	// Dependency entry, held by its own session.
	dep := callSpan("Container.from",
		str(telemetryattrs.CacheContractAttr, telemetryattrs.CacheContractV1),
		str(telemetryattrs.CacheOutcomeAttr, telemetryattrs.CacheOutcomeExecuted),
		str(dagOutputAttr, "xxh3:dep1"),
		str(telemetryattrs.CacheResultIDAttr, "3"),
	)
	require.NoError(t, ing.Traces(ctx, traceReq(sessionResource("cache-a", "DEP"), dep)))

	// Dependent entry names 3 in its deps.
	require.NoError(t, ing.Traces(ctx, traceReq(sessionResource("cache-a", "TOP"),
		callSpan("Container.from", ordinarySpanAttrs("7")...))))

	// Releasing the dependency's own session must not collect it: the
	// dependent's dep edge owns it.
	keys := cache.ReleaseRemoteSession(ctx, "cache-a", "DEP")
	require.Len(t, keys, 1)
	collected, err := cache.CollectRemoteHoldings(ctx, keys)
	require.NoError(t, err)
	require.Empty(t, collected, "dependency must stay owned by its dependent")

	// Releasing the dependent collects both, through the dep edge.
	keys = cache.ReleaseRemoteSession(ctx, "cache-a", "TOP")
	collected, err = cache.CollectRemoteHoldings(ctx, keys)
	require.NoError(t, err)
	require.Len(t, collected, 2)
	require.Equal(t, uint64(3), collected[0].Number)
	require.Equal(t, uint64(7), collected[1].Number)

	got := events()
	require.Len(t, got, 2)
	require.Len(t, got[1].Parts, 2, "complete parts, absent ones included, are retained in order")
	require.Equal(t, dagql.PersistedPartAddress{Part: "fs"}, got[1].Parts[0])
	require.Equal(t, dagql.PersistedPartAddress{OutputPath: dagql.PersistedRefPath{{Field: "out"}}, Part: "meta"}, got[1].Parts[1])
}

// TestLifecycleEventsExactFields feeds the exact engine event bodies of the
// real capture (agents/discovery-capture.json lines 1 and 30) and exercises
// the retention and process-end APIs with them.
func TestLifecycleEventsExactFields(t *testing.T) {
	ctx := context.Background()
	cache, ing, events := newIngestor(t)

	startBody := `{"engineVersion":"v1.0.0-beta.25","engineName":"0784a22f5a14","restored":false,"restoredEntries":0}`
	require.NoError(t, ing.Logs(ctx, logsReq(engineResource("cache-c"), telemetryattrs.EngineEventScope,
		eventLog(telemetryattrs.EngineEventStart, startBody))))
	require.Empty(t, events())

	require.NoError(t, ing.Traces(ctx, traceReq(sessionResource("cache-c", "SESS1"),
		callSpan("Container.from", ordinarySpanAttrs("2")...))))

	// Exact prune body from the capture: two drops with engine times.
	pruneBody := `{"drops":[{"resultId":2,"droppedAtUnixNano":1794113180441479612},{"resultId":3,"droppedAtUnixNano":1794113180441503538}]}`
	require.NoError(t, ing.Logs(ctx, logsReq(engineResource("cache-c"), telemetryattrs.EngineEventScope,
		eventLog(telemetryattrs.EngineEventPrune, pruneBody))), "prune over a known and an unknown holding must not fail")

	// The prune observed retention through the real API; the session hold
	// still owns the entry until release.
	keys := cache.ReleaseRemoteSession(ctx, "cache-c", "SESS1")
	collected, err := cache.CollectRemoteHoldings(ctx, keys)
	require.NoError(t, err)
	require.Len(t, collected, 1)
	require.Equal(t, uint64(2), collected[0].Number)

	// engine.stop ends the process's sessions through EndRemoteProcess.
	require.NoError(t, ing.Traces(ctx, traceReq(sessionResource("cache-c", "SESS2"),
		callSpan("Container.from", ordinarySpanAttrs("5")...))))
	require.True(t, held(t, cache, "cache-c", "SESS2", 5))
	stopBody := `{"clean":true,"savedEntries":1}`
	require.NoError(t, ing.Logs(ctx, logsReq(engineResource("cache-c"), telemetryattrs.EngineEventScope,
		eventLog(telemetryattrs.EngineEventStop, stopBody))))
	require.False(t, held(t, cache, "cache-c", "SESS2", 5), "engine.stop must release the process's session holds")

	// Unknown event kinds are ignored.
	require.NoError(t, ing.Logs(ctx, logsReq(engineResource("cache-c"), telemetryattrs.EngineEventScope,
		eventLog("engine.other", "{}"))))
}

// TestCallPayloadLogLane covers the >1MiB frame lane: raw deterministic
// callpbv1.Call protobuf bytes in a log body, content-typed
// application/vnd.dagger.call+proto, with the digest repeated as an attribute.
func TestCallPayloadLogLane(t *testing.T) {
	ctx := context.Background()
	_, ing, events := newIngestor(t)

	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(&callpbv1.Call{
		Digest: "xxh3:req9",
		Field:  "from",
	})
	require.NoError(t, err)

	rec := &logspb.LogRecord{
		Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: payload}},
		Attributes: []*commonpb.KeyValue{
			str(contentTypeAttr, telemetryattrs.CallPayloadContentType),
			str(telemetryattrs.CallPayloadDigestAttr, "xxh3:req9"),
		},
	}
	require.NoError(t, ing.Logs(ctx, logsReq(engineResource("cache-d"), "dagger.io/core", rec)))

	// A span whose request digest matches the payload attaches normally.
	attrs := ordinarySpanAttrs("4")
	for i, a := range attrs {
		if a.Key == dagDigestAttr {
			attrs[i] = str(dagDigestAttr, "xxh3:req9")
		}
	}
	require.NoError(t, ing.Traces(ctx, traceReq(sessionResource("cache-d", "SESS1"), callSpan("Container.from", attrs...))))
	require.Len(t, events(), 1)

	// A malformed payload body is refused explicitly: malformed telemetry is
	// not silently accepted.
	bad := &logspb.LogRecord{
		Body:       &commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: []byte("junk")}},
		Attributes: []*commonpb.KeyValue{str(contentTypeAttr, telemetryattrs.CallPayloadContentType)},
	}
	require.Error(t, ing.Logs(ctx, logsReq(engineResource("cache-d"), "dagger.io/core", bad)))
}
