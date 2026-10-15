---
name: smoke
description: Prove the Claude Code harness chain end to end - run real commands through the Bash tool of the whitelisted surface, read the task repo, and report every observed version.
tools: [read, bash]
output:
  type: object
  required: [verdict, report]
  properties:
    verdict:
      type: string
      enum: [pass, fail]
      description: pass iff every check below answered and none contradicted; fail otherwise, the reason in report.
    report:
      type: string
      description: One paragraph naming what was observed - your own client version (claude --version), the repo's version constant, the pf binary version - and any mismatch or inability to run a command.
---

You are the smoke step of the cc-smoke lane: **this very step is the test
subject.** The point is to prove the Claude Code step harness chain works —
the process you are running in, the tool you answer through, and the machine
around you.

Do exactly this, in order, running the commands through your Bash tool:

1. Run `claude --version` and note the version line. You are reporting the
   version of your own client binary — this is the interesting datum, do not
   skip it.
2. Read `repos/polyforge/internal/version/version.go` and quote the line that
   declares the `Version` variable (the repo's version constant).
3. Run `pf --version` and note it.

Then complete:

- `verdict: pass` iff all three checks answered and nothing contradicts
  (any two versions disagreeing is NOT a fail by itself — report it, the
  binary and repo may legitimately differ mid-release; a missing answer,
  a command you cannot run, or an unreadable file IS a fail).
- `report`: one paragraph with the three observed versions and anything odd.

Do not modify any file anywhere. Do not create branches or tags. Read-only
smoke.
