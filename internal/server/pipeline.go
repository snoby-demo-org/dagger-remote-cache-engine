package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/dagger/dagger/dagql"
	"github.com/dagger/dagger/engine/remotecache/protocol"
	"github.com/opencontainers/go-digest"

	"github.com/snoby/dcache/internal/graph"
	"github.com/snoby/dcache/internal/peer"
	"github.com/snoby/dcache/internal/renewal"
	"github.com/snoby/dcache/internal/telemetry"
)

// maxWarmBundle bounds the serialized warm bundle at 16 MiB.
const maxWarmBundle = 16 << 20

// maxExportRetries bounds the requeue attempts for an entry that is already
// being exported when a completion observation arrives.
const maxExportRetries = 3

// mergedToReply converts the engine's wire reply to dagql's MergeReply,
// preserving uint64 numbers exactly. Retained roots take their number from the
// matching merged value; skipped roots are marked expired.
func mergedToReply(m protocol.Merged) dagql.MergeReply {
	reply := dagql.MergeReply{
		Generation:         m.Generation,
		EngineTimeUnixNano: m.EngineTime,
		Committed:          true,
	}
	byOrdinal := make(map[dagql.TransferOrdinal]uint64, len(m.Values))
	for _, v := range m.Values {
		byOrdinal[v.Ordinal] = v.Number
		reply.Values = append(reply.Values, dagql.MergedValue{
			Ordinal:       v.Ordinal,
			Number:        v.Number,
			Replacements:  v.Replacements,
			ExpiresAtUnix: v.ExpiresAtUnix,
			Deps:          append([]uint64(nil), v.Deps...),
			Parts:         append([]dagql.PersistedPartAddress(nil), v.Complete...),
			OfferedParts:  append([]dagql.PersistedPartAddress(nil), v.Offered...),
		})
	}
	for _, r := range m.Retained {
		reply.Roots = append(reply.Roots, dagql.MergedRoot{
			Ordinal:                r.Ordinal,
			Number:                 byOrdinal[r.Ordinal],
			Retained:               true,
			RetentionExpiresAtUnix: r.ExpiresAtUnix,
		})
	}
	for _, s := range m.Skipped {
		reply.Roots = append(reply.Roots, dagql.MergedRoot{
			Ordinal: s.Ordinal,
			Expired: true,
		})
	}
	return reply
}

// validateRenewClaims checks that the signed claims name exactly the ordered
// layer digests of the request, that the needed blob belongs to those layers,
// and that the donor is positive.
func validateRenewClaims(claims renewal.Claims, req protocol.Renew) error {
	if claims.Donor == 0 {
		return errors.New("renew: donor must be positive")
	}
	if len(claims.Digests) != len(req.Layers) {
		return fmt.Errorf("renew: claims name %d layers, request has %d", len(claims.Digests), len(req.Layers))
	}
	needed := false
	for i, layer := range req.Layers {
		d := layer.Descriptor.Digest
		if d.String() != claims.Digests[i] {
			return fmt.Errorf("renew: layer %d digest mismatch", i)
		}
		if d == req.NeededBlob {
			needed = true
		}
	}
	if !needed {
		return errors.New("renew: needed blob is not one of the layers")
	}
	return nil
}

// capBundle returns the whole bundle if its serialized size fits maxBytes,
// otherwise an empty bundle. It never prunes closure values.
func capBundle(bundle dagql.ValueBundle, maxBytes int) (dagql.ValueBundle, error) {
	if maxBytes <= 0 {
		return dagql.ValueBundle{}, nil
	}
	raw, err := json.Marshal(bundle)
	if err != nil {
		return dagql.ValueBundle{}, err
	}
	if len(raw) > maxBytes {
		return dagql.ValueBundle{}, nil
	}
	return bundle, nil
}

// bundleDonorExpiry looks up the transferred value sent by donor and returns
// its expiry. A zero expiry legitimately means infinite.
func bundleDonorExpiry(bundle dagql.ValueBundle, donor uint64) (int64, bool) {
	for _, v := range bundle.Values {
		if v.SenderNumber == donor {
			return v.ExpiresAtUnix, true
		}
	}
	return 0, false
}

