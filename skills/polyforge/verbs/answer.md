# answer — answer a pending needs_input question

Execution-plane verb: routed to the local daemon (the answer relay). The
daemon forwards the payload verbatim to the WI Agent over the stdio control
channel (`pf/answer`); it never parses or rewrites the answer.

## Triggers

- EN: answer the question / reply to needs_input / here's my answer / tell
  it X / respond to what it's asking
- 中文: 回答问题 / 它在问我 / 答一下 / 回复它 / 告诉它……

## Command

```
pf answer <id> --text "..." [--question q_xxx] [--answer-id a_xxxx] [--by user]
```

`--question` is the `question_id` the observe output showed; it may be
omitted when exactly one question is pending (the default). `--text` is the
natural-language answer — required. `--answer-id` defaults to a generated
`a_` + 6 chars; supply it explicitly to make a repeat of the same answer
idempotent. `--by` is the answerer's user id, defaulting to the config's
`user_id` / `PF_USER_ID`; the CLI refuses locally if no identity is
resolvable.

## Success

`answer <a_…> delivered for question <q_…>`. Consumption is two-phase:
`delivered` means the relay reached the agent's control channel and the
agent acked; an `ack_pending` outcome means delivered with the consumption
ack still outstanding (a whole model turn can pass before the agent
persists it) — that is NOT an error; re-observe to watch it land.
Re-submitting the **same `answer_id`** is idempotent — it is never applied
twice and returns the same outcome as the first submission.

## Failure branches

| Code | When | What to tell the user |
|---|---|---|
| `DAEMON-409` | The `question_id` does not match the waiting step's persisted question, was already consumed, or another delivery of the same answer id is in flight | The question is stale or mistyped — observe again, take the currently printed `question_id`, and answer that one; for an in-flight duplicate, simply retry. |
| `DAEMON-403` | The `--by` identity does not match the request's caller identity | The answerer identity and the caller identity must be the same — fix `--by` / `PF_USER_ID`, or answer as yourself. |
| `DAEMON-404` | No local job for this work item | The work item is not running on this machine — answer from the machine that runs it. |
| (no code) `answer: --text is required` / `… identity required` | Local usage refusal | Fill in the required flags; no request left the machine. |
| `DAEMON-500` | Delivery failed (agent dead before the ack) | The answer id stays re-deliverable — retry after the agent recovers; nothing was lost. |
| `dial unix …` | Daemon not running | Start the daemon or check `--socket`. |

Answers bind per `question_id`: if the step re-asks, it mints a **new**
question id — never reuse an old answer command; observe and answer the new
question.
