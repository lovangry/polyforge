package watch

// dispatch.go: the harness one-shot dispatch adapter. The adapter
// DISPATCHES — it runs the harness binary the (normalized) harness names
// in one-shot print mode with EXACTLY the model the caller hands it; it
// never selects a model, never falls back, never retries on a different
// one. `pi -p` / `claude -p` with the per-harness configured model.

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Dispatcher is the one-shot dispatch seam (tests use a counting fake).
type Dispatcher interface {
	// Dispatch runs one summarize request and answers the model's
	// output text.
	Dispatch(harness, model, prompt string) (string, error)
}

// DefaultDispatchTimeout bounds one summarize dispatch: a hung model
// call must not stall the watcher's log channel forever.
const DefaultDispatchTimeout = 5 * time.Minute

// ExecDispatcher dispatches through the real harness binary.
type ExecDispatcher struct {
	// Timeout bounds one dispatch; <= 0 means DefaultDispatchTimeout.
	Timeout time.Duration
}

// DispatchArgs builds the one-shot argv (exposed for tests): print mode
// first, then exactly the configured model. The PROMPT rides stdin, never
// argv — a transcript tail routinely exceeds the kernel's per-argument
// limit (~128KB) and fork/exec fails with "argument list too long"
// (caught live: polyforge#117 field use, 2026-10-11).
func DispatchArgs(model string) []string {
	return []string{"-p", "--model", model}
}

// dispatchBinary maps the normalized harness onto the binary name.
func dispatchBinary(harness string) (string, error) {
	switch harness {
	case "pi":
		return "pi", nil
	case "claude":
		return "claude", nil
	default:
		return "", fmt.Errorf("watch: cannot dispatch harness %q — the adapters are pi and claude", harness)
	}
}

// Dispatch runs the harness binary once: `pi -p --model <model> <prompt>`
// / `claude -p --model <model> <prompt>`. A nonzero exit or a timeout is
// an error whose text carries the harness's RAW stderr — the caller
// surfaces it verbatim in summary_error, never faked away.
func (d ExecDispatcher) Dispatch(harness, model, prompt string) (string, error) {
	bin, err := dispatchBinary(harness)
	if err != nil {
		return "", err
	}
	timeout := d.Timeout
	if timeout <= 0 {
		timeout = DefaultDispatchTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, DispatchArgs(model)...)
	cmd.Stdin = strings.NewReader(prompt)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(errb.String())
		if detail == "" {
			detail = strings.TrimSpace(out.String())
		}
		if detail != "" {
			return "", fmt.Errorf("%s -p dispatch failed: %v: %s", bin, err, detail)
		}
		return "", fmt.Errorf("%s -p dispatch failed: %v", bin, err)
	}
	return out.String(), nil
}