// redact strips URL-bearing tails and bounds a diagnostic message.
func redact(msg string) string {
	if idx := strings.Index(msg, "http"); idx >= 0 {
		msg = msg[:idx] + "[redacted]"
	}
	if len(msg) > 256 {
		msg = msg[:256]
	}
	return msg
}

func (s *Server) logStage(stage string, err error) {
	if err == nil {
		return
	}
	log.Printf("dcache: stage %s: %s", stage, redact(err.Error()))
}

func (s *Server) logSkip(number uint64, reason string) {
	log.Printf("dcache: root %d skipped: %s", number, redact(reason))
}

// exportOutputs selects the holding's complete parts for upload, excluding
// metadata parts, which carry no bytes.
func exportOutputs(held dagql.RemoteHoldingInfo) []protocol.ExportOutput {
	var outs []protocol.ExportOutput
	for _, part := range held.Parts {
		if part.Part == "metadata" {
			continue
		}
		outs = append(outs, protocol.ExportOutput{Number: held.Key.Number, Address: part})
	}
	return outs
}

// signChain adds a presigned GET address per layer and a renewal token naming
// the donor and the ordered layer digests.
func signChain(ctx context.Context, store BlobStore, signer *renewal.Signer, donor uint64, chain dagql.OfferedChain) (dagql.OfferedChain, error) {
	if chain.Addresses == nil {
		chain.Addresses = make(map[digest.Digest]dagql.BlobAddress, len(chain.Layers))
	}
	digests := make([]string, 0, len(chain.Layers))
	for _, layer := range chain.Layers {
		d := layer.Descriptor.Digest
		url, expires, err := store.PresignGet(ctx, d.String())
		if err != nil {
			return dagql.OfferedChain{}, fmt.Errorf("sign chain: presign %s: %w", d, err)
		}
		chain.Addresses[d] = dagql.BlobAddress{URL: url, ExpiresAtUnix: expires.Unix()}
		digests = append(digests, d.String())
	}
	token, err := signer.Mint(donor, digests)
	if err != nil {
		return dagql.OfferedChain{}, fmt.Errorf("sign chain: mint: %w", err)
	}
	chain.RenewalKey = token
	return chain, nil
}

// onObservation schedules a bounded export for a telemetry observation. It
// never blocks the ingestor: when the export semaphore is full the event is
// counted as dropped.
func (s *Server) onObservation(e telemetry.Event) {
	s.onObservationAttempt(e, 0)
}

func (s *Server) onObservationAttempt(e telemetry.Event, attempt int) {
	if e.Key.Cache == "" || e.Key.Number == 0 {
		return
	}
	s.mu.Lock()
	if s.ctx.Err() != nil {
		s.mu.Unlock()
		return
	}
	if s.exporting[e.Key] {
		// Bounded coalescing: the owner worker will pick this up after its
		// current attempt instead of dropping it after retries.
		s.dirty[e.Key] = e
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	select {
	case s.exportSem <- struct{}{}:
	default:
		s.stats.Dropped.Add(1)
		return
	}
	s.mu.Lock()
	if s.ctx.Err() != nil {
		s.mu.Unlock()
		<-s.exportSem
		return
	}
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		s.runExport(e, attempt)
	}()
}

