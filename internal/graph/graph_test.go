package graph

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dagger/dagger/dagql"
	"github.com/dagger/dagger/engine/snapshots"
	"github.com/opencontainers/go-digest"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
)

// bigNumber1 and bigNumber2 sit just past 2^53, where float64 rounding would
// corrupt exact publication.
const (
	bigNumber1 = uint64(9007199254740993) // 2^53 + 1
	bigNumber2 = uint64(9007199254740994) // 2^53 + 2
)

var (
	structuralPath = dagql.PersistedRefPath{{Field: "export"}, {Index: 3, IsIndex: true}}
	blobDigest     = digest.Digest("sha256:" + strings.Repeat("cd", 32))
	layerDigest    = digest.Digest("sha256:" + strings.Repeat("ab", 32))
)

// relocatedFixture builds a bundle whose completed output names owner
// dependency and service ordinals, and a reply that relocates both ordinals
// to numbers past 2^53. The bundle values carry the exported copies' own
// expiries, which is what SetStoredPart admits a stored part against; the
// reply carries the kept cloud targets' expiries, which it does not.
func relocatedFixture() (dagql.ValueBundle, dagql.MergeReply) {
	createdAt := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	bundle := dagql.ValueBundle{
		Version: 3,
		Values: []dagql.TransferredValue{
			{Ordinal: 1, SenderNumber: 101, ExpiresAtUnix: 1600000000},
			{Ordinal: 2, SenderNumber: 202, ExpiresAtUnix: 1600000100},
		},
		Outputs: []dagql.TransferredOutput{
			{
				Ordinal: 1,
				Address: dagql.PersistedPartAddress{OutputPath: structuralPath, Part: "snapshot"},
				State:   "completed",
				Value: &dagql.SnapshotValue{
					Kind:     "directory",
					Platform: &ocispecs.Platform{Architecture: "amd64", OS: "linux", OSFeatures: []string{"sse4"}},
					Services: []dagql.TransferredServiceBinding{
						{ServiceResultID: 2, Hostname: "svc.test", Aliases: []string{"first", "second"}},
					},
				},
				Chain: &dagql.OfferedChain{
					Layers: []snapshots.ExportLayer{{
						Description: "layer one",
						CreatedAt:   &createdAt,
						Descriptor: ocispecs.Descriptor{
							MediaType:   "application/vnd.oci.image.layer.v1.tar",
							Digest:      layerDigest,
							Size:        32,
							URLs:        []string{"https://example.test/layer"},
							Annotations: map[string]string{"org.test": "one"},
							Data:        []byte{9, 8, 7},
							Platform:    &ocispecs.Platform{Architecture: "arm64", OS: "linux", OSFeatures: []string{"crc32"}},
						},
					}},
					Addresses: map[digest.Digest]dagql.BlobAddress{
						blobDigest: {URL: "https://blob.test/x", ExpiresAtUnix: 4321},
					},
					RenewalKey: "renew-me",
				},
				Owner: &dagql.PersistedOfferOwner{DependencyIDs: []uint64{2}},
			},
		},
	}
	reply := dagql.MergeReply{
		Values: []dagql.MergedValue{
			{Ordinal: 1, Number: bigNumber1, ExpiresAtUnix: 1700000000},
			{Ordinal: 2, Number: bigNumber2, ExpiresAtUnix: 1800000000},
		},
	}
	return bundle, reply
}

