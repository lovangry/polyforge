// Package aihubclient is the shared HTTP client for the aihub server, used
// by runtime (claim/end-generation), observe (read-through) and the CLI
// verbs. Wire types come from internal/common/api; error decoding maps the
// typed errStatus envelope back into *api.Error.
package aihubclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
)

// Client talks to one aihub server.
type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

func New(baseURL, apiKey string) *Client {
	return &Client{
		BaseURL: baseURL,
		APIKey:  apiKey,
		HTTP:    &http.Client{Timeout: 60 * time.Second},
	}
}

// Do performs a typed request. body may be nil. out receives the decoded
// JSON response when non-nil. Non-2xx answers decode as *api.Error.
func (c *Client) Do(method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.BaseURL+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var ae api.Error
		if json.Unmarshal(data, &ae) == nil && ae.Code != "" {
			return &ae
		}
		return &api.Error{Code: "AUTH-401", Detail: fmt.Sprintf("HTTP %d: %.200s", resp.StatusCode, data)}
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

// ClaimRequest asks aihub to open a generation on a work item.
type ClaimRequest struct {
	WorkItemID string `json:"work_item_id"`
	Intent     string `json:"intent"` // claim|resume|takeover
	MachineID  string `json:"machine_id"`
	Owner      string `json:"owner"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// ClaimResponse carries the attempt identity and the attempt-scoped git push
// credential (generation fencing, main layer).
type ClaimResponse struct {
	Claim       api.Claim `json:"claim"`
	Credential  api.PushCredential `json:"credential"`
	WorkItem    api.WorkItem `json:"work_item"`
}

func (c *Client) Claim(req ClaimRequest) (*ClaimResponse, error) {
	var out ClaimResponse
	if err := c.Do("POST", "/api/v1/work-items/"+url.PathEscape(req.WorkItemID)+"/claim", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// EndGeneration closes the current generation with a terminal verdict.
type EndGenerationRequest struct {
	AttemptID string `json:"attempt_id"`
	Generation int64 `json:"generation"`
	Status    string `json:"status"` // wrapped|failed|blocked
	Note      string `json:"note,omitempty"`
}

func (c *Client) EndGeneration(wi string, req EndGenerationRequest) error {
	return c.Do("POST", "/api/v1/work-items/"+url.PathEscape(wi)+"/end-generation", req, nil)
}

func (c *Client) GetWorkItem(idOrSlug string) (*api.WorkItem, error) {
	var wi api.WorkItem
	if err := c.Do("GET", "/api/v1/work-items/"+url.PathEscape(idOrSlug), nil, &wi); err != nil {
		return nil, err
	}
	return &wi, nil
}

// ListWorkItems filters by project and status (queue views).
func (c *Client) ListWorkItems(project string, status string) ([]api.WorkItem, error) {
	q := url.Values{}
	if project != "" {
		q.Set("project", project)
	}
	if status != "" {
		q.Set("status", status)
	}
	var resp struct {
		Items []api.WorkItem `json:"items"`
		Total int            `json:"total"`
	}
	if err := c.Do("GET", "/api/v1/work-items?"+q.Encode(), nil, &resp); err != nil {
		return nil, err
	}
	return resp.Items, nil
}

// Queue returns ready work items for autonomous claiming (drain mode).
func (c *Client) Queue(project string) ([]api.WorkItem, error) {
	var resp struct {
		Items []api.WorkItem `json:"items"`
	}
	if err := c.Do("GET", "/api/v1/projects/"+url.PathEscape(project)+"/ready-queue", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Items, nil
}
