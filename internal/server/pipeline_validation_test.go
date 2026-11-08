package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/dagger/dagger/dagql"
	"github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/require"
)

// existingBlobStore reports every blob as already existing upstream, and
// verifies successfully, so planUpload should classify layerOne as existing.
type existingBlobStore struct {
	fakeStore
	verifyCalls int
}

func (e *existingBlobStore) Exists(ctx context.Context, dgst string, size int64) (bool, error) {
	return true, nil
}

func (e *existingBlobStore) Verify(ctx context.Context, dgst string, size int64) error {
	e.verifyCalls++
	return nil
}

// TestPlanUploadExistingBlob checks the new planUpload signature: blobs that
// already exist upstream are returned in the third slice, not claimed.
func TestPlanUploadExistingBlob(t *testing.T) {
	ctx := context.Background()
	store := &existingBlobStore{}
	srv := &Server{
		store:     store,
		inflight:  map[string]int{},
		verifySem: make(chan struct{}, 1),
	}

	urls, claimed, existing := srv.planUpload(ctx, map[digest.Digest]int64{
		layerOne: 123,
	})

	require.True(t, len(urls) == 0, "expected no upload URLs for existing blob")
	require.True(t, len(claimed) == 0, "expected no claimed blobs for existing blob")
	require.True(t, len(existing) == 1 && existing[0] == layerOne,
		"expected layerOne to be reported as existing")

	verified := srv.verifyUploaded(ctx, existing, map[digest.Digest]int64{
		layerOne: 123,
	})
	require.True(t, verified[layerOne], "expected existing blob to verify")
	require.True(t, store.verifyCalls == 1, "expected Verify to be called exactly once")
}

// TestCapBundleKeepsDependencyClosure checks that capBundle does not drop
// non-root values that are still referenced by retained values.
func TestCapBundleKeepsDependencyClosure(t *testing.T) {
	bundle := dagql.ValueBundle{
		Version: 3,
		Roots: []dagql.TransferredRoot{
			{Ordinal: 1},
			{Ordinal: 2},
		},
		Values: []dagql.TransferredValue{
			{Ordinal: 1, SenderNumber: 1, DependencyIDs: []uint64{3}},
			{Ordinal: 2, SenderNumber: 2},
			{Ordinal: 3, SenderNumber: 3},
		},
	}
	raw, err := json.Marshal(bundle)
	require.True(t, err == nil, "marshal bundle: %v", err)
	max := len(raw) - 1

	capped, err := capBundle(bundle, max)
	require.NoError(t, err)
	out, err := json.Marshal(capped)
	require.True(t, err == nil, "marshal capped bundle: %v", err)
	require.True(t, len(out) <= max, "capped bundle serialized len %d > max %d", len(out), max)

	if len(capped.Roots) > 0 {
		found := false
		for _, v := range capped.Values {
			if v.Ordinal == 3 {
				found = true
			}
		}
		require.True(t, found, "ordinal 3 dropped from Values despite being a dependency of retained root 1")
	}
}

func TestBundleDonorExpiryUsesStoredDonor(t *testing.T) {
	bundle := dagql.ValueBundle{Values: []dagql.TransferredValue{
		{Ordinal: 1, SenderNumber: 11, ExpiresAtUnix: 1000},
		{Ordinal: 2, SenderNumber: 22, ExpiresAtUnix: 2000},
	}}
	expiry, ok := bundleDonorExpiry(bundle, 22)
	require.True(t, ok)
	require.Equal(t, int64(2000), expiry)
	_, ok = bundleDonorExpiry(bundle, 99)
	require.False(t, ok)
	bundle.Values[1].ExpiresAtUnix = 0
	expiry, ok = bundleDonorExpiry(bundle, 22)
	require.True(t, ok)
	require.Zero(t, expiry)
}
