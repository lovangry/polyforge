---
name: pf-project
description: >
  Use when the user wants to create, update, or list polyforge projects, or rotate
  a project access identifier.
---

# pf-project - Project Management

## Usage

**Purpose**: Manage polyforge projects - create, update, rotate access identifier, or list.

**Pattern**: `/pf-project { create | update | rotate | list } [<name>] [--description <text>] [--visible | --private] [--scenario <name>] [--repos <json>]`

**Required**: a sub-mode (see below)

**Flags**:
- `--description <text>` - project description
- `--visible` / `--private` - visibility toggle (default visible on create)
- `--scenario <git-repo-url>` - scenario repository URL, e.g. `git@github.com:GMISWE/polyforge-coding.git` (omitted = unset; a bare name like `coding` is rejected)
- `--repos <json>` - array of `{name, url, github_owner_repo, description}` entries
- `rotate` is destructive: existing identifier is invalidated and a new one is issued

## When to use

- "create project" / "new project" / "make a project"
- "update project" / "update project repos"
- "rotate identifier" / "rotate identifier" / "reset access token"
- "list projects" / "list projects" / "show projects"

## Mechanic

### Mode A - Create project

```
pf_create_project(
  name=<lowercase, must match ^[a-z][a-z0-9_-]{0,39}$ (a lowercase letter first, then a-z0-9_- up to 40 chars total)>,
  description=<optional>,
  visible=<true|false, default true>,
  scenario=<git repo URL, e.g. git@github.com:GMISWE/polyforge-coding.git; omitted = unset>,
  repos=<optional array of {name, url, github_owner_repo, description}>
)
```

- Any writer+ can create
- `visible=false` -> private project; others gain access through project membership (`pf_update_project` members: `viewer|writer|maintainer`); the identifier token grants **viewer access only**, never write access
- name is globally unique and cannot be changed after creation

### Mode B - Update project

```
pf_update_project(
  name=<project name>,
  description=<optional>,
  visible=<optional>,
  repos=<optional array>
)
```

- Only owner/admin can perform this
- Within a project, repo name/url must be unique

### Mode C - Rotate identifier

```
result = pf_rotate_identifier(name=<project name>)
# result: {plain: "pi_xxx...", prefix: "pi_ab12"}
```

- Only owner/admin can perform this
- After rotation the old identifier is **invalidated immediately**
- `plain` is the plaintext token, **shown only once** - tell the user to save it immediately
- `prefix` identifies this token (useful if it is used in multiple places)

**Show to the user:**
```
Identifier rotated
Token: pi_xxxxxxxxxxxxx...  <- save it now; it cannot be viewed again
Prefix: pi_ab12
```

### Mode D - List projects

```
projects = pf_list_projects()
# returns the projects visible to the caller
```

Display format:
```
| name        | visible | owner    | repos |
|-------------|---------|----------|-------|
| marketplace | public  | xiaokang | 1     |
| aihub       | public  | xiaokang | 1     |
```

## Output (three-segment format)

**Create/Update/Rotate:**
```
## Result
Project '<name>' has been [created / updated / identifier rotated].

## Status
| field   | value           |
|---------|-----------------|
| name    | <name>          |
| visible | public/private  |
| owner   | <display>       |
| repos   | <count>         |

## Next steps
- Use /pf-work --goal "..." to create a wi under this project
- To grant others write access: add them as members via `pf_update_project` (viewer|writer|maintainer; full-list replacement with `members_version` and declared `expected_removals`) - sharing the identifier grants viewer access only
```

## NL Triggers

- "create project" / "new project" / "make a project"
- "update project" / "change repos" / "update project"
- "rotate identifier" / "reset token" / "rotate access"
- "list projects" / "show projects" / "list projects"
