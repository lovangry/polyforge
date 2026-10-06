## NL Routing

Maps non-obvious intent -> operations. If nothing fits or is ambiguous,
**ask rather than guess**; each routing verb's Triggers section is
authoritative. NL Routing decides *what operation*; **Repo Routing** decides
*which repo*.

| intent | operation |
|---|---|
| ready today / ready queue | `pf queue --ready` |
| what needs my decision | `pf ops status`, then `pf observe` jobs pending a question |
| run it / execute / take this one + slug | `polyforge` 路由:submit <slug> |
| resume + slug | `polyforge` 路由:resume <slug> |
| pause / wrap / finished / fail | `polyforge` 路由:pause \| wrap |
| begin / new task / start | `polyforge` 路由:file(建票)|
| design / spec / brainstorm | `polyforge` 路由:file(写进票的 goal/content)|
| this bug / debug | 同上(file 建 bug 票;执行在胶囊内的 grill/spec 步)|
| doctor / can't connect | `pf ops status` + `operator-skills/polyforge/verbs/doctor.md` |
| release / cut / ship it | `polyforge` 路由:ship |

### Disambiguation

- **continue / next** - mid-flow/step -> next step, **not** resume; only
  "resume `<slug>`" resumes a **paused** wi.
- **done / finished** - mid-step -> continue; whole-wi ("wrap up") -> `wrap` 动词。
