package workitem

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ExecutionStateKeyBlacklist is the INV-2c recursive key-name vocabulary
// (02 §5.1a). It covers every field of the current execution.json schema
// (03 §4.2). attrs and wi_events payloads are scanned recursively at every
// object depth; any hit ⇒ CORE-400 EXECUTION_STATE_REJECTED.
var ExecutionStateKeyBlacklist = []string{
	"current_step", "step_id", "step_attempt_id", "attempt", "review_round",
	"review_rounds", "review_verdict", "progress", "workflow_status", "steps",
	"rewind_stats", "budget_consumed", "command_count", "step_start_refs",
	"pending_question",
}

var blacklistSet = func() map[string]bool {
	m := map[string]bool{}
	for _, k := range ExecutionStateKeyBlacklist {
		m[k] = true
	}
	return m
}()

// CheckExecutionStateKeys walks v (a decoded JSON value) recursively and
// returns the full path of the first blacklisted object key, or "".
func CheckExecutionStateKeys(v any, root string) string {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			path := root + "." + k
			if blacklistSet[k] {
				return path
			}
			if hit := CheckExecutionStateKeys(val, path); hit != "" {
				return hit
			}
		}
	case []any:
		for i, val := range t {
			if hit := CheckExecutionStateKeys(val, fmt.Sprintf("%s[%d]", root, i)); hit != "" {
				return hit
			}
		}
	}
	return ""
}

// ValidateAttrs enforces INV-2c on the attrs JSON object of a create/update.
func ValidateAttrs(attrs map[string]any) error {
	if attrs == nil {
		return nil
	}
	if hit := CheckExecutionStateKeys(attrs, "attrs"); hit != "" {
		return fmt.Errorf("CORE-400 EXECUTION_STATE_REJECTED: execution-state key at %s", hit)
	}
	return nil
}

// eventSchemas is the closed per-event payload schema table (02 §5.1a).
// Key sets are closed: unknown payload keys are rejected alongside the
// recursive blacklist.
var eventSchemas = map[string]map[string]bool{
	"claimed":            {"generation": true, "reason": true},
	"paused":             {"generation": true, "reason": true},
	"resumed":            {"generation": true, "reason": true},
	"blocked":            {"generation": true, "reason": true},
	"unblocked":          {"generation": true, "reason": true},
	"wrapped":            {"generation": true, "reason": true},
	"failed":             {"generation": true, "reason": true},
	"cancelled":          {"generation": true, "reason": true},
	"capsule_registered": {"machine_id": true, "generation": true},
	"capsule_takeover":   {"machine_id": true, "generation": true},
	"budget_incremented": {"seq": true, "dimension": true, "amount": true},
	"secret_rotated":     {"epoch": true},
	"note":               {"text": true},
}

// AllowedEventTypes reports whether eventType is in the §5.1a enumeration.
func AllowedEventTypes(eventType string) bool {
	_, ok := eventSchemas[eventType]
	return ok
}

// ValidateEvent validates an event append: closed type enumeration, closed
// payload key set, recursive execution-state blacklist, note size cap.
func ValidateEvent(eventType string, payload map[string]any) error {
	schema, ok := eventSchemas[eventType]
	if !ok {
		return fmt.Errorf("CORE-400 UNKNOWN_EVENT_TYPE: %q", eventType)
	}
	if payload == nil {
		payload = map[string]any{}
	}
	for k := range payload {
		if !schema[k] {
			return fmt.Errorf("CORE-400 UNKNOWN_EVENT_TYPE: payload key %q not in schema of %q", k, eventType)
		}
	}
	root := "payload"
	if hit := CheckExecutionStateKeys(payload, root); hit != "" {
		return fmt.Errorf("CORE-400 EXECUTION_STATE_REJECTED: execution-state key at %s", hit)
	}
	if eventType == "note" {
		if text, ok := payload["text"].(string); ok && len(text) > 4*1024 {
			return fmt.Errorf("CORE-400 note text exceeds 4KB")
		}
	}
	return nil
}

// EncodePayload renders a payload map for storage, tolerating nil.
func EncodePayload(payload map[string]any) string {
	if payload == nil {
		return "{}"
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// hasPrefixFold reports whether s starts with prefix (case-insensitive).
func hasPrefixFold(s, prefix string) bool { return strings.HasPrefix(strings.ToLower(s), prefix) }