// runExport owns one key at a time, holding its exportSem slot and the
// exporting mark for the whole visit. Observations coalesced into dirty
// during an attempt are drained and re-exported before the slot is released.
func (s *Server) runExport(e telemetry.Event, attempt int) {
	defer func() { <-s.exportSem }()
	s.mu.Lock()
	if s.exporting[e.Key] {
		s.dirty[e.Key] = e
		s.mu.Unlock()
		return
	}
	s.exporting[e.Key] = true
	s.mu.Unlock()
	for {
		s.mu.Lock()
		p := s.peers[e.Key.Cache]
		s.mu.Unlock()
		if p != nil {
			s.exportHolding(p, e)
		}
		s.mu.Lock()
		next, ok := s.dirty[e.Key]
		delete(s.dirty, e.Key)
		if !ok || s.ctx.Err() != nil {
			delete(s.exporting, e.Key)
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()
		e = next
	}
}

// exportHolding exports the latest remote holding state of one entry, offers
// its missing parts, uploads the bundle, verifies the blobs, and publishes
// only verified parts.
func (s *Server) exportHolding(p *peer.Peer, e telemetry.Event) {
	slot := s.exportSlotsFor(p)
	select {
	case slot <- struct{}{}:
	case <-s.ctx.Done():
		return
	case <-p.Done():
		return
	}
	defer func() { <-slot }()
	held, ok := s.graph.Cache.RemoteHoldingInfo(e.Key)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, s.cfg.RequestTimeout)
	defer cancel()

	s.offerParts(ctx, p, e.Key, e.Recipe, held.Holding)

	req := protocol.Export{
		Roots:   []uint64{e.Key.Number},
		Outputs: exportOutputs(held.Holding),
	}
	var exported protocol.Exported
	for attempt := 0; ; attempt++ {
		exported = protocol.Exported{}
		// Refresh the latest holding state before every export request or
		// retry so newly completed lazy snapshot parts are selected.
		if latest, ok := s.graph.Cache.RemoteHoldingInfo(e.Key); ok {
			req.Outputs = exportOutputs(latest.Holding)
		}
		if err := p.Request(ctx, protocol.TypeExport, req, &exported); err != nil {
			s.logStage("export", err)
			// A typed engine error means the engine saw the request and did not
			// capture; leave the peer and other healthy exports alone. Any other
			// failure (deadline, cancel, bad decode, unknown) may have stranded a
			// late capture, so close the peer best-effort to release it.
			var remote *peer.RemoteError
			if !errors.As(err, &remote) {
				p.Close()
			}
			return
		}
		busy := false
		for _, r := range exported.Skipped {
			if r.Number == e.Key.Number && r.Reason == protocol.SkipBusy {
				busy = true
				break
			}
		}
		if !busy {
			break
		}
		var msg string
		for _, r := range exported.Skipped {
			if r.Number == e.Key.Number {
				msg = r.Message
				break
			}
		}
		s.logSkip(e.Key.Number, "export skipped: "+string(protocol.SkipBusy)+": "+msg)
		if attempt >= 9 {
			return
		}
		s.releaseExport(p, exported.ExportID)
		select {
		case <-time.After(500 * time.Millisecond):
		case <-ctx.Done():
			return
		}
	}
	s.stats.Exports.Add(1)

	if exported.Bundle == nil {
		// Release any chains the engine kept open, with an empty upload.
		s.releaseExport(p, exported.ExportID)
		return
	}
	bundle := *exported.Bundle
	sizes := bundleBlobSizes(bundle)
	log.Printf("dcache: export root %d captured: outputs=%d bundle=%d sizes=%d", e.Key.Number, len(req.Outputs), len(bundle.Outputs), len(sizes))

	// Prepare before planning URLs so a slow commit never delays the upload
	// continuation. The prepared merge is committed or released exactly once.
	prepared, err := s.graph.Cache.PrepareValueMerge(ctx, e.Key.Cache, bundle)
	if err != nil {
		s.logStage("prepareMerge", err)
		s.releaseExport(p, exported.ExportID)
		return
	}

	urls, claimed, existing := s.planUpload(ctx, sizes)
	defer s.releaseInflight(claimed)

	uploadCtx, uploadCancel := context.WithTimeout(s.ctx, s.cfg.UploadTimeout)
	defer uploadCancel()
	type uploadResult struct {
		uploaded protocol.Uploaded
		err      error
	}
	uploadCh := make(chan uploadResult, 1)
	go func() {
		var uploaded protocol.Uploaded
		err := p.Request(uploadCtx, protocol.TypeUpload, protocol.Upload{
			ExportID: exported.ExportID,
			URLs:     urls,
		}, &uploaded)
		uploadCh <- uploadResult{uploaded: uploaded, err: err}
	}()

	reply, commitErr := s.graph.Cache.CommitValueMerge(ctx, prepared)
	if commitErr != nil {
		s.logStage("commitMerge", commitErr)
		// Commit is terminal; the reply may still describe what landed.
		if reply.Committed {
			s.publishVerified(ctx, bundle, reply, sizes, nil)
		}
		<-uploadCh
		return
	}

	res := <-uploadCh
	if res.err != nil {
		s.logStage("upload", res.err)
		return
	}
	verifyCtx, verifyCancel := context.WithTimeout(s.ctx, 5*time.Minute)
	defer verifyCancel()
	verified := s.verifyUploaded(verifyCtx, append(append([]digest.Digest(nil), res.uploaded.Done...), existing...), sizes)
	s.publishVerified(verifyCtx, bundle, reply, sizes, verified)
	s.stats.Uploaded.Add(int64(len(res.uploaded.Done)))
	_, _ = s.graph.Cache.CollectRemoteHoldings(verifyCtx, reply.Change.Candidates)
}

