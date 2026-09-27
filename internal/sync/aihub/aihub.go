// Package aihub is the Daemon's HTTP client for the AIHub Core: the only
// channel through which a Daemon touches Core (no direct DB access, ever).
// Covers the publish/recovery face (05 §5.1), the execution right (claim /
// force-takeover / end-generation, 02 §5.1), the scheduler face (ready
// queue, 02 §5.1b) and the machine heartbeat (02 §4.10).
//
// Manifest content over HTTP: closed by the row-19 amendment (polyforge#14)
// - GET /revisions?since=&include=ops serves each revision's manifest
// audit content (kind + ops) inline, so 05 §7.4 remote-site reconstruction
// replays entirely over HTTP. ListRevisions always requests include=ops
// (the Daemon's only listing consumer is recovery); GetManifest fetches
// one revision's content through the same endpoint as the fallback path.
package aihub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/silgrid/polyforge/internal/sync/revision"
)

// APIError is the typed Core error envelope ({"error":{code,reason,detail}}).
type APIError struct {
	Code   string
	Reason string
	Detail string
	Status int
}

func (e *APIError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("%s %s: %s", e.Code, e.Reason, e.Detail)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Detail)
}

// IsCode reports whether err is an APIError with the given code.
func IsCode(err error, code string) bool {
	ae, ok := err.(*APIError)
	return ok && ae.Code == code
}

// ClaimResponse mirrors owner.ClaimResponse (the hardened, idempotency-
// cached claim result; 02 §6.1).
type ClaimResponse struct {
	Generation       int64           `json:"generation"`
	GenerationToken  string          `json:"generation_token"`
	SecretEpoch      int             `json:"secret_epoch"`
	Owner            string          `json:"owner"`
	CapsuleBootstrap json.RawMessage `json:"capsule_bootstrap,omitempty"`
}

// ReadyQueue mirrors the flat two-array scheduler response (02 §5.1b).
type ReadyQueue struct {
	Project    string            `json:"project"`
	Executable []ReadyQueueEntry `json:"executable"`
	Blocked    []ReadyQueueEntry `json:"blocked"`
}

// ReadyQueueEntry is one scheduler row.
type ReadyQueueEntry struct {
	ID         string   `json:"id"`
	Slug       string   `json:"slug"`
	Goal       string   `json:"goal"`
	Priority   string   `json:"priority"`
	Status     string   `json:"status"`
	BundleName *string  `json:"bundle_name"`
	WaitingOn  *string  `json:"waiting_on,omitempty"`
	BlockedBy  []string `json:"blocked_by,omitempty"`
	CreatedAt  string   `json:"created_at"`
	Labels     []string `json:"labels"`
}

// FlushEvidence mirrors owner.FlushEvidence (02 §6.4): the Daemon-side
// generated end-generation evidence.
type FlushEvidence struct {
	PublishedGeneration int64             `json:"published_generation"`
	PublishedSeq        int64             `json:"published_seq"`
	ManifestSha         string            `json:"manifest_sha"`
	FrozenCheckpoint    *FrozenCheckpoint `json:"frozen_checkpoint"`
}

// FrozenCheckpoint binds the evidence to the post-freeze local checkpoint.
type FrozenCheckpoint struct {
	ScannedAt     string `json:"scanned_at"`
	BytesDigest   string `json:"bytes_digest"`
	WorktreeState string `json:"worktree_state"`
	ManifestSha   string `json:"manifest_sha"`
	MatchesHead   bool   `json:"matches_head"`
}

// Client talks to one AIHub base URL with one machine api key.
type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

// New creates a client.
func New(baseURL, apiKey string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), APIKey: apiKey, HTTP: http.DefaultClient}
}