func TestRelocateOutputsMapsOwnersAndServices(t *testing.T) {
	bundle, reply := relocatedFixture()
	parts, err := RelocateOutputs(bundle, reply)
	require.NoError(t, err)
	require.Len(t, parts, 1)

	part := parts[0]
	require.Equal(t, bigNumber1, part.Number)
	// The published expiry is the exported copy's own expiry from the
	// bundle, not the kept target's expiry the reply reports:
	// SetStoredPart admits the part against the copy's expiry.
	require.Equal(t, int64(1600000000), part.ExpiresAtUnix)
	require.Equal(t, []uint64{bigNumber2}, part.Part.Owner.DependencyIDs)
	require.Len(t, part.Part.Value.Services, 1)
	require.Equal(t, bigNumber2, part.Part.Value.Services[0].ServiceResultID)
	require.Equal(t, "svc.test", part.Part.Value.Services[0].Hostname)
	require.Equal(t, []string{"first", "second"}, part.Part.Value.Services[0].Aliases)
	// The address travels whole, including its structural path.
	require.Equal(t, bundle.Outputs[0].Address, part.Part.Address)
}

// TestRelocateOutputsPublishesCopyExpiryNotTargetExpiry pins the distinction
// the first version of this package got backwards: SetStoredPart's
// copyExpiresAtUnix is the uploaded copy's expiry, so an expired exported
// copy keeps its old expiry even when the merge kept a fresh target. The
// cloud then sees the copy as expired and refuses to admit it against an
// unexpired value, instead of wrongly admitting it forever.
func TestRelocateOutputsPublishesCopyExpiryNotTargetExpiry(t *testing.T) {
	bundle, reply := relocatedFixture()
	bundle.Values[0].ExpiresAtUnix = 1000      // the exported copy expired long ago
	reply.Values[0].ExpiresAtUnix = 1900000000 // the kept target is fresh
	parts, err := RelocateOutputs(bundle, reply)
	require.NoError(t, err)
	require.Len(t, parts, 1)
	require.Equal(t, int64(1000), parts[0].ExpiresAtUnix)
}

func TestRelocateOutputsRefusesMissingMapping(t *testing.T) {
	t.Run("owner dependency ordinal absent from the reply", func(t *testing.T) {
		bundle, reply := relocatedFixture()
		bundle.Outputs[0].Owner.DependencyIDs = []uint64{7}
		_, err := RelocateOutputs(bundle, reply)
		require.Error(t, err)
	})

	t.Run("service ordinal absent from the reply", func(t *testing.T) {
		bundle, reply := relocatedFixture()
		bundle.Outputs[0].Value.Services[0].ServiceResultID = 9
		_, err := RelocateOutputs(bundle, reply)
		require.Error(t, err)
	})
}

// TestRelocateOutputsRefusesRepeatedOrdinal keeps the duplicate-ordinal
// refusal. The old test here also refused two ordinals that collapse onto one
// final number; that refusal was wrong and moved to
// TestRelocateOutputsAllowsSharedFinalNumber: upstream lands two records of
// one recipe on one entry (cache_value_merge.go's best-record decision), so
// the reply legitimately reports distinct ordinals with one number.
func TestRelocateOutputsRefusesRepeatedOrdinal(t *testing.T) {
	t.Run("reply repeats an ordinal", func(t *testing.T) {
		bundle, reply := relocatedFixture()
		reply.Values = append(reply.Values, dagql.MergedValue{Ordinal: 2, Number: bigNumber2})
		_, err := RelocateOutputs(bundle, reply)
		require.Error(t, err)
	})
}

// TestRelocateOutputsAllowsSharedFinalNumber replaces the old refusal of two
// ordinals mapping to one number: when recipes coincide, the merge keeps one
// entry for both records, and publication must follow the reply.
func TestRelocateOutputsAllowsSharedFinalNumber(t *testing.T) {
	bundle, reply := relocatedFixture()
	reply.Values[1].Number = bigNumber1
	// Two service bindings naming the two ordinals both land on the one
	// entry; per-binding service IDs are kept as they are, not deduped.
	bundle.Outputs[0].Value.Services = []dagql.TransferredServiceBinding{
		{ServiceResultID: 2, Hostname: "svc.test", Aliases: []string{"first"}},
		{ServiceResultID: 2, Hostname: "svc.test", Aliases: []string{"second"}},
	}
	parts, err := RelocateOutputs(bundle, reply)
	require.NoError(t, err)
	require.Len(t, parts, 1)
	require.Equal(t, bigNumber1, parts[0].Number)
	require.Equal(t, []uint64{bigNumber1}, parts[0].Part.Owner.DependencyIDs)
	require.Len(t, parts[0].Part.Value.Services, 2)
	require.Equal(t, bigNumber1, parts[0].Part.Value.Services[0].ServiceResultID)
	require.Equal(t, bigNumber1, parts[0].Part.Value.Services[1].ServiceResultID)
}