// releaseExport sends an empty upload continuation so the engine releases any
// chains it kept open. It ignores an empty export ID, uses a fresh bounded
// context derived from the server context, and closes the peer if the request
// fails.
func (s *Server) releaseExport(p *peer.Peer, exportID string) {
	if exportID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, s.cfg.RequestTimeout)
	defer cancel()
	var uploaded protocol.Uploaded
	if err := p.Request(ctx, protocol.TypeUpload, protocol.Upload{
		ExportID: exportID,
		URLs:     map[digest.Digest]string{},
	}, &uploaded); err != nil {
		p.Close()
	}
}

// bundleBlobSizes returns each layer digest's declared size across the
// bundle's output chains.
func bundleBlobSizes(bundle dagql.ValueBundle) map[digest.Digest]int64 {
	sizes := map[digest.Digest]int64{}
	for _, out := range bundle.Outputs {
		if out.Chain == nil {
			continue
		}
		for _, layer := range out.Chain.Layers {
			sizes[layer.Descriptor.Digest] = layer.Descriptor.Size
		}
	}
	return sizes
}

// planUpload presigns a PUT for each blob the store does not already have,
// claiming each digest in the inflight map. It returns the presigned URLs,
// the claimed digests, and the digests already present in the store.
func (s *Server) planUpload(ctx context.Context, sizes map[digest.Digest]int64) (map[digest.Digest]string, []digest.Digest, []digest.Digest) {
	urls := map[digest.Digest]string{}
	var claimed []digest.Digest
	var existing []digest.Digest
	for d, size := range sizes {
		exists, err := s.store.Exists(ctx, d.String(), size)
		if err != nil {
			continue
		}
		if exists {
			existing = append(existing, d)
			continue
		}
		url, err := s.store.PresignPut(ctx, d.String(), size)
		if err != nil {
			continue
		}
		urls[d] = url
		s.mu.Lock()
		s.inflight[d.String()]++
		s.mu.Unlock()
		claimed = append(claimed, d)
	}
	return urls, claimed, existing
}

// releaseInflight drops the claims planUpload took.
func (s *Server) releaseInflight(claimed []digest.Digest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range claimed {
		key := d.String()
		if s.inflight[key] <= 1 {
			delete(s.inflight, key)
		} else {
			s.inflight[key]--
		}
	}
}

// verifyUploaded verifies each uploaded blob under the verifier semaphore and
// returns the set that passed.
func (s *Server) verifyUploaded(ctx context.Context, done []digest.Digest, sizes map[digest.Digest]int64) map[digest.Digest]bool {
	verified := map[digest.Digest]bool{}
	for _, d := range done {
		size, ok := sizes[d]
		if !ok {
			continue
		}
		select {
		case s.verifySem <- struct{}{}:
		case <-ctx.Done():
			return verified
		}
		err := s.store.Verify(ctx, d.String(), size)
		<-s.verifySem
		if err != nil {
			s.logStage("verify", err)
			continue
		}
		verified[d] = true
	}
	return verified
}

