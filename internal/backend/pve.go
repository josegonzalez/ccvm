package backend

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// pveClient talks to the Proxmox VE REST API.
//
// Proxmox is the one backend that is an API rather than a CLI, so it gets a
// real client with typed responses instead of curl piped through jq. That is
// also what makes it testable without a cluster: every method here is exercised
// against httptest.
type pveClient struct {
	BaseURL string
	// TokenID is user@realm!tokenid.
	TokenID string
	Secret  string
	HTTP    *http.Client
}

func newPVEClient(cfg Config) (*pveClient, error) {
	if strings.TrimSpace(cfg.ProxmoxURL) == "" {
		return nil, fmt.Errorf("%w: set CCVM_PROXMOX_URL", ErrNotConfigured)
	}
	if strings.TrimSpace(cfg.ProxmoxTokenID) == "" || strings.TrimSpace(cfg.ProxmoxSecret) == "" {
		return nil, fmt.Errorf("%w: set CCVM_PROXMOX_TOKEN_ID and CCVM_PROXMOX_SECRET", ErrNotConfigured)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.ProxmoxInsecure {
		// Homelab clusters routinely use a private CA. This is opt-in, never
		// the default.
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return &pveClient{
		BaseURL: strings.TrimRight(cfg.ProxmoxURL, "/"),
		TokenID: cfg.ProxmoxTokenID,
		Secret:  cfg.ProxmoxSecret,
		HTTP:    &http.Client{Timeout: 60 * time.Second, Transport: transport},
	}, nil
}

// pveError is a non-2xx response, carrying enough to classify it.
type pveError struct {
	Status int
	Path   string
	Body   string
}

func (e *pveError) Error() string {
	body := strings.TrimSpace(e.Body)
	if body == "" {
		return fmt.Sprintf("%s: HTTP %d", e.Path, e.Status)
	}
	return fmt.Sprintf("%s: HTTP %d: %s", e.Path, e.Status, body)
}

// Fatal reports whether retrying could ever help. Auth and permission failures
// cannot be retried into success, and pretending otherwise just delays the
// real message.
func (e *pveError) Fatal() bool {
	return e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden ||
		e.Status == http.StatusNotFound
}

func (c *pveClient) do(ctx context.Context, method, path string, form url.Values, out any) error {
	full := c.BaseURL + path

	var body io.Reader
	if form != nil && (method == http.MethodPost || method == http.MethodPut) {
		body = strings.NewReader(form.Encode())
	} else if form != nil {
		full += "?" + form.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, method, full, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", fmt.Sprintf("PVEAPIToken=%s=%s", c.TokenID, c.Secret))
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("%s %s: read response: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &pveError{Status: resp.StatusCode, Path: path, Body: string(raw)}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s %s: parse response: %w", method, path, err)
	}
	return nil
}

// nextID asks the cluster for a free vmid.
//
// It is advisory, not a reservation: a concurrent create can take the id
// between this call and the clone, which is why that specific collision is
// retryable.
func (c *pveClient) nextID(ctx context.Context) (int, error) {
	var resp struct {
		Data json.Number `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/api2/json/cluster/nextid", nil, &resp); err != nil {
		return 0, err
	}
	id, err := resp.Data.Int64()
	if err != nil {
		return 0, fmt.Errorf("cluster/nextid returned %q: %w", resp.Data, err)
	}
	return int(id), nil
}

type pveNode struct {
	Node   string  `json:"node"`
	Status string  `json:"status"`
	MaxMem int64   `json:"maxmem"`
	Mem    int64   `json:"mem"`
	CPU    float64 `json:"cpu"`
}

func (c *pveClient) nodes(ctx context.Context) ([]pveNode, error) {
	var resp struct {
		Data []pveNode `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/api2/json/nodes", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Data, nil
}

// pickNode chooses the online node with the most free memory. Memory rather
// than CPU because a session's floor is memory: a node that cannot fit the
// guest cannot run it at all, whereas a busy one merely runs it slowly.
func (c *pveClient) pickNode(ctx context.Context) (string, error) {
	nodes, err := c.nodes(ctx)
	if err != nil {
		return "", err
	}
	best, bestFree := "", int64(-1)
	for _, n := range nodes {
		if !strings.EqualFold(n.Status, "online") {
			continue
		}
		if free := n.MaxMem - n.Mem; free > bestFree {
			best, bestFree = n.Node, free
		}
	}
	if best == "" {
		return "", fmt.Errorf("no online nodes; is the cluster quorate?")
	}
	return best, nil
}

type pveResource struct {
	VMID     int    `json:"vmid"`
	Node     string `json:"node"`
	Name     string `json:"name"`
	Status   string `json:"status"`
	Type     string `json:"type"`
	Tags     string `json:"tags"`
	Uptime   int64  `json:"uptime"`
	Template int    `json:"template"`
}

// resources enumerates every guest on every node in one call, rather than
// fanning out per node.
func (c *pveClient) resources(ctx context.Context) ([]pveResource, error) {
	var resp struct {
		Data []pveResource `json:"data"`
	}
	form := url.Values{"type": []string{"vm"}}
	if err := c.do(ctx, http.MethodGet, "/api2/json/cluster/resources", form, &resp); err != nil {
		return nil, err
	}
	return resp.Data, nil
}

// upid is the task handle Proxmox returns for anything asynchronous.
type upid string

func (c *pveClient) clone(ctx context.Context, node, kind string, tplID, newID int, form url.Values) (upid, error) {
	var resp struct {
		Data string `json:"data"`
	}
	path := fmt.Sprintf("/api2/json/nodes/%s/%s/%d/clone", node, kind, tplID)
	if err := c.do(ctx, http.MethodPost, path, form, &resp); err != nil {
		return "", err
	}
	return upid(resp.Data), nil
}

type pveTaskStatus struct {
	Status     string `json:"status"`
	ExitStatus string `json:"exitstatus"`
}

func (c *pveClient) taskStatus(ctx context.Context, node string, id upid) (pveTaskStatus, error) {
	var resp struct {
		Data pveTaskStatus `json:"data"`
	}
	path := fmt.Sprintf("/api2/json/nodes/%s/tasks/%s/status", node, url.PathEscape(string(id)))
	err := c.do(ctx, http.MethodGet, path, nil, &resp)
	return resp.Data, err
}

// taskLog is where the useful text lives. The status field says a task failed;
// only the log says why, and "clone failed" wastes the diagnosis.
func (c *pveClient) taskLog(ctx context.Context, node string, id upid) string {
	var resp struct {
		Data []struct {
			Text string `json:"t"`
		} `json:"data"`
	}
	path := fmt.Sprintf("/api2/json/nodes/%s/tasks/%s/log", node, url.PathEscape(string(id)))
	if err := c.do(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return ""
	}
	var lines []string
	for _, l := range resp.Data {
		if t := strings.TrimSpace(l.Text); t != "" {
			lines = append(lines, t)
		}
	}
	// The tail carries the failure; the head is setup noise.
	if len(lines) > 8 {
		lines = lines[len(lines)-8:]
	}
	return strings.Join(lines, "\n")
}

// waitTask blocks until a task finishes, and turns a failure into an error
// carrying the task log.
//
// A clone returns a UPID, not a finished guest. Starting before this returns
// races the clone, which is the most common way a hand-rolled version breaks.
func (c *pveClient) waitTask(ctx context.Context, node string, id upid, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		st, err := c.taskStatus(ctx, node, id)
		if err != nil {
			return err
		}
		if st.Status == "stopped" {
			if st.ExitStatus == "OK" {
				return nil
			}
			detail := c.taskLog(ctx, node, id)
			if detail == "" {
				detail = st.ExitStatus
			}
			return fmt.Errorf("task %s failed: %s", id, detail)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("task %s did not finish within %s", id, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func (c *pveClient) setConfig(ctx context.Context, node, kind string, vmid int, form url.Values) error {
	path := fmt.Sprintf("/api2/json/nodes/%s/%s/%d/config", node, kind, vmid)
	return c.do(ctx, http.MethodPost, path, form, nil)
}

func (c *pveClient) getConfig(ctx context.Context, node, kind string, vmid int) (map[string]any, error) {
	var resp struct {
		Data map[string]any `json:"data"`
	}
	path := fmt.Sprintf("/api2/json/nodes/%s/%s/%d/config", node, kind, vmid)
	if err := c.do(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Data, nil
}

func (c *pveClient) status(ctx context.Context, node, kind string, vmid int, action string) (upid, error) {
	var resp struct {
		Data string `json:"data"`
	}
	path := fmt.Sprintf("/api2/json/nodes/%s/%s/%d/status/%s", node, kind, vmid, action)
	if err := c.do(ctx, http.MethodPost, path, nil, &resp); err != nil {
		return "", err
	}
	return upid(resp.Data), nil
}

func (c *pveClient) destroy(ctx context.Context, node, kind string, vmid int) (upid, error) {
	var resp struct {
		Data string `json:"data"`
	}
	form := url.Values{"force": []string{"1"}, "purge": []string{"1"}}
	path := fmt.Sprintf("/api2/json/nodes/%s/%s/%d", node, kind, vmid)
	if err := c.do(ctx, http.MethodDelete, path+"?"+form.Encode(), nil, &resp); err != nil {
		return "", err
	}
	return upid(resp.Data), nil
}
