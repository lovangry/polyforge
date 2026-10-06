## Three-Segment Output Format

Every polyforge skill response MUST follow this format exactly. Violations = bug.
(Values illustrative; use live ones.)

```markdown
## Result
<1-2 sentences, verb-first. State errors here explicitly.>

## Status
| field | value |
|---|---|
| wi | <project#seq> |
| status | running |
| session_id | <wi-agent session> |
| spawn_n | 3 |
| step | fix |
| machine | <this machine> |

## Next steps
- `pf observe <id>` - watch progress and pending questions
(max 5 items; _none_ if none)
```

Multi-wi lists replace the table with the live queue columns:
`slug / status / priority / bundle / goal`.

## After `pf submit`

The daemon claims the job asynchronously and spawns the WI Agent - this
session never executes the workflow; `pf observe <id>` is the follow-up. Hand
an executor outcome and acceptance criteria only, never an execution path:
`claim -> commit -> push -> PR` in a prompt is followed literally and skips
the graph.
