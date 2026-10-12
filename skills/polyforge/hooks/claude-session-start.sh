#!/bin/sh
# Claude Code SessionStart hook: give this conversation a polyforge chat id.
#
# Reads the hook JSON on stdin (session_id, source), asks the local daemon
# for the chat id of this Claude session via `pf chat init` (a resumed
# session gets the same id), exports PF_CHAT_ID for every later Bash call
# through $CLAUDE_ENV_FILE, and prints a one-line context note.
# The id is a notification label, not a credential. When pf or the daemon
# is unavailable this exits 0 with no output.

command -v pf >/dev/null 2>&1 || exit 0

input=$(cat) || exit 0

if command -v jq >/dev/null 2>&1; then
	sid=$(printf '%s' "$input" | jq -r '.session_id // empty' 2>/dev/null)
else
	sid=$(printf '%s' "$input" | sed -n 's/.*"session_id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)
fi
[ -n "$sid" ] || exit 0

chat=$(pf chat init --harness claude --harness-session "$sid" 2>/dev/null) || exit 0
case "$chat" in
chat_*) ;;
*) exit 0 ;;
esac
# only a plain token may reach the env file
case "$chat" in
*[!A-Za-z0-9_]*) exit 0 ;;
esac

if [ -n "${CLAUDE_ENV_FILE:-}" ]; then
	printf 'export PF_CHAT_ID=%s\n' "$chat" >>"$CLAUDE_ENV_FILE" 2>/dev/null || true
fi
printf 'polyforge chat id: %s\n' "$chat"
exit 0