// TestRelocateOutputsSortsAndDedupsOwnerDependencies pins the owner
// compaction the merge's own offer compaction does: after relocation the
// owner's dependencies are sorted, and aliases that coalesced onto one final
// number are listed once.
func TestRelocateOutputsSortsAndDedupsOwnerDependencies(t *testing.T) {
	t.Run("distinct numbers come out sorted", func(t *testing.T) {
		bundle, reply := relocatedFixture()
		bundle.Outputs[0].Owner.DependencyIDs = []uint64{2, 1}
		parts, err := RelocateOutputs(bundle, reply)
		require.NoError(t, err)
		require.Len(t, parts, 1)
		require.Equal(t, []uint64{bigNumber1, bigNumber2}, parts[0].Part.Owner.DependencyIDs)
	})

	t.Run("aliases of one final number are listed once", func(t *testing.T) {
		bundle, reply := relocatedFixture()
		bundle.Outputs[0].Owner.DependencyIDs = []uint64{2, 1}
		reply.Values[1].Number = bigNumber1
		parts, err := RelocateOutputs(bundle, reply)
		require.NoError(t, err)
		require.Len(t, parts, 1)
		require.Equal(t, []uint64{bigNumber1}, parts[0].Part.Owner.DependencyIDs)
	})
}

func TestRelocateOutputsRefusesZeroIDs(t *testing.T) {
	t.Run("mapped owner dependency is zero", func(t *testing.T) {
		bundle, reply := relocatedFixture()
		reply.Values[1].Number = 0
		_, err := RelocateOutputs(bundle, reply)
		require.Error(t, err)
	})

	t.Run("mapped own number is zero", func(t *testing.T) {
		bundle, reply := relocatedFixture()
		reply.Values[0].Number = 0
		_, err := RelocateOutputs(bundle, reply)
		require.Error(t, err)
	})
}

func TestRelocateOutputsDeepCopies(t *testing.T) {
	t.Run("mutating the published copy leaves the bundle whole", func(t *testing.T) {
		bundle, reply := relocatedFixture()
		parts, err := RelocateOutputs(bundle, reply)
		require.NoError(t, err)
		require.Len(t, parts, 1)
		part := parts[0].Part

		part.Owner.DependencyIDs[0] = 1
		part.Value.Services[0].ServiceResultID = 1
		part.Value.Services[0].Aliases[0] = "mutated"
		part.Value.Platform.OSFeatures[0] = "mutated"
		part.Chain.Addresses[blobDigest] = dagql.BlobAddress{URL: "mutated"}
		part.Chain.Layers[0].Descriptor.Annotations["org.test"] = "mutated"
		part.Chain.Layers[0].Descriptor.URLs[0] = "mutated"
		part.Chain.Layers[0].Descriptor.Data[0] = 0
		part.Chain.Layers[0].Descriptor.Platform.OSFeatures[0] = "mutated"
		*part.Chain.Layers[0].CreatedAt = time.Time{}
		part.Address.OutputPath[0].Field = "mutated"

		original := bundle.Outputs[0]
		require.Equal(t, uint64(2), original.Owner.DependencyIDs[0])
		require.Equal(t, uint64(2), original.Value.Services[0].ServiceResultID)
		require.Equal(t, "first", original.Value.Services[0].Aliases[0])
		require.Equal(t, "sse4", original.Value.Platform.OSFeatures[0])
		require.Equal(t, "https://blob.test/x", original.Chain.Addresses[blobDigest].URL)
		require.Equal(t, "one", original.Chain.Layers[0].Descriptor.Annotations["org.test"])
		require.Equal(t, "https://example.test/layer", original.Chain.Layers[0].Descriptor.URLs[0])
		require.Equal(t, []byte{9, 8, 7}, original.Chain.Layers[0].Descriptor.Data)
		require.Equal(t, "crc32", original.Chain.Layers[0].Descriptor.Platform.OSFeatures[0])
		require.False(t, original.Chain.Layers[0].CreatedAt.IsZero())
		require.Equal(t, "export", original.Address.OutputPath[0].Field)
	})

	t.Run("mutating the bundle leaves the published copy whole", func(t *testing.T) {
		bundle, reply := relocatedFixture()
		parts, err := RelocateOutputs(bundle, reply)
		require.NoError(t, err)
		require.Len(t, parts, 1)

		bundle.Outputs[0].Owner.DependencyIDs[0] = 99
		bundle.Outputs[0].Value.Services[0].Aliases[0] = "mutated"
		bundle.Outputs[0].Chain.Addresses[blobDigest] = dagql.BlobAddress{URL: "mutated"}

		require.Equal(t, bigNumber2, parts[0].Part.Owner.DependencyIDs[0])
		require.Equal(t, "first", parts[0].Part.Value.Services[0].Aliases[0])
		require.Equal(t, "https://blob.test/x", parts[0].Part.Chain.Addresses[blobDigest].URL)
	})
}

