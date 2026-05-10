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

func TestCreateAndDeleteUser(t *testing.T) {
	baseURL, apiKey := startHeadscale(t)
	c := headscale.NewClient(baseURL, apiKey, 5*time.Second)
	ctx := context.Background()

	u, err := c.CreateUser(ctx, "alice")
	require.NoError(t, err)
	require.Equal(t, "alice", u.Name)
	require.NotEmpty(t, u.ID)

	users, err := c.ListUsers(ctx)
	require.NoError(t, err)
	names := collectNames(users)
	require.Contains(t, names, "alice")

	require.NoError(t, c.DeleteUser(ctx, "alice"))

	users, err = c.ListUsers(ctx)
	require.NoError(t, err)
	require.NotContains(t, collectNames(users), "alice")
}

func TestCreateUser_Duplicate(t *testing.T) {
	baseURL, apiKey := startHeadscale(t)
	c := headscale.NewClient(baseURL, apiKey, 5*time.Second)
	ctx := context.Background()

	_, err := c.CreateUser(ctx, "bob")
	require.NoError(t, err)

	_, err = c.CreateUser(ctx, "bob")
	require.ErrorIs(t, err, headscale.ErrUserAlreadyExists)
}

func TestDeleteUser_NotFound(t *testing.T) {
	baseURL, apiKey := startHeadscale(t)
	c := headscale.NewClient(baseURL, apiKey, 5*time.Second)
	ctx := context.Background()

	err := c.DeleteUser(ctx, "ghost")
	require.ErrorIs(t, err, headscale.ErrUserNotFound)
}

func collectNames(users []headscale.User) []string {
	out := make([]string, 0, len(users))
	for _, u := range users {
		out = append(out, u.Name)
	}
	return out
}
