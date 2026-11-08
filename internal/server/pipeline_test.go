package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dagger/dagger/dagql"
	"github.com/dagger/dagger/engine/remotecache/protocol"
	"github.com/dagger/dagger/engine/snapshots"
	"github.com/opencontainers/go-digest"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"

	"github.com/snoby/dcache/internal/renewal"
	"github.com/snoby/dcache/internal/storage"
)

const (
	bigNumber1 = uint64(9007199254740993) // 2^53 + 1
	bigNumber2 = uint64(9007199254740994) // 2^53 + 2
)

var (
	layerOne = digest.Digest("sha256:" + strings.Repeat("ab", 32))
	layerTwo = digest.Digest("sha256:" + strings.Repeat("cd", 32))
)

func ocispecDescriptor(d digest.Digest, size int64) ocispecs.Descriptor {
	return ocispecs.Descriptor{MediaType: "application/vnd.oci.image.layer.v1.tar", Digest: d, Size: size}
}

// fakeStore is a minimal BlobStore for pure helper tests.
type fakeStore struct{}

func (f *fakeStore) PresignPut(context.Context, string, int64) (string, error) {
	return "", nil
}
func (f *fakeStore) PresignGet(_ context.Context, d string) (string, time.Time, error) {
	return "https://get/" + d, time.Unix(1234, 0), nil
}
func (f *fakeStore) Exists(context.Context, string, int64) (bool, error) { return false, nil }
func (f *fakeStore) Verify(context.Context, string, int64) error         { return nil }
func (f *fakeStore) List(context.Context) ([]storage.Object, error)      { return nil, nil }
func (f *fakeStore) Delete(context.Context, string) error                { return nil }

func TestMergedToReplyPreservesUint64Precision(t *testing.T) {
	m := protocol.Merged{
		Generation: 7,
		EngineTime: 123,
		Values: []protocol.MergedValue{{
			Ordinal:       1,
			Number:        bigNumber1,
			Replacements:  2,
			ExpiresAtUnix: 99,
			Deps:          []uint64{bigNumber2},
			Complete:      []dagql.PersistedPartAddress{{Part: "snapshot"}},
			Offered:       []dagql.PersistedPartAddress{{Part: "metadata"}},
		}},
		Retained: []protocol.RetainedRoot{{Ordinal: 1, ExpiresAtUnix: 100}},
		Skipped:  []protocol.SkippedValue{{Ordinal: 2, Reason: protocol.SkipExpired}},
	}
	reply := mergedToReply(m)
	require.Equal(t, bigNumber1, reply.Values[0].Number)
	require.Equal(t, bigNumber2, reply.Values[0].Deps[0])
	require.Equal(t, uint64(7), reply.Generation)
	require.Equal(t, int64(123), reply.EngineTimeUnixNano)
	require.True(t, reply.Committed)
	require.Len(t, reply.Roots, 2)
	require.Equal(t, bigNumber1, reply.Roots[0].Number)
	require.True(t, reply.Roots[0].Retained)
	require.Equal(t, int64(100), reply.Roots[0].RetentionExpiresAtUnix)
	require.True(t, reply.Roots[1].Expired)
}

func TestValidateRenewClaims(t *testing.T) {
	layers := []snapshots.ExportLayer{
		{Descriptor: ocispecDescriptor(layerOne, 10)},
		{Descriptor: ocispecDescriptor(layerTwo, 20)},
	}
	valid := renewal.Claims{Pool: "pool", Donor: 5, Digests: []string{layerOne.String(), layerTwo.String()}}
	req := protocol.Renew{RenewalKey: "k", Layers: layers, NeededBlob: layerOne}
	require.NoError(t, validateRenewClaims(valid, req))

	wrongOrder := valid
	wrongOrder.Digests = []string{layerTwo.String(), layerOne.String()}
	require.Error(t, validateRenewClaims(wrongOrder, req))

	missingNeeded := req
	missingNeeded.NeededBlob = digest.Digest("sha256:" + strings.Repeat("ef", 32))
	require.Error(t, validateRenewClaims(valid, missingNeeded))

	noDonor := valid
	noDonor.Donor = 0
	require.Error(t, validateRenewClaims(noDonor, req))

	short := valid
	short.Digests = []string{layerOne.String()}
	require.Error(t, validateRenewClaims(short, req))
}

func TestCapBundleReducesRoots(t *testing.T) {
	bundle := dagql.ValueBundle{Version: 3}
	for i := uint64(1); i <= 3; i++ {
		bundle.Roots = append(bundle.Roots, dagql.TransferredRoot{Ordinal: dagql.TransferOrdinal(i)})
		bundle.Values = append(bundle.Values, dagql.TransferredValue{
			Ordinal:      dagql.TransferOrdinal(i),
			SenderNumber: i,
			DependencyIDs: []uint64{
				bigNumber1, bigNumber2, bigNumber1, bigNumber2, bigNumber1, bigNumber2,
			},
		})
	}
	capped, err := capBundle(bundle, 400)
	require.NoError(t, err)
	raw, err := json.Marshal(capped)
	require.NoError(t, err)
	require.LessOrEqual(t, len(raw), 400)
	require.Less(t, len(capped.Roots), len(bundle.Roots))
}

func TestExportOutputsExcludeMetadata(t *testing.T) {
	held := dagql.RemoteHoldingInfo{
		Key: dagql.HolderKey{Cache: "engine", Number: 42},
		Parts: []dagql.PersistedPartAddress{
			{Part: "snapshot"},
			{Part: "metadata"},
		},
	}
	outs := exportOutputs(held)
	require.Len(t, outs, 1)
	require.Equal(t, uint64(42), outs[0].Number)
	require.Equal(t, dagql.PartKey("snapshot"), outs[0].Address.Part)
}

func TestSignChainAddsAddressesAndRenewal(t *testing.T) {
	store := &fakeStore{}
	signer, err := renewal.New([]byte(strings.Repeat("s", 32)), "pool", time.Minute)
	require.NoError(t, err)
	chain := dagql.OfferedChain{Layers: []snapshots.ExportLayer{
		{Descriptor: ocispecDescriptor(layerOne, 10)},
		{Descriptor: ocispecDescriptor(layerTwo, 20)},
	}}
	signed, err := signChain(context.Background(), store, signer, 5, chain)
	require.NoError(t, err)
	require.Equal(t, "https://get/"+layerOne.String(), signed.Addresses[layerOne].URL)
	require.Equal(t, int64(1234), signed.Addresses[layerOne].ExpiresAtUnix)
	require.NotEmpty(t, signed.RenewalKey)
	claims, err := signer.Verify(signed.RenewalKey)
	require.NoError(t, err)
	require.Equal(t, uint64(5), claims.Donor)
	require.Equal(t, []string{layerOne.String(), layerTwo.String()}, claims.Digests)
}
