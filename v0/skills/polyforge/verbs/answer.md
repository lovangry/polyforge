# answer — answer a pending needs_input question

Execution-plane verb: routed to the local daemon (00 §5.1; the relay message
schema is 00 §3.4a, carried by the 04 §5.1 `answer` operation). The daemon
forwards the payload verbatim; it never parses or rewrites the answer.

## Triggers

- EN: answer the question / reply to needs_input / here's my answer / tell
  it X / respond to what it's asking
- 中文: 回答问题 / 它在问我 / 答一下 / 回复它 / 告诉它……

## Command

```
pf answer <id> --question q_xxx --text "..." [--answer-id a_xxxx] [--by user]
```

`--question` is the `question_id` the observe output showed — required and
exact. `--text` is the natural-language answer — required.
`--answer-id` defaults to a generated `a_` + 6 chars; supply it explicitly to
make a repeat of the same answer idempotent. `--by` is the answerer's user
id, defaulting to the config's `user_id` / `PF_USER_ID`; the CLI refuses
locally if no identity is resolvable.

## Success

Human mode: `answer <a_…> delivered for question <q_…>`. `--json`: the raw
response. Re-submitting the **same `answer_id`** is idempotent — it is not
applied twice and returns the same result as the first submission.

## Failure branches

| Code | When | What to tell the user |
|---|---|---|
| `DAEMON-409` | The `question_id` does not match the waiting step's persisted question, was already consumed (the step resumed and cleared it), or no step is waiting for input at all | The question is stale or mistyped — observe again, take the currently printed `question_id`, and answer that one. |
| `DAEMON-403` | The `--by` identity does not match the request's authenticated user | The answerer identity and the caller identity must be the same — fix `--by` / `PF_USER_ID`, or answer as yourself. |
| `DAEMON-404` | No local job for this work item | The work item is not running on this machine — answer from the machine that runs it. |
| (no code) `answer: --question and --text are required` / `… identity required` | Local usage refusal | Fill in the required flags; no request left the machine. |
| `dial unix …` | Daemon not running | Start the daemon or check `--socket`. |

Answers bind per `question_id`: if the step re-asks, it mints a **new**
question id — never reuse an old answer command; observe and answer the new
question.
