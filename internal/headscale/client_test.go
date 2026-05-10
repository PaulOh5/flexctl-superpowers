package headscale_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/paul/flexctl/internal/headscale"
)

func TestListUsers_InitialHasControlPlane(t *testing.T) {
	baseURL, apiKey := startHeadscale(t)
	c := headscale.NewClient(baseURL, apiKey, 5*time.Second)

	users, err := c.ListUsers(context.Background())
	require.NoError(t, err)

	names := make([]string, 0, len(users))
	for _, u := range users {
		names = append(names, u.Name)
	}
	require.Contains(t, names, "control-plane",
		"testhelpers create control-plane user; should appear in ListUsers")
}
