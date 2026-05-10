package headscale

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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
