package cli

import (
	"flag"
	"fmt"
	"strings"
)

// Management face M1–M5 (02 §5.1 F group; same port, same auth, admin
// unless noted). These are NOT part of the 12 routing verbs — they are
// the `pf` management CLI surface the design hands the old pf-user /
// pf-project skills to (00 §11.1 migration matrix).

// ---------------------------------------------------------------------------
// users (M1)
// ---------------------------------------------------------------------------

var cmdUsers = &Command{
	Name:    "users",
	Summary: "User management (M1, admin only)",
	Usage: "pf users create --display-name N [--kind human|machine] [--role writer|admin]\n" +
		"pf users list\n" +
		"pf users get <id>\n" +
		"pf users patch <id> [--display-name N] [--role writer|admin]",
	Setup: func(fs *flag.FlagSet) {},
	Run: func(ctx *Context, fs *flag.FlagSet, args []string) error {
		if len(args) == 0 {
			return fmt.Errorf("users: expected create|list|get|patch")
		}
		client := NewClient(ctx.Cfg.Server, ctx.Cfg.APIKey)
		sub, rest := args[0], args[1:]
		switch sub {
		case "create":
			f := flag.NewFlagSet("pf users create", flag.ContinueOnError)
			f.SetOutput(ctx.Stderr)
			name := f.String("display-name", "", "human-readable display name (required)")
			kind := f.String("kind", "human", "human|machine")
			role := f.String("role", "writer", "writer|admin (global role)")
			rest, err := parseCommandArgs(f, rest)
			if err != nil {
				return err
			}
			if len(rest) != 0 || *name == "" {
				return fmt.Errorf("users create: --display-name is required")
			}
			raw, err := client.CreateUser(&UserCreateRequest{
				DisplayName: *name,
				Kind:        *kind,
				Role:        *role,
			})
			if err != nil {
				return err
			}
			ctx.emit(raw)
			if !ctx.JSON {
				var u userView
				if err := decodeJSON(raw, &u); err == nil {
					fmt.Fprintf(ctx.Stdout, "created user %s (%s, %s)\n", u.ID, u.Kind, u.Role)
				}
			}
			return nil
		case "list":
			raw, err := client.ListUsers()
			if err != nil {
				return err
			}
			ctx.emit(raw)
			if !ctx.JSON {
				var l userListView
				if err := decodeJSON(raw, &l); err == nil {
					renderUsers(ctx.Stdout, &l)
				}
			}
			return nil
		case "get":
			if len(rest) != 1 {
				return fmt.Errorf("users get: expected exactly one user id")
			}
			raw, err := client.GetUser(rest[0])
			if err != nil {
				return err
			}
			ctx.emit(raw)
			if !ctx.JSON {
				printJSON(ctx.Stdout, raw)
			}
			return nil
		case "patch":
			f := flag.NewFlagSet("pf users patch", flag.ContinueOnError)
			f.SetOutput(ctx.Stderr)
			name := f.String("display-name", "", "new display name")
			role := f.String("role", "", "writer|admin")
			ids, err := parseCommandArgs(f, rest)
			if err != nil {
				return err
			}
			if len(ids) != 1 || (*name == "" && *role == "") {
				return fmt.Errorf("users patch: expected <id> and at least one of --display-name / --role")
			}
			raw, err := client.PatchUser(ids[0], &UserPatchRequest{
				DisplayName: *name,
				Role:        *role,
			})
			if err != nil {
				return err
			}
			ctx.emit(raw)
			if !ctx.JSON {
				fmt.Fprintf(ctx.Stdout, "user %s updated\n", ids[0])
			}
			return nil
		default:
			return fmt.Errorf("users: unknown action %q", sub)
		}
	},
}

// ---------------------------------------------------------------------------
// api-keys (M2)
// ---------------------------------------------------------------------------

