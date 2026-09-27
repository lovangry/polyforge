package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// APIError is the typed error from either server face, rendered from the
// wire envelope {error:{code,reason,detail}} (api/openapi.yaml
// ErrorEnvelope; the daemon face reuses the same envelope shape with
// DAEMON-* codes per 04-local-runtime.md §5.2 — client-side inference,
// the design pins the codes but not the envelope shape).
type APIError struct {
	// Code is the prominent typed code: CORE-*/SYNC-*/MEM-* or DAEMON-*,
	// or the synthesized "HTTP-<status>" when the body carried no envelope.
	Code string
	// Reason is the machine-readable sub-reason ("" when none).
	Reason string
	// Detail is the human-readable detail.
	Detail string
	// Status is the HTTP status the response arrived with. NOTE: the wire
	// contract deliberately derives status from code and the two diverge
	// for SYNC-403/MEM-403 (they surface as HTTP 500, api/openapi.yaml
	// x-error-status-map) — the code is authoritative, never the status.
	Status int
	// Raw is the original response body, kept so --json mode can emit the
	// server's error JSON as-is.
	Raw []byte
}

// Error renders the code prominently: "CORE-409 ALREADY_CLAIMED: detail".
func (e *APIError) Error() string {
	msg := e.Code
	if e.Reason != "" {
		msg += " " + e.Reason
	}
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}

// errorEnvelope mirrors the wire error contract (api/openapi.yaml).
type errorEnvelope struct {
	Error struct {
		Code   string `json:"code"`
		Reason string `json:"reason"`
		Detail string `json:"detail"`
	} `json:"error"`
}

// parseAPIError decodes a non-2xx response body into an *APIError. A body
// that does not carry the envelope (proxy in the way, plain-text crash)
// degrades to HTTP-<status> rather than masking the real code.
func parseAPIError(status int, body []byte) *APIError {
	var env errorEnvelope
	if err := json.Unmarshal(body, &env); err == nil && env.Error.Code != "" {
		return &APIError{
			Code:   env.Error.Code,
			Reason: env.Error.Reason,
			Detail: env.Error.Detail,
			Status: status,
			Raw:    body,
		}
	}
	return &APIError{
		Code:   fmt.Sprintf("HTTP-%d", status),
		Detail: strings.TrimSpace(string(body)),
		Status: status,
		Raw:    body,
	}
}

// execute performs one HTTP request and returns the raw response body.
// Any non-2xx answer is returned as an *APIError. body may be nil (no
// request body) or any JSON-marshalable value.
func execute(httpc *http.Client, method, url string, body any, header map[string]string) ([]byte, error) {
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request body: %w", err)
		}
	}
	var req *http.Request
	var err error
	if payload != nil {
		req, err = http.NewRequest(method, url, bytes.NewReader(payload))
	} else {
		req, err = http.NewRequest(method, url, nil)
	}
	if err != nil {
		return nil, err
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, parseAPIError(resp.StatusCode, raw)
	}
	return raw, nil
}