func TestRelocateOutputsSkipsUnpublishableOutputs(t *testing.T) {
	bundle, reply := relocatedFixture()
	completed := bundle.Outputs[0]
	// An accepted offer: every pointer set, but still pending. It must not
	// be read as completed.
	bundle.Outputs = append(bundle.Outputs,
		dagql.TransferredOutput{
			Ordinal: 2,
			Address: dagql.PersistedPartAddress{Part: "snapshot"},
			State:   "pending",
			Value:   completed.Value,
			Chain:   completed.Chain,
			Owner:   completed.Owner,
		},
		dagql.TransferredOutput{Ordinal: 2, Address: dagql.PersistedPartAddress{Part: "metadata"}, State: "metadata"},
		dagql.TransferredOutput{Ordinal: 2, Address: dagql.PersistedPartAddress{Part: "snapshot"}, State: "absent"},
	)
	parts, err := RelocateOutputs(bundle, reply)
	require.NoError(t, err)
	require.Len(t, parts, 1)
	require.Equal(t, bigNumber1, parts[0].Number)
}

// TestRelocateOutputsSkipsOutputsTheReplyOmits pins the distinction between
// an output whose own ordinal the reply omits and a reference to an omitted
// ordinal. An output whose own ordinal has no merged value belongs to a root
// the merge skipped as expired, with whatever only it needed: it is skipped,
// not refused. A published output's reference to an omitted ordinal is still
// refused: publication must not name an entry the merge didn't leave.
// (The old suite refused the own-ordinal case; that expectation encoded the
// wrong contract and is superseded here, not deleted.)
func TestRelocateOutputsSkipsOutputsTheReplyOmits(t *testing.T) {
	t.Run("a skipped output is omitted and a valid one still publishes", func(t *testing.T) {
		bundle, reply := relocatedFixture()
		skipped := bundle.Outputs[0]
		skipped.Ordinal = 3
		skipped.Address = dagql.PersistedPartAddress{Part: "snapshot"}
		bundle.Outputs = append(bundle.Outputs, skipped)
		// The reply keeps both values: the skipped output's own ordinal 3 is
		// absent, while the published output's reference to ordinal 2 stays
		// mapped.
		parts, err := RelocateOutputs(bundle, reply)
		require.NoError(t, err)
		require.Len(t, parts, 1)
		require.Equal(t, bigNumber1, parts[0].Number)
	})

	t.Run("an output alone with an omitted ordinal publishes nothing", func(t *testing.T) {
		bundle, reply := relocatedFixture()
		reply.Values = reply.Values[1:]
		parts, err := RelocateOutputs(bundle, reply)
		require.NoError(t, err)
		require.Empty(t, parts)
	})

	t.Run("a published output's reference to an omitted ordinal is refused", func(t *testing.T) {
		bundle, reply := relocatedFixture()
		bundle.Outputs[0].Owner.DependencyIDs = []uint64{2}
		reply.Values = reply.Values[:1]
		_, err := RelocateOutputs(bundle, reply)
		require.Error(t, err)
	})
}