var cmdApiKeys = &Command{
	Name:    "api-keys",
	Summary: "API key management (M2, admin only; plaintext shown exactly once)",
	Usage: "pf api-keys create --user U [--project-scope P]\n" +
		"pf api-keys list\n" +
		"pf api-keys revoke <id>",
	Setup: func(fs *flag.FlagSet) {},
	Run: func(ctx *Context, fs *flag.FlagSet, args []string) error {
		if len(args) == 0 {
			return fmt.Errorf("api-keys: expected create|list|revoke")
		}
		client := NewClient(ctx.Cfg.Server, ctx.Cfg.APIKey)
		sub, rest := args[0], args[1:]
		switch sub {
		case "create":
			f := flag.NewFlagSet("pf api-keys create", flag.ContinueOnError)
			f.SetOutput(ctx.Stderr)
			user := f.String("user", "", "user id to mint the key for (required)")
			scope := f.String("project-scope", "", "optional project scope (narrows, never grants)")
			rest, err := parseCommandArgs(f, rest)
			if err != nil {
				return err
			}
			if len(rest) != 0 || *user == "" {
				return fmt.Errorf("api-keys create: --user is required")
			}
			req := &ApiKeyCreateRequest{UserID: *user}
			if *scope != "" {
				req.ProjectID = strPtr(*scope)
			}
			raw, err := client.CreateApiKey(req)
			if err != nil {
				return err
			}
			ctx.emit(raw)
			if !ctx.JSON {
				printJSON(ctx.Stdout, raw)
				fmt.Fprintln(ctx.Stdout, "NOTE: the key plaintext is shown exactly once and never stored.")
			}
			return nil
		case "list":
			raw, err := client.ListApiKeys()
			if err != nil {
				return err
			}
			ctx.emit(raw)
			if !ctx.JSON {
				var l apiKeyListView
				if err := decodeJSON(raw, &l); err == nil {
					renderApiKeys(ctx.Stdout, &l)
				}
			}
			return nil
		case "revoke":
			if len(rest) != 1 {
				return fmt.Errorf("api-keys revoke: expected exactly one key id")
			}
			raw, err := client.RevokeApiKey(rest[0])
			if err != nil {
				return err
			}
			ctx.emit(raw)
			if !ctx.JSON {
				fmt.Fprintf(ctx.Stdout, "key %s revoked\n", rest[0])
			}
			return nil
		default:
			return fmt.Errorf("api-keys: unknown action %q", sub)
		}
	},
}

// ---------------------------------------------------------------------------
// projects (M3), including membership add/remove
// ---------------------------------------------------------------------------

var cmdProjects = &Command{
	Name:    "projects",
	Summary: "Project and membership management (M3)",
	Usage: "pf projects create --name N [--visible]\n" +
		"pf projects list\n" +
		"pf projects get <id-or-name>\n" +
		"pf projects patch <id-or-name> [--visible[=true|false]]\n" +
		"pf projects members add <id-or-name> --user U --role viewer|writer|maintainer\n" +
		"pf projects members remove <id-or-name> --user U [--user U ...]",
	Setup: func(fs *flag.FlagSet) {},
	Run: func(ctx *Context, fs *flag.FlagSet, args []string) error {
		if len(args) == 0 {
			return fmt.Errorf("projects: expected create|list|get|patch|members")
		}
		client := NewClient(ctx.Cfg.Server, ctx.Cfg.APIKey)
		sub, rest := args[0], args[1:]
		switch sub {
		case "create":
			f := flag.NewFlagSet("pf projects create", flag.ContinueOnError)
			f.SetOutput(ctx.Stderr)
			name := f.String("name", "", "project name (required)")
			visible := f.Bool("visible", true, "directory-listing visibility (grants no data access)")
			rest, err := parseCommandArgs(f, rest)
			if err != nil {
				return err
			}
			if len(rest) != 0 || *name == "" {
				return fmt.Errorf("projects create: --name is required")
			}
			raw, err := client.CreateProject(&ProjectCreateRequest{
				Name:    *name,
				Visible: visible,
			})
			if err != nil {
				return err
			}
			ctx.emit(raw)
			if !ctx.JSON {
				var p projectView
				if err := decodeJSON(raw, &p); err == nil {
					fmt.Fprintf(ctx.Stdout, "created project %s (%s)\n", p.Name, p.ID)
				}
			}
			return nil
		case "list":
			raw, err := client.ListProjects()
			if err != nil {
				return err
			}
			ctx.emit(raw)
			if !ctx.JSON {
				var l projectListView
				if err := decodeJSON(raw, &l); err == nil {
					renderProjects(ctx.Stdout, &l)
				}
			}
			return nil
		case "get":
			if len(rest) != 1 {
				return fmt.Errorf("projects get: expected exactly one project id or name")
			}
			raw, err := client.GetProject(rest[0])
			if err != nil {
				return err
			}
			ctx.emit(raw)
			if !ctx.JSON {
				printJSON(ctx.Stdout, raw)
			}
			return nil
		case "patch":
			f := flag.NewFlagSet("pf projects patch", flag.ContinueOnError)
			f.SetOutput(ctx.Stderr)
			// Explicit-set tracking: the wire body carries `visible` ONLY
			// when the flag was given (openapi ProjectPatchRequest marks
			// it optional). A plain patch must not flip a private project
			// visible (02 §5.1 M3).
			visible := optBool{}
			f.Var(&visible, "visible", "directory-listing visibility (omit to leave unchanged)")
			ids, err := parseCommandArgs(f, rest)
			if err != nil {
				return err
			}
			if len(ids) != 1 {
				return fmt.Errorf("projects patch: expected one project id or name")
			}
			raw, err := client.PatchProject(ids[0], &ProjectPatchRequest{Visible: visible.Ptr()})
			if err != nil {
				return err
			}
			ctx.emit(raw)
			if !ctx.JSON {
				fmt.Fprintf(ctx.Stdout, "project %s updated\n", ids[0])
			}
			return nil
		case "members":
			return projectsMembers(ctx, client, rest)
		default:
			return fmt.Errorf("projects: unknown action %q", sub)
		}
	},
}

