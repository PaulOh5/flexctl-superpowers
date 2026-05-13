package headscale

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

func NewClient(baseURL, apiKey string, timeout time.Duration) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    &http.Client{Timeout: timeout},
	}
}

var (
	ErrUserAlreadyExists = errors.New("headscale user already exists")
	ErrUserNotFound      = errors.New("headscale user not found")
)

type User struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
}

type listUsersResp struct {
	Users []User `json:"users"`
}

func (c *Client) ListUsers(ctx context.Context) ([]User, error) {
	var out listUsersResp
	if err := c.do(ctx, http.MethodGet, "/api/v1/user", nil, &out); err != nil {
		return nil, err
	}
	return out.Users, nil
}

type createUserReq struct {
	Name string `json:"name"`
}

type userResp struct {
	User User `json:"user"`
}

func (c *Client) CreateUser(ctx context.Context, name string) (User, error) {
	var out userResp
	err := c.do(ctx, http.MethodPost, "/api/v1/user", createUserReq{Name: name}, &out)
	if err != nil {
		if isHeadscaleStatusError(err, http.StatusBadRequest) ||
			isHeadscaleStatusError(err, http.StatusConflict) ||
			strings.Contains(err.Error(), "already exists") {
			return User{}, ErrUserAlreadyExists
		}
		return User{}, err
	}
	return out.User, nil
}

func (c *Client) DeleteUser(ctx context.Context, name string) error {
	err := c.do(ctx, http.MethodDelete, "/api/v1/user/"+name, nil, nil)
	if err != nil {
		if isHeadscaleStatusError(err, http.StatusNotFound) ||
			strings.Contains(err.Error(), "not found") {
			return ErrUserNotFound
		}
		return err
	}
	return nil
}

func isHeadscaleStatusError(err error, status int) bool {
	prefix := fmt.Sprintf(": %d ", status)
	return err != nil && strings.Contains(err.Error(), prefix)
}

type policyReq struct {
	Policy string `json:"policy"`
}

type policyResp struct {
	Policy    string    `json:"policy"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func (c *Client) SetPolicy(ctx context.Context, hujson string) error {
	return c.do(ctx, http.MethodPut, "/api/v1/policy", policyReq{Policy: hujson}, nil)
}

func (c *Client) GetPolicy(ctx context.Context) (string, error) {
	var out policyResp
	if err := c.do(ctx, http.MethodGet, "/api/v1/policy", nil, &out); err != nil {
		return "", err
	}
	return out.Policy, nil
}

type PreAuthKeyRequest struct {
	User       string
	Reusable   bool
	Ephemeral  bool
	Expiration time.Duration
	ACLTags    []string
}

type PreAuthKey struct {
	ID         string    `json:"id"`
	Key        string    `json:"key"`
	Ephemeral  bool      `json:"ephemeral"`
	Reusable   bool      `json:"reusable"`
	Expiration time.Time `json:"expiration"`
	CreatedAt  time.Time `json:"createdAt"`
}

type preAuthKeyHTTPReq struct {
	User       string   `json:"user"`
	Reusable   bool     `json:"reusable"`
	Ephemeral  bool     `json:"ephemeral"`
	Expiration string   `json:"expiration"` // RFC 3339
	ACLTags    []string `json:"aclTags"`
}

type preAuthKeyHTTPResp struct {
	PreAuthKey PreAuthKey `json:"preAuthKey"`
}

func (c *Client) CreatePreAuthKey(ctx context.Context, req PreAuthKeyRequest) (PreAuthKey, error) {
	body := preAuthKeyHTTPReq{
		User: req.User, Reusable: req.Reusable, Ephemeral: req.Ephemeral,
		Expiration: time.Now().Add(req.Expiration).UTC().Format(time.RFC3339),
		ACLTags:    req.ACLTags,
	}
	var out preAuthKeyHTTPResp
	if err := c.do(ctx, http.MethodPost, "/api/v1/preauthkey", body, &out); err != nil {
		return PreAuthKey{}, err
	}
	return out.PreAuthKey, nil
}

type Node struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	GivenName   string   `json:"given_name"`
	User        User     `json:"user"`
	IPAddresses []string `json:"ip_addresses"`
}

type listNodesResp struct {
	Nodes []Node `json:"nodes"`
}

// ListNodes returns all Headscale nodes for a given user (by name). Empty
// user filter returns all nodes (admin scope). Used by devices.Service to
// look up a device's Headscale node ID before deletion.
func (c *Client) ListNodes(ctx context.Context, user string) ([]Node, error) {
	path := "/api/v1/node"
	if user != "" {
		path += "?user=" + url.QueryEscape(user)
	}
	var out listNodesResp
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out.Nodes, nil
}

// DeleteNode removes a Headscale node by its numeric ID (as returned by
// ListNodes). Used by flexctl logout to drop a user device from the tailnet.
func (c *Client) DeleteNode(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/node/"+id, nil, nil)
}

func (c *Client) do(ctx context.Context, method, path string, in any, out any) error {
	var body io.Reader
	if in != nil {
		buf, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("marshal: %w", err)
		}
		body = strings.NewReader(string(buf))
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("do: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}

	if resp.StatusCode >= 400 {
		return fmt.Errorf("headscale %s %s: %d %s", method, path, resp.StatusCode, string(respBody))
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("unmarshal: %w (body=%s)", err, string(respBody))
		}
	}
	return nil
}
