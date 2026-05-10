package headscale_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/paul/flexctl/internal/headscale"
)

func TestGenerateACL_EmptyUsers(t *testing.T) {
	out, err := headscale.GenerateACL(nil)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.Empty(t, got["tagOwners"])
	require.Empty(t, got["acls"])
}

func TestGenerateACL_SingleUser(t *testing.T) {
	out, err := headscale.GenerateACL([]string{"paul"})
	require.NoError(t, err)
	require.Contains(t, out, `"tag:device-paul"`)
	require.Contains(t, out, `"tag:env-paul"`)
	require.Contains(t, out, `"control-plane"`)

	var got struct {
		TagOwners map[string][]string `json:"tagOwners"`
		ACLs      []struct {
			Action string   `json:"action"`
			Src    []string `json:"src"`
			Dst    []string `json:"dst"`
		} `json:"acls"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got))

	require.Equal(t, []string{"control-plane"}, got.TagOwners["tag:device-paul"])
	require.Equal(t, []string{"control-plane"}, got.TagOwners["tag:env-paul"])
	require.Len(t, got.ACLs, 1)
	require.Equal(t, "accept", got.ACLs[0].Action)
	require.Equal(t, []string{"tag:device-paul"}, got.ACLs[0].Src)
	require.Equal(t, []string{"tag:env-paul:22"}, got.ACLs[0].Dst)
}

func TestGenerateACL_TwoUsersDeterministicOrder(t *testing.T) {
	a, err := headscale.GenerateACL([]string{"paul", "alice"})
	require.NoError(t, err)
	b, err := headscale.GenerateACL([]string{"alice", "paul"})
	require.NoError(t, err)
	require.Equal(t, a, b, "output must be sorted by slug to be deterministic")

	// Both users represented
	require.Contains(t, a, "tag:device-paul")
	require.Contains(t, a, "tag:device-alice")

	// alice should appear before paul (lexicographic)
	require.True(t, strings.Index(a, "tag:device-alice") < strings.Index(a, "tag:device-paul"))
}

func TestGenerateACL_RejectsBadSlug(t *testing.T) {
	_, err := headscale.GenerateACL([]string{"Paul"}) // uppercase
	require.ErrorIs(t, err, headscale.ErrInvalidSlug)

	_, err = headscale.GenerateACL([]string{"paul space"})
	require.ErrorIs(t, err, headscale.ErrInvalidSlug)
}