func (c *Client) do(method, path string, body any, token string, out any) error {
	var rd io.Reader
	if body != nil {
		blob, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(blob)
	}
	req, err := http.NewRequest(method, c.BaseURL+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	if token != "" {
		req.Header.Set("X-PF-Generation-Token", token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		var env struct {
			Error struct {
				Code   string `json:"code"`
				Reason string `json:"reason"`
				Detail string `json:"detail"`
			} `json:"error"`
		}
		if jerr := json.Unmarshal(raw, &env); jerr == nil && env.Error.Code != "" {
			return &APIError{Code: env.Error.Code, Reason: env.Error.Reason, Detail: env.Error.Detail, Status: resp.StatusCode}
		}
		return &APIError{Code: fmt.Sprintf("HTTP-%d", resp.StatusCode), Detail: string(raw), Status: resp.StatusCode}
	}
	if out != nil {
		if len(raw) == 0 {
			return nil
		}
		if uerr := json.Unmarshal(raw, out); uerr != nil {
			return fmt.Errorf("aihub: decode %s: %w", path, uerr)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Execution right (02 §5.1 rows 7–13)

// Claim executes claim/resume/unblock (all one claim, 02 §2.3).
func (c *Client) Claim(wiID, idempotencyKey, machineID string) (*ClaimResponse, error) {
	var resp ClaimResponse
	if err := c.do("POST", "/api/v1/work-items/"+url.PathEscape(wiID)+"/claim",
		map[string]string{"idempotency_key": idempotencyKey, "machine_id": machineID}, "", &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ForceTakeover is the admin recovery entry (02 §6.2): ends the old
// generation and returns the NEW generation's credentials in one response —
// the caller consumes them directly, no second claim.
func (c *Client) ForceTakeover(wiID, machineID string) (*ClaimResponse, error) {
	var resp ClaimResponse
	if err := c.do("POST", "/api/v1/work-items/"+url.PathEscape(wiID)+"/force-takeover",
		map[string]string{"machine_id": machineID}, "", &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// EndGeneration closes the active generation with one of the four reasons.
// Flush evidence is REQUIRED (02 §6.4); a stale token fails CORE-403.
func (c *Client) EndGeneration(wiID, reason, waitingOn, token string, fe *FlushEvidence) error {
	body := map[string]any{"reason": reason, "flush_evidence": fe}
	if waitingOn != "" {
		body["waiting_on"] = waitingOn
	}
	return c.do("POST", "/api/v1/work-items/"+url.PathEscape(wiID)+"/end-generation", body, token, nil)
}

// ---------------------------------------------------------------------------
// Scheduler + machine face

// ReadyQueue fetches {executable, blocked} for a project (02 §5.1b).
func (c *Client) ReadyQueue(project string) (*ReadyQueue, error) {
	var rq ReadyQueue
	if err := c.do("GET", "/api/v1/ready-queue?project="+url.QueryEscape(project), nil, "", &rq); err != nil {
		return nil, err
	}
	return &rq, nil
}

// Heartbeat upserts the machine row (02 §4.10): first call registers (the
// plaintext key returned exactly once), later calls refresh.
func (c *Client) Heartbeat(machineID, runtimeVersion, mode string) (*HeartbeatResult, error) {
	body := map[string]string{"machine_id": machineID, "runtime_version": runtimeVersion}
	if mode != "" {
		body["mode"] = mode
	}
	var out struct {
		HeartbeatResult
		APIKey string `json:"api_key"`
	}
	if err := c.do("POST", "/api/v1/machines/heartbeat", body, "", &out); err != nil {
		return nil, err
	}
	res := out.HeartbeatResult
	res.APIKey = out.APIKey
	return &res, nil
}

// HeartbeatResult is the heartbeat response.
type HeartbeatResult struct {
	MachineID  string `json:"machine_id"`
	Registered bool   `json:"registered"`
	APIKey     string `json:"api_key,omitempty"`
}

// ---------------------------------------------------------------------------
// Publish / recovery face (05 §5.1) — rows 14–17, 19.

// PutBlock uploads one zstd-compressed CAS block; shaHex addresses the
// ORIGINAL bytes; the generation token is fenced server-side (05 §2.9:
// fencing at the BLOCK stage, so a fenced-out owner cannot pile up orphans).
func (c *Client) PutBlock(wiID string, seq int64, shaHex string, block []byte, token string) error {
	req, err := http.NewRequest("PUT",
		fmt.Sprintf("%s/api/v1/work-items/%s/revisions/%d/blocks/%s", c.BaseURL, url.PathEscape(wiID), seq, shaHex),
		bytes.NewReader(block))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("X-PF-Generation-Token", token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return decodeErr(raw, resp.StatusCode)
	}
	return nil
}

// PostManifest submits {manifest, manifest_sha} (05 §5.1 row 15). The
// server's accept chain is: fencing → idempotency → HEAD bootstrap/parent →
// manifest_sha recompute → SYNC-INV-3 → block integrity.
func (c *Client) PostManifest(wiID string, m revision.Manifest, manifestSha, token string) (headSeq int64, err error) {
	var out struct {
		HeadSeq int64 `json:"head_seq"`
	}
	body := map[string]any{"manifest": m, "manifest_sha": manifestSha}
	if err := c.do("POST",
		fmt.Sprintf("/api/v1/work-items/%s/revisions/%d/manifest", url.PathEscape(wiID), m.Seq),
		body, token, &out); err != nil {
		return 0, err
	}
	return out.HeadSeq, nil
}

// GetHead reads the HEAD pointer; found=false for the 404 no-row case
// (pre-bootstrap).
func (c *Client) GetHead(wiID string) (*revision.HeadInfo, bool, error) {
	var h revision.HeadInfo
	err := c.do("GET", "/api/v1/work-items/"+url.PathEscape(wiID)+"/head", nil, "", &h)
	if err != nil {
		if IsCode(err, "404") {
			return nil, false, nil
		}
		return nil, false, err
	}
	return &h, true, nil
}

// GetBlock fetches one CAS block (recovery face, row 17).
func (c *Client) GetBlock(wiID, shaHex string) ([]byte, error) {
	req, err := http.NewRequest("GET",
		fmt.Sprintf("%s/api/v1/work-items/%s/revisions/0/blocks/%s", c.BaseURL, url.PathEscape(wiID), shaHex), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, decodeErr(raw, resp.StatusCode)
	}
	return raw, nil
}

// ListRevisions fetches the revision listing since seq (row 19). The
// Daemon's only consumer is remote-site recovery (05 §7.4), which replays
// each revision's manifest ops, so the listing is ALWAYS requested with
// include=ops (the row-19 amendment, polyforge#14) and the returned rows
// carry kind+ops inline. The endpoint's default (parameter absent)
// remains metadata-only per the wire contract.
func (c *Client) ListRevisions(wiID string, since int64) ([]revision.RevisionMeta, error) {
	var out struct {
		Revisions []revision.RevisionMeta `json:"revisions"`
	}
	if err := c.do("GET",
		fmt.Sprintf("/api/v1/work-items/%s/revisions?since=%d&include=ops", url.PathEscape(wiID), since),
		nil, "", &out); err != nil {
		return nil, err
	}
	return out.Revisions, nil
}

// GetManifest fetches one revision's manifest content over the real HTTP
// face: row 19 with include=ops (the amendment, polyforge#14) serves the
// manifest audit content inline, so this lists since=seq and picks the
// matching row. It is the FALLBACK path of recover.Recover (used when a
// remote's listing does not carry ops inline). Fields the row-19 listing
// does not carry (live_files, execution_state_included, built_at) are
// zero-valued; recovery only consumes kind and ops.
func (c *Client) GetManifest(wiID string, seq int64) (revision.Manifest, error) {
	rows, err := c.ListRevisions(wiID, seq)
	if err != nil {
		return revision.Manifest{}, err
	}
	for _, r := range rows {
		if r.Seq == seq {
			return revision.Manifest{
				WorkItemID: wiID, Seq: r.Seq, Parent: r.Parent, Generation: r.Generation,
				Kind: r.Kind, Ops: r.Ops, DataBytes: r.DataBytes,
			}, nil
		}
	}
	return revision.Manifest{}, &APIError{
		Code: "SYNC-404", Detail: fmt.Sprintf("revision %d not listed", seq), Status: 404,
	}
}

// Progress read-through: renders the latest published HEAD's execution.json
// (02 §5.2). Used by the daemon's second observe layer for WIs without a
// local capsule.
func (c *Client) Progress(wiID string) (headSeq *int64, progress map[string]any, err error) {
	var out struct {
		HeadSeq  *int64          `json:"head_seq"`
		Progress json.RawMessage `json:"progress"`
	}
	if err := c.do("GET", "/api/v1/work-items/"+url.PathEscape(wiID)+"/progress", nil, "", &out); err != nil {
		return nil, nil, err
	}
	if len(out.Progress) > 0 {
		if uerr := json.Unmarshal(out.Progress, &progress); uerr != nil {
			return out.HeadSeq, nil, uerr
		}
	}
	return out.HeadSeq, progress, nil
}

func decodeErr(raw []byte, status int) error {
	var env struct {
		Error struct {
			Code   string `json:"code"`
			Reason string `json:"reason"`
			Detail string `json:"detail"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err == nil && env.Error.Code != "" {
		return &APIError{Code: env.Error.Code, Reason: env.Error.Reason, Detail: env.Error.Detail, Status: status}
	}
	return &APIError{Code: fmt.Sprintf("HTTP-%d", status), Detail: string(raw), Status: status}
}

// IsAPIError marks every ANSWERED request's error as definite (the publish
// flow treats any explicit server verdict — success or 4xx/5xx — as a
// settled outcome per 05 §2.8; only transport loss is ambiguous).
func (e *APIError) IsAPIError() bool { return true }

// ---------------------------------------------------------------------------
// Read faces used by the WI-Agent local proxy (04 §5.3 whitelist).

// GetWI returns a work item's detail JSON (read_workflow_ref: bundle
// reference columns; Core stores no resolved steps — INV-8).
func (c *Client) GetWI(wiID string) (map[string]any, error) {
	var out map[string]any
	if err := c.do("GET", "/api/v1/work-items/"+url.PathEscape(wiID), nil, "", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetBudget returns the WI budget baseline and effective caps.
func (c *Client) GetBudget(wiID string) (map[string]any, error) {
	var out map[string]any
	if err := c.do("GET", "/api/v1/work-items/"+url.PathEscape(wiID)+"/budget", nil, "", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// MemorySearch forwards a memory_read proxy operation (06 §5.1).
func (c *Client) MemorySearch(body map[string]any) (map[string]any, error) {
	var out map[string]any
	if err := c.do("POST", "/api/v1/memories/search", body, "", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// MemoryCreate forwards a memory_write proxy operation (06 §5.1).
func (c *Client) MemoryCreate(body map[string]any) (map[string]any, error) {
	var out map[string]any
	if err := c.do("POST", "/api/v1/memories", body, "", &out); err != nil {
		return nil, err
	}
	return out, nil
}