// projectsMembers handles `pf projects members add|remove ...` — both map
// onto the M3 project PATCH (members upsert / remove_members, the
// implementation's simpler physical-removal scheme, api/openapi.yaml fix
// #18).
func projectsMembers(ctx *Context, client *Client, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("projects members: expected add|remove")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "add":
		f := flag.NewFlagSet("pf projects members add", flag.ContinueOnError)
		f.SetOutput(ctx.Stderr)
		user := f.String("user", "", "user id (required)")
		role := f.String("role", "writer", "viewer|writer|maintainer")
		ids, err := parseCommandArgs(f, rest)
		if err != nil {
			return err
		}
		if len(ids) != 1 || *user == "" {
			return fmt.Errorf("projects members add: expected <id-or-name> --user U --role R")
		}
		switch *role {
		case "viewer", "writer", "maintainer":
		default:
			return fmt.Errorf("projects members add: --role must be viewer|writer|maintainer")
		}
		members := []ProjectMember{{UserID: *user, Role: *role}}
		raw, err := client.PatchProject(ids[0], &ProjectPatchRequest{Members: &members})
		if err != nil {
			return err
		}
		ctx.emit(raw)
		if !ctx.JSON {
			fmt.Fprintf(ctx.Stdout, "member %s upserted on %s as %s\n", *user, ids[0], *role)
		}
		return nil
	case "remove", "rm":
		f := flag.NewFlagSet("pf projects members remove", flag.ContinueOnError)
		f.SetOutput(ctx.Stderr)
		var users stringList
		f.Var(&users, "user", "user id to remove (repeatable)")
		ids, err := parseCommandArgs(f, rest)
		if err != nil {
			return err
		}
		if len(ids) != 1 || len(users) == 0 {
			return fmt.Errorf("projects members remove: expected <id-or-name> --user U [--user U ...]")
		}
		rem := []string(users)
		raw, err := client.PatchProject(ids[0], &ProjectPatchRequest{RemoveMembers: &rem})
		if err != nil {
			return err
		}
		ctx.emit(raw)
		if !ctx.JSON {
			fmt.Fprintf(ctx.Stdout, "removed from %s: %s\n", ids[0], strings.Join(rem, ","))
		}
		return nil
	default:
		return fmt.Errorf("projects members: unknown action %q", sub)
	}
}

// ---------------------------------------------------------------------------
// machines — row 21: the operator's machine-registration face (02 §4.10:
// the FIRST registration call is made by the deployment operator with a
// human/admin api key; the response carries the machine's one-time api
// key). Not one of the 12 routing verbs nor an M-group row, but it is a
// documented operator duty the pf-daemon fatal message itself points to
// ("register this machine via POST /api/v1/machines/heartbeat first") —
// before the polyforge#17 dogfood round there was NO CLI path for it.
// ---------------------------------------------------------------------------

