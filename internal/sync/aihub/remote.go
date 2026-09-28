package aihub

// remote.go: the ONE concrete bridge from the aihub HTTP client to the
// context-shaped transport faces the publish/recover layers consume
// (internal/sync/publish.Remote — rows 14–16 — and
// internal/sync/recover.Remote — rows 16/17/19). The daemon, the dogfood
// harness and the integration drills previously each carried their own
// private adapter with byte-identical forwarding bodies; this is the
// single shared implementation they all construct now.
//
// Every method is a PLAIN FORWARD to the client: the ctx parameters are
// the transport faces' shape only and are deliberately NOT honored —
// starting to honor context cancellation here is a behavior change that
// is out of scope. The publish.Remote/recover.Remote test seams (fakes)
// and the stripOps drill overrides stay as they are.

import (
	"context"

	"github.com/silgrid/polyforge/internal/sync/revision"
)

// RemoteAdapter implements publish.Remote and recover.Remote over the
// concrete *Client. Construct as &aihub.RemoteAdapter{Client: c}. The
// conformance is pinned by the real call sites (publish.New /
// recover.Recover take the interface directly); this package must not
// import its consumers.
type RemoteAdapter struct{ Client *Client }

// PutBlock forwards to the client (publish.Remote).
func (r *RemoteAdapter) PutBlock(ctx context.Context, wi string, seq int64, sha string, block []byte, token string) error {
	return r.Client.PutBlock(wi, seq, sha, block, token)
}

// PostManifest forwards to the client (publish.Remote).
func (r *RemoteAdapter) PostManifest(ctx context.Context, wi string, m revision.Manifest, sha, token string) (int64, error) {
	return r.Client.PostManifest(wi, m, sha, token)
}

// GetHead forwards to the client (publish.Remote + recover.Remote).
func (r *RemoteAdapter) GetHead(ctx context.Context, wi string) (*revision.HeadInfo, bool, error) {
	return r.Client.GetHead(wi)
}

// ListRevisions forwards to the client (recover.Remote; the row-19
// listing is requested with include=ops by the client).
func (r *RemoteAdapter) ListRevisions(ctx context.Context, wi string, since int64) ([]revision.RevisionMeta, error) {
	return r.Client.ListRevisions(wi, since)
}

// GetManifest forwards to the client (recover.Remote).
func (r *RemoteAdapter) GetManifest(ctx context.Context, wi string, seq int64) (revision.Manifest, error) {
	return r.Client.GetManifest(wi, seq)
}

// GetBlock forwards to the client (recover.Remote).
func (r *RemoteAdapter) GetBlock(ctx context.Context, wi, sha string) ([]byte, error) {
	return r.Client.GetBlock(wi, sha)
}
