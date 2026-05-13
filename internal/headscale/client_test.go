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

func TestSetAndGetPolicy(t *testing.T) {
	baseURL, apiKey := startHeadscale(t)
	c := headscale.NewClient(baseURL, apiKey, 5*time.Second)
	ctx := context.Background()

	want := `{
  "tagOwners": {
    "tag:device-paul": ["control-plane"],
    "tag:env-paul":    ["control-plane"]
  },
  "acls": [
    {"action":"accept","src":["tag:device-paul"],"dst":["tag:env-paul:22"]}
  ]
}`

	require.NoError(t, c.SetPolicy(ctx, want))

	got, err := c.GetPolicy(ctx)
	require.NoError(t, err)
	require.Contains(t, got, "tag:device-paul")
	require.Contains(t, got, "tag:env-paul:22")
}

func TestSetPolicy_Invalid(t *testing.T) {
	baseURL, apiKey := startHeadscale(t)
	c := headscale.NewClient(baseURL, apiKey, 5*time.Second)
	err := c.SetPolicy(context.Background(), `{"this is not valid hujson`)
	require.Error(t, err, "headscale should reject malformed policy")
}

func TestCreatePreAuthKey_Ephemeral(t *testing.T) {
	baseURL, apiKey := startHeadscale(t)
	c := headscale.NewClient(baseURL, apiKey, 5*time.Second)
	ctx := context.Background()

	_, err := c.CreateUser(ctx, "paul")
	require.NoError(t, err)

	key, err := c.CreatePreAuthKey(ctx, headscale.PreAuthKeyRequest{
		User:       "paul",
		Reusable:   false,
		Ephemeral:  true,
		Expiration: 24 * time.Hour,
		ACLTags:    []string{"tag:env-paul"},
	})
	require.NoError(t, err)
	require.NotEmpty(t, key.Key)
	require.True(t, key.Ephemeral)
}

func TestListNodes_EmptyWhenNoNodes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	baseURL, apiKey := startHeadscale(t)
	c := headscale.NewClient(baseURL, apiKey, 5*time.Second)

	_, err := c.CreateUser(ctx, "paul")
	require.NoError(t, err)

	got, err := c.ListNodes(ctx, "paul")
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestDeleteNode_UnknownID_Returns404Error(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	baseURL, apiKey := startHeadscale(t)
	c := headscale.NewClient(baseURL, apiKey, 5*time.Second)

	err := c.DeleteNode(ctx, "9999999")
	require.Error(t, err)
}
