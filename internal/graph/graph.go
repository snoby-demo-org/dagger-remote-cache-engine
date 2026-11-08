// Package graph is a standalone coordinator over the pinned dagql cache. It
// opens the cache's SQLite store, checkpoints and closes it, and relocates a
// merged transfer bundle's completed outputs to the numbers the merge
// committed, so the caller can publish them as stored parts.
package graph

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"

	_ "github.com/dagger/dagger/core" // registers the real persisted object families and transfer codecs
	"github.com/dagger/dagger/dagql"
	"github.com/dagger/dagger/engine/snapshots"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
)

// Graph owns one blob-backed dagql cache.
type Graph struct {
	Cache *dagql.Cache
}

// New opens the blob-backed dagql cache persisted at path. The path is
// required, and its parent directory is created if it does not exist.
func New(ctx context.Context, path string) (*Graph, error) {
	if path == "" {
		return nil, errors.New("graph: a database path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("graph: create database directory: %w", err)
	}
	cache, err := dagql.NewCache(ctx, path, nil, nil, dagql.WithBlobStore())
	if err != nil {
		return nil, fmt.Errorf("graph: open dagql cache: %w", err)
	}
	return &Graph{Cache: cache}, nil
}

// Checkpoint saves the cache's state and truncates its write-ahead log.
func (g *Graph) Checkpoint(ctx context.Context) error {
	if g == nil || g.Cache == nil {
		return errors.New("graph: checkpoint: no cache")
	}
	return g.Cache.Checkpoint(ctx)
}

// Close closes the cache, persisting its state. It is safe on a nil Graph.
func (g *Graph) Close(ctx context.Context) error {
	if g == nil || g.Cache == nil {
		return nil
	}
	return g.Cache.Close(ctx)
}

// StoredPart is one verified publication of a merged bundle's completed
// output: the cloud entry it belongs to, the offer to store, and the copy's
// expiry, which is the exported copy's own expiry from the bundle (0: none).
type StoredPart struct {
	Number uint64
	Part   dagql.PersistedPartOffer
	// ExpiresAtUnix is the exported copy's own expiry, from the bundle's
	// value for the output's ordinal. SetStoredPart takes it as
	// copyExpiresAtUnix and admits the part against it: an expired copy
	// attaches only while the entry's value has expired too. It is not the
	// kept target's expiry the reply reports.
	ExpiresAtUnix int64
}

// Output states, as the pinned dagql declares them.
const (
	outputStateCompleted = "completed"
	outputStatePending   = "pending"
	outputStateAbsent    = "absent"
	outputStateMetadata  = "metadata"
)

// RelocateOutputs rewrites a merged bundle's completed outputs for verified
// publication: every bundle ordinal the outputs reference, in their owners'
// dependency IDs and their values' service IDs, is mapped through the reply's
// merged values to the final cloud numbers. It returns one StoredPart per
// published output; pending, absent and metadata outputs are skipped, and a
// pending output is never read as completed even when all its pointers are
// set. A completed output whose required value, chain or owner pointer is
// nil is refused, not silently accepted; so is any reference without exactly
// one mapping, a mapping to zero, or an unknown output state.
//
// A completed output whose own ordinal has no merged value is skipped, not
// refused: the merge skipped its root as expired, with whatever only it
// needed, and the reply reports no target for it. A published output's
// reference to an omitted ordinal is still refused: publication must not name
// an entry the merge didn't leave.
//
// Two distinct ordinals can legitimately map to one final number, when their
// recipes coincide and the merge keeps one entry for both records; the
// published owner's dependencies are then sorted and deduplicated, as the
// merge compacts its own offers' owners, while each service binding keeps its
// own relocated ID.
//
// The returned offers are deep copies: the caller's bundle is left whole,
// and the copies share no memory with it. An empty chain is valid, for a
// scratch output.
func RelocateOutputs(bundle dagql.ValueBundle, reply dagql.MergeReply) ([]StoredPart, error) {
	numbers, err := replyIndex(reply)
	if err != nil {
		return nil, err
	}
	copyExpiries := make(map[dagql.TransferOrdinal]int64, len(bundle.Values))
	for _, value := range bundle.Values {
		if _, dup := copyExpiries[value.Ordinal]; dup {
			return nil, fmt.Errorf("relocate outputs: bundle repeats ordinal %d", value.Ordinal)
		}
		copyExpiries[value.Ordinal] = value.ExpiresAtUnix
	}
	var parts []StoredPart
	for _, output := range bundle.Outputs {
		switch output.State {
		case outputStateCompleted:
		case outputStatePending, outputStateAbsent, outputStateMetadata:
			continue
		default:
			return nil, fmt.Errorf("relocate outputs: invalid output state %q", output.State)
		}
		if output.Value == nil || output.Chain == nil || output.Owner == nil {
			return nil, fmt.Errorf(
				"relocate outputs: completed output %d:%s: missing %s",
				output.Ordinal, output.Address.Part, missingPointer(output))
		}
		number, ok := numbers[output.Ordinal]
		if !ok {
			// The merge skipped this output's root as expired, with
			// whatever only it needed: there is no target to publish.
			continue
		}
		if number == 0 {
			return nil, fmt.Errorf("relocate outputs: output %d: merged to number zero", output.Ordinal)
		}
		part, err := cloneOffer(dagql.PersistedPartOffer{
			Address: output.Address,
			Value:   *output.Value,
			Chain:   *output.Chain,
			Owner:   *output.Owner,
		})
		if err != nil {
			return nil, fmt.Errorf("relocate outputs: output %d:%s: %w", output.Ordinal, output.Address.Part, err)
		}
		if err := relocateIDs(part.Owner.DependencyIDs, numbers, "owner dependency"); err != nil {
			return nil, fmt.Errorf("relocate outputs: output %d:%s: %w", output.Ordinal, output.Address.Part, err)
		}
		slices.Sort(part.Owner.DependencyIDs)
		part.Owner.DependencyIDs = slices.Compact(part.Owner.DependencyIDs)
		for i := range part.Value.Services {
			service := part.Value.Services[i].ServiceResultID
			if err := relocateIDs([]uint64{service}, numbers, "service"); err != nil {
				return nil, fmt.Errorf("relocate outputs: output %d:%s: %w", output.Ordinal, output.Address.Part, err)
			}
			part.Value.Services[i].ServiceResultID = numbers[dagql.TransferOrdinal(service)]
		}
		expiry, ok := copyExpiries[output.Ordinal]
		if !ok {
			return nil, fmt.Errorf("relocate outputs: output %d: no bundle value for its own ordinal", output.Ordinal)
		}
		parts = append(parts, StoredPart{
			Number:        number,
			Part:          part,
			ExpiresAtUnix: expiry,
		})
	}
	return parts, nil
}

// replyIndex indexes the reply's merged values by ordinal: each ordinal's
// final number. A repeated ordinal is refused: publication would then name an
// entry ambiguously. Two distinct ordinals that collapsed onto one number are
// not refused: when recipes coincide, the merge keeps one entry for both
// records, and the reply legitimately reports both ordinals with that number.
func replyIndex(reply dagql.MergeReply) (map[dagql.TransferOrdinal]uint64, error) {
	numbers := make(map[dagql.TransferOrdinal]uint64, len(reply.Values))
	for _, merged := range reply.Values {
		if previous, dup := numbers[merged.Ordinal]; dup {
			return nil, fmt.Errorf("relocate outputs: reply merges ordinal %d twice (numbers %d and %d)",
				merged.Ordinal, previous, merged.Number)
		}
		numbers[merged.Ordinal] = merged.Number
	}
	return numbers, nil
}

// relocateIDs rewrites each bundle ordinal to its final number in place.
func relocateIDs(ids []uint64, numbers map[dagql.TransferOrdinal]uint64, what string) error {
	for i, id := range ids {
		if id == 0 {
			return fmt.Errorf("zero %s reference", what)
		}
		number, ok := numbers[dagql.TransferOrdinal(id)]
		if !ok {
			return fmt.Errorf("%s ordinal %d has no merged value", what, id)
		}
		if number == 0 {
			return fmt.Errorf("%s ordinal %d merged to number zero", what, id)
		}
		ids[i] = number
	}
	return nil
}

// missingPointer names the first required pointer a completed output lacks.
func missingPointer(output dagql.TransferredOutput) string {
	switch {
	case output.Value == nil:
		return "value"
	case output.Chain == nil:
		return "chain"
	default:
		return "owner"
	}
}

// cloneOffer deep-copies an offer: every nested map, slice and pointer,
// before reference relocation changes the copy. It mirrors dagql's own offer
// copy, which is not exported.
func cloneOffer(offer dagql.PersistedPartOffer) (dagql.PersistedPartOffer, error) {
	offer.Address.OutputPath = slices.Clone(offer.Address.OutputPath)
	offer.Value.Platform = clonePlatform(offer.Value.Platform)
	offer.Value.Services = slices.Clone(offer.Value.Services)
	for i := range offer.Value.Services {
		offer.Value.Services[i].Aliases = slices.Clone(offer.Value.Services[i].Aliases)
	}
	offer.Chain.Layers = slices.Clone(offer.Chain.Layers)
	for i := range offer.Chain.Layers {
		layer := &offer.Chain.Layers[i]
		layer.Descriptor.URLs = slices.Clone(layer.Descriptor.URLs)
		layer.Descriptor.Annotations = maps.Clone(layer.Descriptor.Annotations)
		layer.Descriptor.Data = slices.Clone(layer.Descriptor.Data)
		layer.Descriptor.Platform = clonePlatform(layer.Descriptor.Platform)
		if layer.CreatedAt != nil {
			if _, err := layer.CreatedAt.MarshalJSON(); err != nil {
				return dagql.PersistedPartOffer{}, fmt.Errorf("copy layer created time: %w", err)
			}
			createdAt := *layer.CreatedAt
			layer.CreatedAt = &createdAt
		}
	}
	offer.Chain.Addresses = maps.Clone(offer.Chain.Addresses)
	offer.Owner.DependencyIDs = slices.Clone(offer.Owner.DependencyIDs)
	return offer, nil
}

func clonePlatform(platform *ocispecs.Platform) *ocispecs.Platform {
	if platform == nil {
		return nil
	}
	cloned := *platform
	cloned.OSFeatures = slices.Clone(platform.OSFeatures)
	return &cloned
}

// compile-time anchors for the pinned types the contract names.
var (
	_ snapshots.ExportLayer
	_ time.Time
)