func TestRelocateOutputsPublishesScratchOutput(t *testing.T) {
	bundle, reply := relocatedFixture()
	bundle.Outputs = []dagql.TransferredOutput{{
		Ordinal: 1,
		Address: dagql.PersistedPartAddress{Part: "snapshot"},
		State:   "completed",
		Value:   &dagql.SnapshotValue{Kind: "directory"},
		Chain:   &dagql.OfferedChain{},
		Owner:   &dagql.PersistedOfferOwner{},
	}}
	parts, err := RelocateOutputs(bundle, reply)
	require.NoError(t, err)
	require.Len(t, parts, 1)
	require.Equal(t, bigNumber1, parts[0].Number)
	require.Empty(t, parts[0].Part.Chain.Layers)
	require.Empty(t, parts[0].Part.Owner.DependencyIDs)
}

func TestRelocateOutputsRefusesInconsistentCompletedOutput(t *testing.T) {
	base, reply := relocatedFixture()
	cases := []struct {
		name   string
		mutate func(*dagql.TransferredOutput)
	}{
		{"nil chain", func(o *dagql.TransferredOutput) { o.Chain = nil }},
		{"nil value", func(o *dagql.TransferredOutput) { o.Value = nil }},
		{"nil owner", func(o *dagql.TransferredOutput) { o.Owner = nil }},
		{"unknown state", func(o *dagql.TransferredOutput) { o.State = "accepted" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bundle := base
			out := bundle.Outputs[0]
			tc.mutate(&out)
			bundle.Outputs = []dagql.TransferredOutput{out}
			_, err := RelocateOutputs(bundle, reply)
			require.Error(t, err)
		})
	}
}

func TestRelocateOutputsMatchesStructuralPaths(t *testing.T) {
	bundle, reply := relocatedFixture()
	pathless := bundle.Outputs[0]
	pathless.Address.OutputPath = nil
	bundle.Outputs = append(bundle.Outputs, pathless)

	parts, err := RelocateOutputs(bundle, reply)
	require.NoError(t, err)
	require.Len(t, parts, 2)
	require.Equal(t, structuralPath, parts[0].Part.Address.OutputPath)
	require.Nil(t, parts[1].Part.Address.OutputPath)
	require.Equal(t, dagql.PartKey("snapshot"), parts[0].Part.Address.Part)
	require.Equal(t, parts[0].Part.Address.Part, parts[1].Part.Address.Part)
	require.Equal(t, bigNumber1, parts[0].Number)
	require.Equal(t, bigNumber1, parts[1].Number)
}

func TestGraphLifecycleOnSQLite(t *testing.T) {
	ctx := context.Background()
	// The parent directory does not exist yet; New must create it.
	path := filepath.Join(t.TempDir(), "nested", "cache.db")

	g, err := New(ctx, path)
	require.NoError(t, err)
	require.NotNil(t, g.Cache)
	require.NoError(t, g.Checkpoint(ctx))
	require.NoError(t, g.Close(ctx))

	reopened, err := New(ctx, path)
	require.NoError(t, err)
	require.NotNil(t, reopened.Cache)
	require.NoError(t, reopened.Checkpoint(ctx))
	require.NoError(t, reopened.Close(ctx))
}

func TestNewRejectsEmptyPath(t *testing.T) {
	g, err := New(context.Background(), "")
	require.Error(t, err)
	require.Nil(t, g)
}
