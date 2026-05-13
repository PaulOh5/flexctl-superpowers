package flexctlcli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type APIClient struct {
	base   string
	cookie string
	hc     *http.Client
}

func NewAPIClient(base, cookie string) *APIClient {
	return &APIClient{base: base, cookie: cookie, hc: &http.Client{Timeout: 30 * time.Second}}
}

func (c *APIClient) Cookie() string { return c.cookie }

func (c *APIClient) do(ctx context.Context, method, path string, in any, out any) (*http.Response, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, fmt.Errorf("marshal: %w", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.cookie != "" {
		req.Header.Set("Cookie", c.cookie)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do: %w", err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(resp.Body)
		return resp, fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, string(msg))
	}
	if out != nil {
		defer resp.Body.Close()
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp, fmt.Errorf("decode: %w", err)
		}
	}
	return resp, nil
}

type loginReq struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (c *APIClient) Login(ctx context.Context, email, password string) (string, error) {
	resp, err := c.do(ctx, http.MethodPost, "/v1/auth/login", loginReq{Email: email, Password: password}, nil)
	if err != nil {
		return "", err
	}
	for _, ck := range resp.Cookies() {
		if ck.Name == "flex_session" {
			c.cookie = ck.Name + "=" + ck.Value
			return c.cookie, nil
		}
	}
	return "", fmt.Errorf("no flex_session cookie in response")
}

func (c *APIClient) Logout(ctx context.Context) error {
	_, err := c.do(ctx, http.MethodPost, "/v1/auth/logout", nil, nil)
	return err
}

type MeResp struct {
	ID    string `json:"id"`
	Slug  string `json:"slug"`
	Email string `json:"email"`
}

func (c *APIClient) Me(ctx context.Context) (MeResp, error) {
	var out MeResp
	_, err := c.do(ctx, http.MethodGet, "/v1/me", nil, &out)
	return out, err
}

type PairDeviceResp struct {
	DeviceID      string `json:"device_id"`
	Hostname      string `json:"hostname"`
	PreauthKey    string `json:"preauth_key"`
	HeadscaleURL  string `json:"headscale_url"`
	TailnetDomain string `json:"tailnet_domain"`
}

type pairDeviceReq struct {
	Name string `json:"name"`
}

func (c *APIClient) PairDevice(ctx context.Context, name string) (PairDeviceResp, error) {
	var out PairDeviceResp
	_, err := c.do(ctx, http.MethodPost, "/v1/devices/pair", pairDeviceReq{Name: name}, &out)
	return out, err
}

type Device struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Hostname string `json:"hostname"`
}

func (c *APIClient) ListDevices(ctx context.Context) ([]Device, error) {
	var out []Device
	_, err := c.do(ctx, http.MethodGet, "/v1/devices", nil, &out)
	return out, err
}

func (c *APIClient) DeleteDevice(ctx context.Context, id string) error {
	_, err := c.do(ctx, http.MethodDelete, "/v1/devices/"+id, nil, nil)
	return err
}

type SSHKey struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Fingerprint string `json:"fingerprint"`
	PublicKey   string `json:"public_key"`
}

type addKeyReq struct {
	Name      string `json:"name"`
	PublicKey string `json:"public_key"`
}

func (c *APIClient) AddSSHKey(ctx context.Context, name, publicKey string) (string, error) {
	var out struct {
		ID          string `json:"id"`
		Fingerprint string `json:"fingerprint"`
	}
	_, err := c.do(ctx, http.MethodPost, "/v1/me/ssh-keys", addKeyReq{Name: name, PublicKey: publicKey}, &out)
	return out.ID, err
}

func (c *APIClient) ListSSHKeys(ctx context.Context) ([]SSHKey, error) {
	var out []SSHKey
	_, err := c.do(ctx, http.MethodGet, "/v1/me/ssh-keys", nil, &out)
	return out, err
}

func (c *APIClient) DeleteSSHKey(ctx context.Context, id string) error {
	_, err := c.do(ctx, http.MethodDelete, "/v1/me/ssh-keys/"+id, nil, nil)
	return err
}

type Env struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Hostname string `json:"hostname"`
	Status   string `json:"status"`
	NodeID   string `json:"node_id"`
}

func (c *APIClient) ListEnvs(ctx context.Context) ([]Env, error) {
	var out []Env
	_, err := c.do(ctx, http.MethodGet, "/v1/envs", nil, &out)
	return out, err
}
