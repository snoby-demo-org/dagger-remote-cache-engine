// Package telemetry ingests isolated engine telemetry (OTLP traces and logs)
// into a dagql cache built from other engines' caches: it turns the
// dagger.io/cache.* span contract and the engine cache event logs into
// remote holdings, session holds and retention observations, and reports each
// known holding's state to the service through a callback.
package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/dagger/dagger/dagql"
	callpbv1 "github.com/dagger/dagger/dagql/call/callpbv1"
	"github.com/dagger/dagger/engine/telemetryattrs"
	"github.com/opencontainers/go-digest"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// Attribute keys written by the engine that are not exported from
// telemetryattrs: the dagql output/request digests of the call, and the
// content type of a call payload log body.
const (
	dagOutputAttr   = "dagger.io/dag.output"
	dagDigestAttr   = "dagger.io/dag.digest"
	contentTypeAttr = "dagger.io/content.type"
)

// Event is one report of a known holding's state, emitted after the holding
// has been attached or updated in the cache.
type Event struct {
	Key      dagql.HolderKey
	Recipe   string
	Parts    []dagql.PersistedPartAddress
	Executed bool
	Session  string
}

// deferredUpdate is an out-of-order report for a holding that does not exist
// yet: a lazy span or a share event never creates one, so its facts wait
// here, bounded, until a call span attaches the holding.
type deferredUpdate struct {
	contentDigest digest.Digest
	deps          []uint64
	parts         []dagql.PersistedPartAddress
	replacements  uint64
	// digests retains a deduplicated, bounded history of every nonempty
	// content digest observed for this key, so identities of older or
	// superseded observations are not lost when the pending update is
	// coalesced.
	digests []digest.Digest
}

// deferredFrame is a call payload log whose frame arrived before any span
// named its request digest. The frame supplies the decoded identity (field
// name) for the holding that span will attach.
type deferredFrame struct {
	field string
}

// Ingestor folds engine telemetry into a dagql cache's remote holdings.
type Ingestor struct {
	cache   *dagql.Cache
	onEvent func(Event)

	mu sync.Mutex
	// recipes maps each known holding key to the recipe digest of the call
	// that attached it, so a later lazy update can still report the recipe.
	recipes map[dagql.HolderKey]string
	// sessions maps each cache to the sessions seen on its spans, for
	// engine.stop's process end.
	sessions map[dagql.CacheID]map[string]struct{}
	// pending holds out-of-order updates per key, bounded by maxPendingKeys.
	pending map[dagql.HolderKey]*deferredUpdate
	// frames holds deferred call payload frames per request digest, bounded
	// by maxPendingFrames.
	frames map[string]*deferredFrame
}

const (
	// maxPendingKeys bounds the out-of-order update buffer: keys, not
	// records, so one key's repeated updates cannot grow it.
	maxPendingKeys = 4096
	// maxPendingFrames bounds the deferred call-payload frame map.
	maxPendingFrames = 4096
	// maxPendingDigests bounds the per-key content digest history retained
	// by a pending update.
	maxPendingDigests = 64
)

// New builds an Ingestor over cache. onEvent is called, outside the
// ingestor's locks, after a known holding has been attached or updated.
func New(cache *dagql.Cache, onEvent func(Event)) *Ingestor {
	return &Ingestor{
		cache:    cache,
		onEvent:  onEvent,
		recipes:  map[dagql.HolderKey]string{},
		sessions: map[dagql.CacheID]map[string]struct{}{},
		pending:  map[dagql.HolderKey]*deferredUpdate{},
		frames:   map[string]*deferredFrame{},
	}
}

