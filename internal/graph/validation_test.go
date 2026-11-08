package graph

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRelocateOutputsMissingCopyExpirySource verifies that relocation fails
// when an output references a source value that is absent from the bundle.
func TestRelocateOutputsMissingCopyExpirySource(t *testing.T) {
	bundle, reply := relocatedFixture()
	bundle.Values = bundle.Values[1:]

	_, err := RelocateOutputs(bundle, reply)
	require.Error(t, err)
}

// TestRelocateOutputsDuplicateBundleOrdinal verifies that relocation fails
// when duplicate ordinals in the bundle make the expiry source ambiguous.
func TestRelocateOutputsDuplicateBundleOrdinal(t *testing.T) {
	bundle, reply := relocatedFixture()
	bundle.Values = append(bundle.Values, bundle.Values[0])

	_, err := RelocateOutputs(bundle, reply)
	require.Error(t, err)
}

// TestRelocateOutputsExplicitZeroExpiryIsLegitimate verifies that an explicit
// zero expiry on the source value is copied through as a legitimate zero
// rather than being treated as a missing expiry.
func TestRelocateOutputsExplicitZeroExpiryIsLegitimate(t *testing.T) {
	bundle, reply := relocatedFixture()
	bundle.Values[0].ExpiresAtUnix = 0

	outputParts, err := RelocateOutputs(bundle, reply)
	require.NoError(t, err)
	require.Len(t, outputParts, 1)
	require.Equal(t, int64(0), outputParts[0].ExpiresAtUnix)
}
