package daemon

// memface.go: the F6 memory adapters — the daemon-side halves of both
// memory lanes. Every aihub call is made HERE, with the daemon's own
// machine credential: the spawned agents hold no aihub credential, no
// aihub URL and no direct server path (the hard constraint of the F6
// ruling), and both the claim-time snapshot (F6-A) and the UDS recall /
// remember proxies (F6-B) ride these adapters.
//
// The wire shapes are the aihub faces' own: the memory search endpoint
// (with the F6 excerpt extension — a bounded content prefix per hit)
// and the memory create endpoint. Both live behind the generic
// aihubclient.Do so the shared client stays untouched.

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/polyforge/observe"
	"github.com/silgrid/polyforge/internal/polyforge/runtime"
)

// Excerpt bounds (runes), per lane: the static snapshot carries short
// prefixes (it is one of many capsule inputs); the live recall channel
// carries longer ones (it is the agent's actual fetch path). Both stay
// under the server's excerpt ceiling.
const (
	snapshotExcerptRunes = 1200
	recallExcerptRunes   = 6000
)

// memSearchHit decodes one search answer row with the excerpt extension.
type memSearchHit struct {
	ID             string   `json:"id"`
	Type           string   `json:"type"`
	Title          *string  `json:"title"`
	ContentExcerpt string   `json:"content_excerpt"`
	Tags           []string `json:"tags"`
	CreatedAt      string   `json:"created_at"`
}

func (h memSearchHit) title() string {
	if h.Title != nil {
		return *h.Title
	}
	return ""
}

// recallSnapshot binds the F6-A claim-time seam: goal+content in, the
// top-K hits (identity + bounded excerpt) out. The runtime's
// materialize writes the answer as the static snapshot; failures travel
// back as errors and degrade to a noticed empty list there.
func recallSnapshot(hub Hub) func(query string, topK int) ([]runtime.RecalledMemory, error) {
	return func(query string, topK int) ([]runtime.RecalledMemory, error) {
		hits, err := memSearch(hub, query, topK, snapshotExcerptRunes)
		if err != nil {
			return nil, err
		}
		out := make([]runtime.RecalledMemory, 0, len(hits))
		for _, h := range hits {
			out = append(out, runtime.RecalledMemory{
				ID:      h.ID,
				Type:    h.Type,
				Title:   h.title(),
				Excerpt: h.ContentExcerpt,
			})
		}
		return out, nil
	}
}

// memSearch runs one bounded memory search against aihub and decodes the
// excerpted hits.
func memSearch(hub Hub, query string, topK, excerptRunes int) ([]memSearchHit, error) {
	v := url.Values{}
	v.Set("q", query)
	if topK > 0 {
		v.Set("limit", strconv.Itoa(topK))
	}
	v.Set("excerpt", strconv.Itoa(excerptRunes))
	var resp struct {
		Items []memSearchHit `json:"items"`
	}
	if err := hub.Do("GET", "/api/v1/memories/search?"+v.Encode(), nil, &resp); err != nil {
		return nil, err
	}
	return resp.Items, nil
}

// memFace implements observe.MemoryFace over the shared aihub client:
// the UDS recall/remember proxies' server half.
type memFace struct {
	hub Hub
}

// Recall answers one UDS recall: the semantic search in the machine
// key's readable scope (the machine key is project-scoped in
// production), with bounded excerpts.
func (f memFace) Recall(wi, query string, topK int) ([]observe.MemoryHit, error) {
	hits, err := memSearch(f.hub, query, topK, recallExcerptRunes)
	if err != nil {
		return nil, err
	}
	out := make([]observe.MemoryHit, 0, len(hits))
	for _, h := range hits {
		out = append(out, observe.MemoryHit{
			ID:        h.ID,
			Type:      h.Type,
			Title:     h.title(),
			Excerpt:   h.ContentExcerpt,
			Tags:      h.Tags,
			CreatedAt: h.CreatedAt,
		})
	}
	return out, nil
}

// Remember stores one UDS remember: the project is resolved from the
// work item (the agent never names one — it could never name another's)
// and the memory is attributed to the work item.
func (f memFace) Remember(wi string, req observe.MemoryRemember) (observe.MemoryStored, error) {
	item, err := f.hub.GetWorkItem(wi)
	if err != nil {
		return observe.MemoryStored{}, fmt.Errorf("resolve work item for memory attribution: %w", err)
	}
	if item.ProjectName == "" {
		return observe.MemoryStored{}, &api.Error{
			Code:   api.ErrDaemonInternal,
			Detail: "work item carries no project; memory attribution refused",
		}
	}
	body := map[string]any{
		"project":      item.ProjectName,
		"type":         req.Type,
		"content":      req.Content,
		"visibility":   req.Visibility,
		"work_item_id": wi,
	}
	if req.Title != "" {
		body["title"] = req.Title
	}
	if len(req.Tags) > 0 {
		body["tags"] = req.Tags
	}
	var raw json.RawMessage
	if err := f.hub.Do("POST", "/api/v1/memories", body, &raw); err != nil {
		return observe.MemoryStored{}, err
	}
	var stored struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &stored); err != nil || stored.ID == "" {
		return observe.MemoryStored{}, &api.Error{
			Code:   api.ErrDaemonInternal,
			Detail: "memory create answer carried no id",
		}
	}
	return observe.MemoryStored{ID: stored.ID, Status: "created"}, nil
}