// publishVerified relocates the bundle's completed outputs and stores only
// parts whose every layer blob verified. A nil verified set means no upload
// confirmation, so nothing is published.
func (s *Server) publishVerified(ctx context.Context, bundle dagql.ValueBundle, reply dagql.MergeReply, sizes map[digest.Digest]int64, verified map[digest.Digest]bool) {
	if verified == nil {
		return
	}
	parts, err := graph.RelocateOutputs(bundle, reply)
	if err != nil {
		s.logStage("publish", err)
		return
	}
	for _, part := range parts {
		if part.Part.Address.Part == "metadata" {
			continue
		}
		ok := true
		for _, layer := range part.Part.Chain.Layers {
			if !verified[layer.Descriptor.Digest] {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		stored, err := s.graph.Cache.SetStoredPart(ctx, part.Number, part.Part, part.ExpiresAtUnix)
		if err != nil {
			s.logStage("publish", err)
			continue
		}
		if stored {
			s.stats.Published.Add(1)
		}
	}
}

// offerParts offers the Cloud's stored parts of equivalent entries to a known
// target holding, then applies the engine's actual reply state. Accepted
// offers are never counted as installed.
func (s *Server) offerParts(ctx context.Context, p *peer.Peer, key dagql.HolderKey, recipe string, held dagql.RemoteHoldingInfo) {
	if recipe == "" {
		return
	}
	numbers := s.graph.Cache.EquivalentEntries(recipe)
	if len(numbers) == 0 {
		return
	}
	state, err := s.graph.Cache.StoredState()
	if err != nil {
		return
	}
	have := map[string]bool{}
	for _, part := range held.Parts {
		have[partKey(part)] = true
	}
	for _, part := range held.OfferedParts {
		have[partKey(part)] = true
	}
	var offers []protocol.CloudPart
	for _, number := range numbers {
		for _, address := range state.Parts[number] {
			if have[partKey(address)] {
				continue
			}
			avail, err := s.graph.Cache.AvailablePart(number, address)
			if err != nil || avail.State != dagql.PartStored {
				continue
			}
			placed, ok, err := s.graph.Cache.PlaceOffer(key, avail.Part)
			if err != nil || !ok {
				continue
			}
			chain, err := signChain(ctx, s.store, s.signer, avail.Donor, placed.Chain)
			if err != nil {
				continue
			}
			placed.Chain = chain
			donorResult, err := s.graph.Cache.StoredBundle(ctx, []uint64{avail.Donor})
			if err != nil {
				continue
			}
			expiry, ok := bundleDonorExpiry(donorResult.Bundle, avail.Donor)
			if !ok {
				continue
			}
			offers = append(offers, protocol.CloudPart{
				Offer:              placed,
				CloudNumber:        avail.Donor,
				CloudExpiresAtUnix: expiry,
				CloudStored:        true,
			})
		}
	}
	if len(offers) == 0 {
		return
	}
	req := protocol.Offer{Items: []protocol.OfferItem{{Number: key.Number, Offers: offers}}}
	var offered protocol.Offered
	if err := p.Request(ctx, protocol.TypeOffer, req, &offered); err != nil {
		return
	}
	s.stats.Offers.Add(1)
	for _, item := range offered.Items {
		if item.Gone {
			continue
		}
		var complete, offeredParts []dagql.PersistedPartAddress
		for _, part := range item.Parts {
			switch part.Outcome {
			case protocol.OfferAlreadyComplete:
				complete = append(complete, part.Address)
			case protocol.OfferAccepted, protocol.OfferExecutionStarted:
				offeredParts = append(offeredParts, part.Address)
			}
		}
		_, _, _ = s.graph.Cache.ApplyHeldValueState(ctx, key, dagql.HeldValueState{
			Replacements: item.Replacements,
			Deps:         item.Deps,
			Parts:        complete,
			OfferedParts: offeredParts,
		})
	}
}

func partKey(address dagql.PersistedPartAddress) string {
	raw, _ := json.Marshal(address)
	return string(raw)
}

// warmPeer merges a capped, recent, size-bounded bundle of the Cloud's stored
// roots into a newly connected engine. For v1 it includes single-donor roots
// as well, a deliberate pragmatic simplification so the first engine can
// warm-hit after another has exported.
func (s *Server) warmPeer(p *peer.Peer) {
	state, err := s.graph.Cache.StoredState()
	if err != nil || len(state.Roots) == 0 {
		return
	}
	roots := state.Roots
	if s.cfg.WarmRoots > 0 {
		for len(roots) > s.cfg.WarmRoots {
			roots = roots[1:]
		}
	}
	ctx, cancel := context.WithTimeout(s.ctx, s.cfg.RequestTimeout)
	defer cancel()
	for len(roots) > 0 {
		result, err := s.graph.Cache.StoredBundle(ctx, roots)
		if err != nil {
			return
		}
		bundle := result.Bundle
		if len(bundle.Roots) == 0 {
			roots = roots[:len(roots)-1]
			continue
		}
		// Sign before the serialized size cap so the signed bundle is what is
		// bounded; on overflow drop the oldest root and rebuild the whole
		// bundle, preserving the donor pairing from StoredBundle.
		for i := range bundle.Outputs {
			if bundle.Outputs[i].Chain == nil {
				continue
			}
			donor := uint64(0)
			if i < len(result.Donors) {
				donor = result.Donors[i]
			}
			if donor == 0 {
				continue
			}
			signed, err := signChain(ctx, s.store, s.signer, donor, *bundle.Outputs[i].Chain)
			if err != nil {
				return
			}
			bundle.Outputs[i].Chain = &signed
		}
		raw, err := json.Marshal(bundle)
		if err != nil {
			s.logStage("warmBundle", err)
			return
		}
		if len(raw) > maxWarmBundle {
			log.Printf("dcache: warm bundle %d bytes exceeds cap %d; dropping oldest of %d roots", len(raw), maxWarmBundle, len(roots))
			roots = roots[1:]
			continue
		}
		var merged protocol.Merged
		if err := p.Request(ctx, protocol.TypeMerge, protocol.Merge{Bundle: bundle}, &merged); err != nil {
			return
		}
		for i, v := range merged.Values {
			if i >= 8 {
				break
			}
			log.Printf("dcache: warm peer=%s ordinal=%d number=%d expiry=%d complete=%d offered=%d", p.Hello.CacheID, v.Ordinal, v.Number, v.ExpiresAtUnix, len(v.Complete), len(v.Offered))
		}
		reply := mergedToReply(merged)
		change, err := s.graph.Cache.ApplyMergedReply(ctx, dagql.CacheID(p.Hello.CacheID), bundle, reply)
		if err != nil {
			return
		}
		s.stats.Merges.Add(1)
		_, _ = s.graph.Cache.CollectRemoteHoldings(ctx, change.Candidates)
		return
	}
}

// handleRequest serves engine requests. Only renew is handled here; the
// whole renewal chain shares one RenewTimeout, and a missing object drops the
// stored parts only on a positive not-found, never on a backend error.
func (s *Server) handleRequest(ctx context.Context, p *peer.Peer, envelope protocol.Envelope) (any, error) {
	if envelope.Type != protocol.TypeRenew {
		return nil, fmt.Errorf("unsupported request type %q", envelope.Type)
	}
	var req protocol.Renew
	if err := envelope.Decode(&req); err != nil {
		return nil, err
	}
	claims, err := s.signer.Verify(req.RenewalKey)
	if err != nil {
		return nil, err
	}
	if err := validateRenewClaims(claims, req); err != nil {
		return nil, err
	}
	rctx, cancel := context.WithTimeout(ctx, s.cfg.RenewTimeout)
	defer cancel()
	addresses := make(map[digest.Digest]dagql.BlobAddress, len(req.Layers))
	for _, layer := range req.Layers {
		d := layer.Descriptor.Digest
		size := layer.Descriptor.Size
		exists, err := s.store.Exists(rctx, d.String(), size)
		if err != nil {
			return protocol.Renewed{Unavailable: true}, nil
		}
		if !exists {
			_, _ = s.graph.Cache.DropStoredParts(d)
			return protocol.Renewed{Unavailable: true}, nil
		}
		url, expires, err := s.store.PresignGet(rctx, d.String())
		if err != nil {
			return protocol.Renewed{Unavailable: true}, nil
		}
		addresses[d] = dagql.BlobAddress{URL: url, ExpiresAtUnix: expires.Unix()}
	}
	s.stats.Renewals.Add(1)
	return protocol.Renewed{Addresses: addresses}, nil
}