var cmdMachines = &Command{
	Name:    "machines",
	Summary: "Machine registration/heartbeat (row 21; operator duty)",
	Usage: "pf machines register <machine-id> [--version V] [--display-name N]\n" +
		"pf machines heartbeat <machine-id> [--version V]",
	Setup: func(fs *flag.FlagSet) {},
	Run: func(ctx *Context, fs *flag.FlagSet, args []string) error {
		if len(args) == 0 {
			return fmt.Errorf("machines: expected register|heartbeat")
		}
		client := NewClient(ctx.Cfg.Server, ctx.Cfg.APIKey)
		sub, rest := args[0], args[1:]
		switch sub {
		case "register", "heartbeat":
			f := flag.NewFlagSet("pf machines "+sub, flag.ContinueOnError)
			f.SetOutput(ctx.Stderr)
			version := f.String("version", "", "runtime version stamp")
			name := f.String("display-name", "", "machine display name")
			ids, err := parseCommandArgs(f, rest)
			if err != nil {
				return err
			}
			if len(ids) != 1 || ids[0] == "" {
				return fmt.Errorf("machines %s: expected exactly one machine id", sub)
			}
			raw, err := client.RegisterMachine(&MachineRegisterRequest{
				MachineID: ids[0], RuntimeVersion: *version, DisplayName: *name,
			})
			if err != nil {
				return err
			}
			ctx.emit(raw)
			if !ctx.JSON {
				var r struct {
					MachineID  string `json:"machine_id"`
					UserID     string `json:"user_id"`
					Registered bool   `json:"registered"`
					APIKey     string `json:"api_key"`
				}
				if err := decodeJSON(raw, &r); err == nil {
					if r.Registered {
						fmt.Fprintf(ctx.Stdout, "machine %s registered (user %s)\n", r.MachineID, r.UserID)
						fmt.Fprintf(ctx.Stdout, "machine api key (shown exactly once): %s\n", r.APIKey)
						fmt.Fprintln(ctx.Stdout, "NOTE: put it in the daemon's --api-key-file / POLYFORGE_API_KEY; it is never stored again.")
					} else {
						fmt.Fprintf(ctx.Stdout, "machine %s heartbeat refreshed (already registered)\n", r.MachineID)
					}
				} else {
					printJSON(ctx.Stdout, raw)
				}
			}
			return nil
		default:
			return fmt.Errorf("machines: unknown action %q", sub)
		}
	},
}

// ---------------------------------------------------------------------------
// backup (M4) and rotate-secret (M5)
// ---------------------------------------------------------------------------

var cmdBackup = &Command{
	Name:    "backup",
	Summary: "Trigger an on-demand backup snapshot (M4, admin only)",
	Usage:   "pf backup",
	Setup:   func(fs *flag.FlagSet) {},
	Run: func(ctx *Context, fs *flag.FlagSet, args []string) error {
		raw, err := NewClient(ctx.Cfg.Server, ctx.Cfg.APIKey).Backup()
		if err != nil {
			return err
		}
		ctx.emit(raw)
		if !ctx.JSON {
			var resp struct {
				Backup string `json:"backup"`
			}
			if err := decodeJSON(raw, &resp); err == nil {
				fmt.Fprintf(ctx.Stdout, "snapshot written: %s\n", resp.Backup)
			} else {
				printJSON(ctx.Stdout, raw)
			}
		}
		return nil
	},
}

var cmdRotateSecret = &Command{
	Name:    "rotate-secret",
	Summary: "Rotate the service secret (M5, admin only)",
	Usage: "pf rotate-secret --mode grace --grace-seconds N\n" +
		"pf rotate-secret --mode rollback",
	Setup: func(fs *flag.FlagSet) {
		fs.String("mode", "", "grace (old epochs valid for a window) | rollback (immediate, permanent)")
		fs.Int64("grace-seconds", 0, "grace window seconds (mode=grace)")
	},
	Run: func(ctx *Context, fs *flag.FlagSet, args []string) error {
		if len(args) != 0 {
			return fmt.Errorf("rotate-secret: unexpected arguments %v", args)
		}
		mode := stringFlag(fs, "mode")
		grace := int64Flag(fs, "grace-seconds")
		switch mode {
		case "grace":
			if grace <= 0 {
				return fmt.Errorf("rotate-secret: --grace-seconds is required for mode=grace")
			}
		case "rollback":
			// grace_seconds MUST be 0 for rollback (CORE-400 otherwise).
			grace = 0
		default:
			return fmt.Errorf("rotate-secret: --mode must be grace|rollback")
		}
		raw, err := NewClient(ctx.Cfg.Server, ctx.Cfg.APIKey).RotateSecret(&RotateSecretRequest{
			Mode:         mode,
			GraceSeconds: grace,
		})
		if err != nil {
			return err
		}
		ctx.emit(raw)
		if !ctx.JSON {
			fmt.Fprintf(ctx.Stdout, "service secret rotated (mode=%s)\n", mode)
		}
		return nil
	},
}
