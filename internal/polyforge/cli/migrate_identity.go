package cli

// migrate_identity.go: old→new identity resolution for both migration
// verbs. All identities exist before any file is placed; re-runs re-find
// rows an earlier run created, never duplicating them.
//
// Target project: REQUIRED and must already exist (the seeding order in
// tools/seed-v1.sh creates it; the migration never invents projects).
//
// Old owner/reporter identifiers resolve in this order:
//  1. the --user-map entry (must name an existing user — a dangling
//     pointer is a config error, never a silent placeholder);
//  2. an existing user with the same email (human identifiers);
//  3. an existing user with the same display_name (the export emits
//     display_name for identities without an email);
//  4. an existing machine user with the same display_name;
//  5. a freshly created placeholder machine user "<old> (imported)" —
//     the deterministic name re-runs re-find.
//
// --owner (memories) / --reporter (work items) is a global override that
// short-circuits the whole chain: "import everything as this user".

import (
	"encoding/json"
	"strings"

	"github.com/silgrid/polyforge/internal/polyforge/aihubclient"
)

// migUserView is one target user as the resolver sees it.
type migUserView struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Email       string `json:"email"`
	UserType    string `json:"user_type"`
}

// migProjectView is one target project.
type migProjectView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// migResolver caches old→new identity resolution.
type migResolver struct {
	client    *aihubclient.Client
	userMap   map[string]string // old identifier → target user id
	users     []migUserView
	usersDone map[string]string // old identifier → target user id
	// ownerOverride: when set, EVERY old identity maps to this target user
	// (validated once against the target's user list).
	ownerOverride   string
	overrideChecked bool
}

func newMigResolver(client *aihubclient.Client, userMap map[string]string, ownerOverride string) *migResolver {
	return &migResolver{
		client: client, userMap: userMap,
		usersDone: map[string]string{}, ownerOverride: strings.TrimSpace(ownerOverride),
	}
}

// preload fetches the target user list once.
func (r *migResolver) preload() error {
	raw, err := r.client.ListUsers()
	if err != nil {
		return resolveErrf("list users: %v", err)
	}
	var l struct {
		Items []migUserView `json:"items"`
	}
	if err := unmarshalStrict(raw, &l); err != nil {
		return resolveErrf("list users: %v", err)
	}
	r.users = l.Items
	return nil
}

// user resolves one old identifier to a target user id.
func (r *migResolver) user(oldIdentifier string, human bool) (string, error) {
	if r.ownerOverride != "" {
		if !r.overrideChecked {
			if u := r.findUser(func(v migUserView) bool { return v.ID == r.ownerOverride }); u == nil {
				return "", resolveErrf("--owner/--reporter %q is not a user in the target system", r.ownerOverride)
			}
			r.overrideChecked = true
		}
		return r.ownerOverride, nil
	}
	if newID, ok := r.usersDone[oldIdentifier]; ok {
		return newID, nil
	}
	if newID, ok := r.userMap[oldIdentifier]; ok {
		if u := r.findUser(func(v migUserView) bool { return v.ID == newID }); u != nil {
			r.usersDone[oldIdentifier] = u.ID
			return u.ID, nil
		}
		return "", resolveErrf("user map: %q → %q: no such user in the target system", oldIdentifier, newID)
	}
	// Existing user with the same email (the human identifier contract).
	if u := r.findUser(func(v migUserView) bool { return human && v.Email == oldIdentifier }); u != nil {
		r.usersDone[oldIdentifier] = u.ID
		return u.ID, nil
	}
	// Existing user with the same display_name (identities without an
	// email — the v0 pilot's users table carries no emails at all).
	if u := r.findUser(func(v migUserView) bool { return v.DisplayName == oldIdentifier }); u != nil {
		r.usersDone[oldIdentifier] = u.ID
		return u.ID, nil
	}
	// Placeholder: a deterministic machine user a re-run re-finds.
	phName := oldIdentifier + " (imported)"
	if u := r.findUser(func(v migUserView) bool { return v.DisplayName == phName }); u != nil {
		r.usersDone[oldIdentifier] = u.ID
		return u.ID, nil
	}
	created, err := r.client.CreateUser(aihubclient.UserCreateRequest{
		DisplayName: phName, UserType: "machine", Role: "writer",
	})
	if err != nil {
		return "", resolveErrf("create placeholder user for %q: %v", oldIdentifier, err)
	}
	var u migUserView
	if err := unmarshalStrict(created, &u); err != nil {
		return "", resolveErrf("create placeholder user for %q: %v", oldIdentifier, err)
	}
	r.users = append(r.users, u)
	r.usersDone[oldIdentifier] = u.ID
	return u.ID, nil
}

func (r *migResolver) findUser(match func(migUserView) bool) *migUserView {
	for i := range r.users {
		if match(r.users[i]) {
			return &r.users[i]
		}
	}
	return nil
}

// userOptional resolves an old id that MAY stay unresolved (assignee
// attribution on terminal records): --user-map entry, an existing target
// user with that id, else nil (blanked — the caller logs it).
func (r *migResolver) userOptional(oldID string) (*string, error) {
	if r.ownerOverride != "" {
		return &r.ownerOverride, nil
	}
	if newID, ok := r.userMap[oldID]; ok {
		if u := r.findUser(func(v migUserView) bool { return v.ID == newID }); u != nil {
			return &u.ID, nil
		}
		return nil, resolveErrf("user map: %q → %q: no such user in the target system", oldID, newID)
	}
	if u := r.findUser(func(v migUserView) bool { return v.ID == oldID }); u != nil {
		return &u.ID, nil
	}
	return nil, nil
}

// migProjectResolution resolves the --project flag once.
func migResolveProject(client *aihubclient.Client, project string) (migProjectView, error) {
	raw, err := client.GetProject(project)
	if err != nil {
		return migProjectView{}, resolveErrf("target project %q: %v (create it first — the migration never invents projects)", project, err)
	}
	var p migProjectView
	if err := unmarshalStrict(raw, &p); err != nil {
		return migProjectView{}, resolveErrf("target project %q: %v", project, err)
	}
	return p, nil
}

// unmarshalStrict is json.Unmarshal with a clearer error.
func unmarshalStrict(raw []byte, v any) error {
	return json.Unmarshal(raw, v)
}
