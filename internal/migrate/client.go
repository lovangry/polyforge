package migrate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// aihub is the minimal admin-face HTTP client the migration needs. It is
// deliberately self-contained (the tool's only live-server surface):
// project/user resolution and creation (import order step ①), the
// rebuild-index call (step ③) and the retrieval smoke (verification
// step ③ of §10.1). Authentication is the operator's admin api key —
// rebuild-index and the management face are admin-only.
type aihub struct {
	base   string
	apiKey string
	hc     *http.Client
}

func newAihub(base, apiKey string) *aihub {
	return &aihub{base: base, apiKey: apiKey, hc: &http.Client{}}
}

// httpError is a non-2xx aihub answer, keeping the typed envelope fields.
type httpError struct {
	Method string
	Path   string
	Status int
	Code   string
	Reason string
	Detail string
	Body   string
}

func (e *httpError) Error() string {
	msg := e.Code
	if msg == "" {
		msg = fmt.Sprintf("HTTP-%d", e.Status)
	}
	if e.Reason != "" {
		msg += " " + e.Reason
	}
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	if msg == e.Code && e.Body != "" {
		msg += ": " + e.Body
	}
	return fmt.Sprintf("aihub %s %s: %s", e.Method, e.Path, msg)
}

// notFound reports whether err is a server 404 answer (CORE-404/MEM-404
// surface as HTTP 404/404-class typed codes).
func notFound(err error) bool {
	he, ok := err.(*httpError)
	if !ok {
		return false
	}
	return he.Status == http.StatusNotFound ||
		he.Code == "CORE-404" || he.Code == "MEM-404"
}

// do issues one JSON request; a non-2xx answer becomes a typed *httpError
// carrying the server's envelope fields.
func (a *aihub) do(method, path string, query url.Values, body any, out any) error {
	u := a.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	var rd io.Reader
	if payload != nil {
		rd = bytes.NewReader(payload)
	}
	req, err := http.NewRequest(method, u, rd)
	if err != nil {
		return err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+a.apiKey)
	resp, err := a.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var env struct {
			Error struct {
				Code   string `json:"code"`
				Reason string `json:"reason"`
				Detail string `json:"detail"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &env)
		return &httpError{
			Method: method, Path: path, Status: resp.StatusCode,
			Code: env.Error.Code, Reason: env.Error.Reason, Detail: env.Error.Detail,
			Body: string(bytes.TrimSpace(raw)),
		}
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("aihub %s %s: decode response: %w", method, path, err)
		}
	}
	return nil
}

// projectView mirrors GET /api/v1/projects items.
type projectView struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Visible bool   `json:"visible"`
}

// userView mirrors GET /api/v1/users items.
type userView struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Kind        string `json:"kind"`
	Role        string `json:"role"`
}

func (a *aihub) listProjects() ([]projectView, error) {
	var out struct {
		Items []projectView `json:"items"`
	}
	if err := a.do("GET", "/api/v1/projects", nil, nil, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

func (a *aihub) listUsers() ([]userView, error) {
	var out struct {
		Items []userView `json:"items"`
	}
	if err := a.do("GET", "/api/v1/users", nil, nil, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// createProject creates a placeholder project (import step ①, B03:
// visible=false so an unmapped old project never surfaces in the
// directory listing before a human merges it).
func (a *aihub) createProject(name string, visible bool) (projectView, error) {
	var out projectView
	body := map[string]any{"name": name, "visible": visible}
	if err := a.do("POST", "/api/v1/projects", nil, body, &out); err != nil {
		return out, err
	}
	return out, nil
}

// createUser creates a placeholder machine user (B03: display_name =
// "<old> (imported)", kind machine; humans resolve through --user-map,
// placeholders are always machine rows so they never impersonate a real
// person in permission decisions).
func (a *aihub) createUser(displayName, kind, role string) (userView, error) {
	var out userView
	body := map[string]any{"display_name": displayName, "kind": kind, "role": role}
	if err := a.do("POST", "/api/v1/users", nil, body, &out); err != nil {
		return out, err
	}
	return out, nil
}

// rebuildIndex is import step ③: the live server's §5.3 full reconcile
// backfills every placed authority file into Core + FTS. Returns the
// number of reindexed active entries.
func (a *aihub) rebuildIndex() (int, error) {
	var out struct {
		Rebuilt int `json:"rebuilt"`
	}
	if err := a.do("POST", "/api/v1/memories/rebuild-index", nil, nil, &out); err != nil {
		return 0, err
	}
	return out.Rebuilt, nil
}

// searchMemories is the verification-step-③ retrieval smoke over the
// real HTTP search face (row 23).
func (a *aihub) searchMemories(projectName, q string) ([]string, error) {
	query := url.Values{}
	query.Set("project", projectName)
	query.Set("q", q)
	query.Set("limit", "200")
	var out struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := a.do("GET", "/api/v1/memories/search", query, nil, &out); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(out.Items))
	for _, it := range out.Items {
		ids = append(ids, it.ID)
	}
	return ids, nil
}
