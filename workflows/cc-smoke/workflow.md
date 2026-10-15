---
name: cc-smoke
description: Claude Code harness smoke lane - a single claude-harness step proving the CC adapter end to end (spawn argv, the claude-config settings.json credential authority, the session transcript, the whitelisted tool surface, the result envelope).
rewind:
  max_total: 1
  max_per_step: 1
defaults:
  harness: claude
  model_candidates: [claude-opus-5-5]
steps:
  - { id: smoke, context_policy: fresh, handoff: false, requires_command_evidence: true }
---

# cc-smoke — Claude Code harness smoke lane

One step, one purpose: prove the Claude Code step harness (polyforge#159,
authority layer per #171) works end to end on this machine. The step itself is
the test subject — its process is a headless `claude -p --bare` under the
daemon-owned `<harness-root>/claude-config`, its credential is that
settings.json's env block, and its whole tool surface is the adapter's
whitelist.

## Shape of the flow

```
smoke  (claude harness, requires_command_evidence: true)
  └─ fail ⇒ rewind once, then terminal
```

## Candidate shape

The candidate list carries bare `claude-*` ids (no provider prefix): that is
the harness tag — `harness.Serves` routes a bare claude id to the Claude
harness and never to pi, exactly as a `provider/model` id routes to pi only.
The id is passed verbatim as `--model`; the gateway resolves it upstream.

## When to use this lane

Dogfooding the CC harness after an install or upgrade, or validating a
credential change under `claude-config/settings.json`. It is not a remediation
lane: no fix pass, no deliver — a fail verdict means the harness chain itself
is broken and a human should read the step's report.