// Traces ingests one OTLP trace export request.
func (ing *Ingestor) Traces(ctx context.Context, req *coltracepb.ExportTraceServiceRequest) error {
	if req == nil {
		return nil
	}
	for _, rs := range req.ResourceSpans {
		cacheID, generation, session, err := resourceScope(rs.GetResource())
		if err != nil {
			return err
		}
		for _, ss := range rs.GetScopeSpans() {
			for _, span := range ss.GetSpans() {
				if err := ing.span(ctx, cacheID, generation, session, span); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// Logs ingests one OTLP log export request.
func (ing *Ingestor) Logs(ctx context.Context, req *collogspb.ExportLogsServiceRequest) error {
	if req == nil {
		return nil
	}
	for _, rl := range req.ResourceLogs {
		cacheID, generation, session, err := resourceScope(rl.GetResource())
		if err != nil {
			return err
		}
		for _, sl := range rl.GetScopeLogs() {
			for _, rec := range sl.GetLogRecords() {
				if err := ing.logRecord(ctx, cacheID, generation, session, rec); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// resourceScope reads the engine cache identity ("<id>/<generation>"), the
// session and the engine instance from a record's resource attributes.
// lastCut splits s at the final occurrence of sep: the cache identity is
// "<id>/<generation>" and the id itself may contain slashes.
func lastCut(s, sep string) (string, string, bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return "", "", false
	}
	return s[:i], s[i+len(sep):], true
}

func resourceScope(res *resourcepb.Resource) (dagql.CacheID, uint64, string, error) {
	var (
		cacheIDGen string
		session    string
	)
	for _, attr := range res.GetAttributes() {
		switch attr.GetKey() {
		case telemetryattrs.EngineCacheAttr:
			cacheIDGen = attr.GetValue().GetStringValue()
		case telemetryattrs.EngineSessionAttr:
			session = attr.GetValue().GetStringValue()
		}
	}
	if cacheIDGen == "" {
		return "", 0, session, nil
	}
	id, generation, ok := lastCut(cacheIDGen, "/")
	if !ok {
		return "", 0, "", fmt.Errorf("telemetry: malformed %s %q", telemetryattrs.EngineCacheAttr, cacheIDGen)
	}
	gen, err := strconv.ParseUint(generation, 10, 64)
	if err != nil {
		return "", 0, "", fmt.Errorf("telemetry: malformed %s generation %q", telemetryattrs.EngineCacheAttr, generation)
	}
	return dagql.CacheID(id), gen, session, nil
}

// spanAttrs flattens a span's attributes; repeated keys keep the last value.
func spanAttrs(span *tracepb.Span) map[string]*commonpb.AnyValue {
	attrs := make(map[string]*commonpb.AnyValue, len(span.GetAttributes()))
	for _, attr := range span.GetAttributes() {
		attrs[attr.GetKey()] = attr.GetValue()
	}
	return attrs
}

// logAttrs flattens a log record's attributes.
func logAttrs(rec *logspb.LogRecord) map[string]*commonpb.AnyValue {
	attrs := make(map[string]*commonpb.AnyValue, len(rec.GetAttributes()))
	for _, attr := range rec.GetAttributes() {
		attrs[attr.GetKey()] = attr.GetValue()
	}
	return attrs
}

// span ingests one span: a call span with the cache contract attaches or
// updates a holding; a lazy span (no outcome) updates an existing holding or
// defers; a session-end carrier releases the session's holds.
func (ing *Ingestor) span(ctx context.Context, cacheID dagql.CacheID, generation uint64, session string, span *tracepb.Span) error {
	attrs := spanAttrs(span)

	if _, ok := attrs[telemetryattrs.WcprofSessionCompleteAttr]; ok {
		return ing.sessionEnd(ctx, cacheID, session)
	}

	contract, hasContract := attrs[telemetryattrs.CacheContractAttr]
	contractV1 := hasContract && contract.GetStringValue() == telemetryattrs.CacheContractV1
	if hasContract && !contractV1 {
		return nil // explicit unknown contract: not eligible
	}
	outcome := attrs[telemetryattrs.CacheOutcomeAttr].GetStringValue()
	if outcome != "" && !contractV1 {
		// An outcome-bearing span must carry the supported contract.
		return nil
	}

	number, err := parseUintAttr(attrs, telemetryattrs.CacheResultIDAttr)
	if err != nil {
		return err
	}
	if number == 0 {
		return nil
	}
	key := dagql.HolderKey{Cache: cacheID, Number: number}

	if outcome == "" {
		// A lazy-evaluation span: it never creates a holding.
		return ing.lazySpan(ctx, key, session, attrs)
	}

	recipe := digest.Digest(attrs[dagOutputAttr].GetStringValue())
	if recipe == "" {
		return fmt.Errorf("telemetry: %s span %q has no %s", telemetryattrs.CacheOutcomeAttr, span.GetName(), dagOutputAttr)
	}
	request := digest.Digest(attrs[dagDigestAttr].GetStringValue())

	replacements, err := parseUintAttr(attrs, telemetryattrs.CacheReplacementsAttr)
	if err != nil {
		return err
	}
	desc := dagql.RemoteHolding{
		Recipe:        recipe,
		Request:       request,
		Field:         span.GetName(),
		TypeName:      attrs[telemetryattrs.CacheTypeAttr].GetStringValue(),
		Executed:      outcome == telemetryattrs.CacheOutcomeExecuted,
		Replacements:  replacements,
		ExpiresAtUnix: parseIntOr(attrs, telemetryattrs.CacheExpiresAttr),
	}
	if self := attrs[telemetryattrs.CacheSelfDigestAttr].GetStringValue(); self != "" {
		term := &dagql.RemoteTerm{Self: digest.Digest(self)}
		for _, in := range stringSlice(attrs[telemetryattrs.CacheStructuralInputsAttr]) {
			term.Inputs = append(term.Inputs, digest.Digest(in))
		}
		desc.Term = term
	}
	if cd := attrs[telemetryattrs.CacheOutputContentDigestAttr].GetStringValue(); cd != "" {
		desc.ContentDigest = digest.Digest(cd)
	}
	deps, err := uint64Slice(attrs, telemetryattrs.CacheDepsAttr)
	if err != nil {
		return err
	}
	desc.Deps = deps
	parts, err := partSlice(attrs, telemetryattrs.CachePartsAttr)
	if err != nil {
		return err
	}
	desc.Parts = parts

	// Replay anything that arrived before this holding existed, merged into
	// the attach description.
	ing.mu.Lock()
	pending := ing.pending[key]
	delete(ing.pending, key)
	frame := ing.frames[string(request)]
	delete(ing.frames, string(request))
	ing.recipes[key] = string(recipe)
	ing.noteSessionLocked(cacheID, session)
	ing.mu.Unlock()
	if frame != nil && desc.Field == "" {
		desc.Field = frame.field
	}

	change, err := ing.cache.AttachRemoteHolding(ctx, key, desc)
	if err != nil {
		return fmt.Errorf("telemetry: attach holding %s/%d: %w", cacheID, number, err)
	}
	if session != "" {
		if err := ing.cache.AddRemoteHold(ctx, key, session); err != nil {
			return fmt.Errorf("telemetry: hold %s/%d: %w", cacheID, number, err)
		}
	}
	// Replay the deferred update after the attach and hold ownership are
	// established. First teach the holding every content identity observed
	// while it was pending: a zero-replacement update is ignored for state
	// but the class still learns the digest. Then apply the coalesced
	// deferred update itself; if it is older than the attach description the
	// cache ignores its state but still learns any identity.
	var replayCandidates []dagql.HolderKey
	if pending != nil {
		for _, d := range pending.digests {
			change, ok, err := ing.cache.UpdateRemoteHolding(ctx, key, dagql.RemoteHoldingUpdate{
				ContentDigest: d,
			})
			if err != nil {
				return fmt.Errorf("telemetry: replay digest %s/%d: %w", cacheID, number, err)
			}
			if ok {
				replayCandidates = append(replayCandidates, change.Candidates...)
			}
		}
		change, ok, err := ing.cache.UpdateRemoteHolding(ctx, key, dagql.RemoteHoldingUpdate{
			ContentDigest: pending.contentDigest,
			Deps:          pending.deps,
			Parts:         pending.parts,
			Replacements:  pending.replacements,
		})
		if err != nil {
			return fmt.Errorf("telemetry: replay deferred update %s/%d: %w", cacheID, number, err)
		}
		if ok {
			replayCandidates = append(replayCandidates, change.Candidates...)
		}
	}
	// Report the retention edge before collecting: the observation may add
	// candidates of its own.
	retained := attrs[telemetryattrs.CacheRetainedAttr].GetStringValue() == "true"
	retCandidates, err := ing.cache.ObserveRemoteRetention(ctx, key, dagql.RetentionObservation{
		Retained:           retained,
		ExpiresAtUnix:      parseIntOr(attrs, telemetryattrs.CacheRetentionExpiresAttr),
		Generation:         generation,
		EngineTimeUnixNano: int64(span.GetEndTimeUnixNano()),
	})
	if err != nil {
		return fmt.Errorf("telemetry: observe retention %s/%d: %w", cacheID, number, err)
	}
	change.Candidates = append(change.Candidates, retCandidates...)
	change.Candidates = append(change.Candidates, replayCandidates...)
	// Collect only after the hold, session ownership and all replayed
	// updates are established.
	if err := ing.collect(ctx, change.Candidates, fmt.Sprintf("collect after attach %s/%d", cacheID, number)); err != nil {
		return err
	}
	emitParts := desc.Parts
	if current, ok := ing.cache.RemoteHoldingInfo(key); ok {
		// The cache reconciles parts as a set; preserve observation order
		// without re-emitting parts a newer replacement discarded.
		keyOf := func(p dagql.PersistedPartAddress) string { b, _ := json.Marshal(p); return string(b) }
		remaining := make(map[string]bool, len(current.Holding.Parts))
		for _, p := range current.Holding.Parts {
			remaining[keyOf(p)] = true
		}
		observed := append([]dagql.PersistedPartAddress(nil), desc.Parts...)
		if pending != nil {
			observed = append(observed, pending.parts...)
		}
		observed = append(observed, current.Holding.Parts...)
		emitParts = nil
		for _, p := range observed {
			if k := keyOf(p); remaining[k] {
				emitParts = append(emitParts, p)
				delete(remaining, k)
			}
		}
	}
	ing.emit(key, string(recipe), emitParts, desc.Executed, session)
	return nil
}

// lazySpan applies a lazy-evaluation span: it updates an existing holding, or
// defers bounded until the call span attaches one. It never creates.
func (ing *Ingestor) lazySpan(ctx context.Context, key dagql.HolderKey, session string, attrs map[string]*commonpb.AnyValue) error {
	replacements, err := parseUintAttr(attrs, telemetryattrs.CacheReplacementsAttr)
	if err != nil {
		return err
	}
	update := dagql.RemoteHoldingUpdate{
		ContentDigest: digest.Digest(attrs[telemetryattrs.CacheOutputContentDigestAttr].GetStringValue()),
		Replacements:  replacements,
	}
	deps, err := uint64Slice(attrs, telemetryattrs.CacheDepsAttr)
	if err != nil {
		return err
	}
	update.Deps = deps
	parts, err := partSlice(attrs, telemetryattrs.CachePartsAttr)
	if err != nil {
		return err
	}
	update.Parts = parts

	ing.mu.Lock()
	recipe := ing.recipes[key]
	ing.noteSessionLocked(key.Cache, session)
	ing.mu.Unlock()

	change, ok, err := ing.cache.UpdateRemoteHolding(ctx, key, update)
	if err != nil {
		return fmt.Errorf("telemetry: update holding %s/%d: %w", key.Cache, key.Number, err)
	}
	if !ok {
		ing.deferUpdate(key, update)
		return nil
	}
	if err := ing.collect(ctx, change.Candidates, fmt.Sprintf("collect after update %s/%d", key.Cache, key.Number)); err != nil {
		return err
	}
	ing.emit(key, recipe, update.Parts, false, session)
	return nil
}

// deferUpdate records an out-of-order update, bounded: when the buffer is
// full the oldest-tracked key's entry is dropped rather than growing without
// bound.
func (ing *Ingestor) deferUpdate(key dagql.HolderKey, update dagql.RemoteHoldingUpdate) {
	ing.mu.Lock()
	defer ing.mu.Unlock()
	pending := ing.pending[key]
	if pending == nil {
		if len(ing.pending) >= maxPendingKeys {
			for drop := range ing.pending {
				delete(ing.pending, drop)
				break
			}
		}
		pending = &deferredUpdate{}
		ing.pending[key] = pending
	}
	// Record the observed identity before any replacement-count logic, so
	// older or superseded observations keep their content identity.
	if update.ContentDigest != "" {
		seen := false
		for _, d := range pending.digests {
			if d == update.ContentDigest {
				seen = true
				break
			}
		}
		if !seen {
			if len(pending.digests) >= maxPendingDigests {
				pending.digests = pending.digests[1:]
			}
			pending.digests = append(pending.digests, update.ContentDigest)
		}
	}
	switch {
	case update.Replacements > pending.replacements:
		// A higher replacement supersedes everything deferred so far.
		pending.replacements = update.Replacements
		pending.contentDigest = update.ContentDigest
		pending.deps = append([]uint64(nil), update.Deps...)
		pending.parts = append([]dagql.PersistedPartAddress(nil), update.Parts...)
	case update.Replacements == pending.replacements:
		if update.ContentDigest != "" {
			pending.contentDigest = update.ContentDigest
		}
		pending.deps = append(pending.deps, update.Deps...)
		pending.parts = append(pending.parts, update.Parts...)
	}
	// A strictly lower replacement is ignored.
}

// collect runs one collection pass and, for each key actually collected,
// drops the ingestor's stale bookkeeping so its maps cannot grow without
// bound.
func (ing *Ingestor) collect(ctx context.Context, candidates []dagql.HolderKey, where string) error {
	collected, err := ing.cache.CollectRemoteHoldings(ctx, candidates)
	if err != nil {
		return fmt.Errorf("telemetry: %s: %w", where, err)
	}
	ing.mu.Lock()
	for _, key := range collected {
		delete(ing.recipes, key)
		delete(ing.pending, key)
	}
	ing.mu.Unlock()
	return nil
}

// logRecord ingests one log record: engine cache events (start, prune, share,
// stop) and call payload logs. Everything else is ignored: an ordinary log is
// never a session end.
func (ing *Ingestor) logRecord(ctx context.Context, cacheID dagql.CacheID, generation uint64, session string, rec *logspb.LogRecord) error {
	attrs := logAttrs(rec)

	if ct := attrs[contentTypeAttr].GetStringValue(); ct == telemetryattrs.CallPayloadContentType {
		return ing.callPayload(rec.GetBody().GetBytesValue(), attrs)
	}

	kind := attrs[telemetryattrs.EngineEventAttr].GetStringValue()
	if kind == "" {
		return nil
	}
	eventCache, eventGen, err := eventScope(attrs, cacheID, generation)
	if err != nil {
		return err
	}
	switch kind {
	case telemetryattrs.EngineEventStart:
		var body telemetryattrs.EngineStartEvent
		if err := json.Unmarshal([]byte(rec.GetBody().GetStringValue()), &body); err != nil {
			return fmt.Errorf("telemetry: malformed %s body: %w", kind, err)
		}
		if body.WipedCache == "" {
			return nil
		}
		// The engine replaced an old cache: release everything held under
		// the old cache's ID, not the new one.
		old := dagql.CacheID(body.WipedCache)
		candidates := ing.cache.ReleaseRemoteCache(ctx, old)
		if _, err := ing.cache.CollectRemoteHoldings(ctx, candidates); err != nil {
			return fmt.Errorf("telemetry: collect after wiped cache %s: %w", old, err)
		}
		ing.mu.Lock()
		delete(ing.sessions, old)
		for key := range ing.recipes {
			if key.Cache == old {
				delete(ing.recipes, key)
			}
		}
		for key := range ing.pending {
			if key.Cache == old {
				delete(ing.pending, key)
			}
		}
		ing.mu.Unlock()
		return nil
	case telemetryattrs.EngineEventPrune:
		var body telemetryattrs.EnginePruneEvent
		if err := json.Unmarshal([]byte(rec.GetBody().GetStringValue()), &body); err != nil {
			return fmt.Errorf("telemetry: malformed %s body: %w", kind, err)
		}
		return ing.prune(ctx, eventCache, eventGen, body)
	case telemetryattrs.EngineEventShare:
		var body telemetryattrs.EngineShareEvent
		if err := json.Unmarshal([]byte(rec.GetBody().GetStringValue()), &body); err != nil {
			return fmt.Errorf("telemetry: malformed %s body: %w", kind, err)
		}
		return ing.share(ctx, eventCache, body)
	case telemetryattrs.EngineEventStop:
		var body telemetryattrs.EngineStopEvent
		if err := json.Unmarshal([]byte(rec.GetBody().GetStringValue()), &body); err != nil {
			return fmt.Errorf("telemetry: malformed %s body: %w", kind, err)
		}
		return ing.stop(ctx, eventCache)
	default:
		return nil
	}
}

// eventScope resolves the event's cache identity: the record's own
// EngineCacheAttr attribute, else the resource's.
func eventScope(attrs map[string]*commonpb.AnyValue, cacheID dagql.CacheID, generation uint64) (dagql.CacheID, uint64, error) {
	if v := attrs[telemetryattrs.EngineCacheAttr].GetStringValue(); v != "" {
		id, gen, ok := lastCut(v, "/")
		if !ok {
			return "", 0, fmt.Errorf("telemetry: malformed %s %q", telemetryattrs.EngineCacheAttr, v)
		}
		genN, err := strconv.ParseUint(gen, 10, 64)
		if err != nil {
			return "", 0, fmt.Errorf("telemetry: malformed %s generation %q", telemetryattrs.EngineCacheAttr, gen)
		}
		return dagql.CacheID(id), genN, nil
	}
	return cacheID, generation, nil
}

// prune applies one prune run's retention drops through the real retention
// API, ordered by generation and engine time. Drops of holdings the cache
// does not know are ignored.
func (ing *Ingestor) prune(ctx context.Context, cacheID dagql.CacheID, generation uint64, body telemetryattrs.EnginePruneEvent) error {
	var candidates []dagql.HolderKey
	for _, drop := range body.Drops {
		key := dagql.HolderKey{Cache: cacheID, Number: drop.ResultID}
		// A drop means the retention edge is gone: not retained, no expiry.
		retCandidates, err := ing.cache.ObserveRemoteRetention(ctx, key, dagql.RetentionObservation{
			Retained:           false,
			Generation:         generation,
			EngineTimeUnixNano: drop.DroppedAtUnixNano,
		})
		if err != nil {
			if errors.Is(err, dagql.ErrUnknownHolding) {
				continue
			}
			return fmt.Errorf("telemetry: prune drop %s/%d: %w", cacheID, drop.ResultID, err)
		}
		candidates = append(candidates, retCandidates...)
	}
	return ing.collect(ctx, candidates, "collect after prune")
}

// share applies one snapshot-sharing pass: it updates only existing
// holdings, never creates one.
func (ing *Ingestor) share(ctx context.Context, cacheID dagql.CacheID, body telemetryattrs.EngineShareEvent) error {
	for _, part := range body.Parts {
		key := dagql.HolderKey{Cache: cacheID, Number: part.ResultID}
		address, err := partAddress(part.Part)
		if err != nil {
			return fmt.Errorf("telemetry: share part %s/%d: %w", cacheID, part.ResultID, err)
		}
		update := dagql.RemoteHoldingUpdate{
			Deps:         part.Deps,
			Parts:        []dagql.PersistedPartAddress{address},
			Replacements: part.Replacements,
		}
		ing.mu.Lock()
		recipe := ing.recipes[key]
		ing.mu.Unlock()
		change, ok, err := ing.cache.UpdateRemoteHolding(ctx, key, update)
		if err != nil {
			return fmt.Errorf("telemetry: share %s/%d: %w", cacheID, part.ResultID, err)
		}
		if !ok {
			ing.deferUpdate(key, update)
			continue
		}
		if err := ing.collect(ctx, change.Candidates, fmt.Sprintf("collect after share %s/%d", cacheID, part.ResultID)); err != nil {
			return err
		}
		ing.emit(key, recipe, update.Parts, false, "")
	}
	return nil
}

// stop ends the engine cache's process: every session this ingestor saw on
// the cache's telemetry releases its holds.
func (ing *Ingestor) stop(ctx context.Context, cacheID dagql.CacheID) error {
	ing.mu.Lock()
	sessions := make([]string, 0, len(ing.sessions[cacheID]))
	for s := range ing.sessions[cacheID] {
		sessions = append(sessions, s)
	}
	delete(ing.sessions, cacheID)
	ing.mu.Unlock()
	if len(sessions) == 0 {
		return nil
	}
	candidates := ing.cache.EndRemoteProcess(ctx, cacheID, sessions)
	return ing.collect(ctx, candidates, fmt.Sprintf("collect after stop %s", cacheID))
}

// sessionEnd releases one session's holds when a session-end carrier span
// arrives. Only the carrier span (wcprof.session_complete) does this; no
// other span or log is a session end.
func (ing *Ingestor) sessionEnd(ctx context.Context, cacheID dagql.CacheID, session string) error {
	if session == "" {
		return nil
	}
	ing.mu.Lock()
	if held := ing.sessions[cacheID]; held != nil {
		delete(held, session)
	}
	ing.mu.Unlock()
	candidates := ing.cache.ReleaseRemoteSession(ctx, cacheID, session)
	return ing.collect(ctx, candidates, fmt.Sprintf("collect after session end %s", session))
}

// callPayload decodes a call payload log body (raw deterministic
// callpbv1.Call protobuf) and remembers its identity for the span that names
// its digest. A body that does not decode is refused explicitly.
func (ing *Ingestor) callPayload(body []byte, attrs map[string]*commonpb.AnyValue) error {
	var frame callpbv1.Call
	if err := (proto.UnmarshalOptions{}).Unmarshal(body, &frame); err != nil {
		return fmt.Errorf("telemetry: malformed call payload: %w", err)
	}
	dgst := attrs[telemetryattrs.CallPayloadDigestAttr].GetStringValue()
	if dgst == "" {
		dgst = frame.GetDigest()
	}
	ing.mu.Lock()
	defer ing.mu.Unlock()
	if _, ok := ing.frames[dgst]; !ok && len(ing.frames) >= maxPendingFrames {
		for drop := range ing.frames {
			delete(ing.frames, drop)
			break
		}
	}
	ing.frames[dgst] = &deferredFrame{field: frame.GetField()}
	return nil
}

// noteSessionLocked records a session seen on a cache's telemetry.
func (ing *Ingestor) noteSessionLocked(cacheID dagql.CacheID, session string) {
	if session == "" {
		return
	}
	held := ing.sessions[cacheID]
	if held == nil {
		held = map[string]struct{}{}
		ing.sessions[cacheID] = held
	}
	held[session] = struct{}{}
}

// emit reports one known holding's state to the service, outside the
// ingestor's locks.
func (ing *Ingestor) emit(key dagql.HolderKey, recipe string, parts []dagql.PersistedPartAddress, executed bool, session string) {
	if ing.onEvent == nil || recipe == "" {
		return
	}
	ing.onEvent(Event{
		Key:      key,
		Recipe:   recipe,
		Parts:    parts,
		Executed: executed,
		Session:  session,
	})
}

// parseUintAttr reads a decimal uint64 attribute. It refuses anything that is
// not exactly a decimal uint64 string: no float round-trip, no sign, no
// spaces.
func parseUintAttr(attrs map[string]*commonpb.AnyValue, key string) (uint64, error) {
	v := attrs[key].GetStringValue()
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("telemetry: malformed %s %q", key, v)
	}
	return n, nil
}

// parseUintOr reads a decimal uint64 attribute, 0 when absent or malformed.
func parseUintOr(attrs map[string]*commonpb.AnyValue, key string) uint64 {
	n, _ := parseUintAttr(attrs, key)
	return n
}

// parseIntOr reads a decimal int64 attribute, 0 when absent or malformed.
func parseIntOr(attrs map[string]*commonpb.AnyValue, key string) int64 {
	v := attrs[key].GetStringValue()
	if v == "" {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// uint64Slice reads a native string array of decimal uint64s, in order.
func uint64Slice(attrs map[string]*commonpb.AnyValue, key string) ([]uint64, error) {
	values := attrs[key].GetArrayValue().GetValues()
	if len(values) == 0 {
		return nil, nil
	}
	out := make([]uint64, 0, len(values))
	for _, item := range values {
		s := item.GetStringValue()
		n, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("telemetry: malformed %s entry %q", key, s)
		}
		out = append(out, n)
	}
	return out, nil
}

// partSlice reads a native string array of part addresses, each a JSON
// encoding of dagql.PersistedPartAddress, in order.
func partSlice(attrs map[string]*commonpb.AnyValue, key string) ([]dagql.PersistedPartAddress, error) {
	values := attrs[key].GetArrayValue().GetValues()
	if len(values) == 0 {
		return nil, nil
	}
	out := make([]dagql.PersistedPartAddress, 0, len(values))
	for _, item := range values {
		address, err := partAddress(item.GetStringValue())
		if err != nil {
			return nil, fmt.Errorf("telemetry: malformed %s entry: %w", key, err)
		}
		out = append(out, address)
	}
	return out, nil
}

// partAddress decodes one part address JSON string.
func partAddress(s string) (dagql.PersistedPartAddress, error) {
	var address dagql.PersistedPartAddress
	if err := json.Unmarshal([]byte(s), &address); err != nil {
		return address, err
	}
	return address, nil
}

// stringSlice reads a native string array of strings.
func stringSlice(v *commonpb.AnyValue) []string {
	values := v.GetArrayValue().GetValues()
	out := make([]string, 0, len(values))
	for _, item := range values {
		out = append(out, item.GetStringValue())
	}
	return out
}
