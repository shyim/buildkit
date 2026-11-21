package remotecache

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseMountsAttr(t *testing.T) {
	for v, want := range map[string]MountsMode{
		"":      MountsDisabled,
		"false": MountsDisabled,
		"0":     MountsDisabled,
		"true":  MountsEnabled,
		"1":     MountsEnabled,
		"only":  MountsOnly,
	} {
		got, err := ParseMountsAttr(map[string]string{AttrMounts: v})
		require.NoError(t, err, v)
		require.Equal(t, want, got, v)
	}

	got, err := ParseMountsAttr(nil)
	require.NoError(t, err)
	require.Equal(t, MountsDisabled, got)

	_, err = ParseMountsAttr(map[string]string{AttrMounts: "bogus"})
	require.ErrorContains(t, err, "expected true, false or only")

	require.True(t, MountsEnabled.Mounts() && MountsEnabled.Layers())
	require.True(t, MountsOnly.Mounts() && !MountsOnly.Layers())
	require.True(t, !MountsDisabled.Mounts() && MountsDisabled.Layers())
}
