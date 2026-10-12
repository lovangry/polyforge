// pf-chat: gives every pi session a polyforge chat id (PF_CHAT_ID) and runs
// the resident `pf watch` for it.
//
// On session_start it asks the local daemon, via `pf chat init`, for the
// chat id of this pi session (a resumed session gets the same id). Every
// bash tool call is then prefixed with `PF_CHAT_ID=<id> ` so pf commands
// the model runs know which chat they belong to. The id is a notification
// label, not a credential.
//
// Watching (v2-converge §3): the extension spawns the resident
// `pf watch --chat <id>`; its stdout carries ONLY fixed-template decision
// lines, each of which wakes the agent (sendMessage + triggerTurn). Progress
// never enters the model context: every 2s the widget is refreshed from
// `pf watch --chat <id> --statusline` (reads the watcher's progress file).
// Without pf or a daemon this does nothing.
import { spawn, type ChildProcess } from "node:child_process";
import { createInterface } from "node:readline";
import { isToolCallEventType, type ExtensionAPI } from "@earendil-works/pi-coding-agent";

const WIDGET_KEY = "polyforge";
const POLL_MS = 2000;
const RESTART_MS = 5000;

export default function (pi: ExtensionAPI) {
  let chatId = "";
  let watcher: ChildProcess | undefined;
  let poller: ReturnType<typeof setInterval> | undefined;
  let restart: ReturnType<typeof setTimeout> | undefined;
  let stopped = true;

  function stopWatching(ctx?: { ui: { setWidget(key: string, content: string[] | undefined): void } }) {
    stopped = true;
    if (poller) clearInterval(poller);
    if (restart) clearTimeout(restart);
    poller = restart = undefined;
    const child = watcher;
    watcher = undefined;
    if (child && !child.killed) child.kill("SIGTERM");
    ctx?.ui.setWidget(WIDGET_KEY, undefined);
  }

  function startWatcher() {
    if (stopped || !chatId) return;
    const child = spawn("pf", ["watch", "--chat", chatId], { stdio: ["ignore", "pipe", "ignore"] });
    watcher = child;
    child.on("error", () => {
      // pf missing: nothing to watch
      if (watcher === child) watcher = undefined;
    });
    createInterface({ input: child.stdout! }).on("line", (line) => {
      // Only fixed-template decision lines are ever printed on stdout.
      if (!line.startsWith("[polyforge] ")) return;
      pi.sendMessage(
        { customType: "polyforge-decision", content: line, display: true },
        { triggerTurn: true },
      );
    });
    child.on("exit", () => {
      if (watcher === child) watcher = undefined;
      // unexpected end (lock race, crash): retry unless the session ended
      if (!stopped) restart = setTimeout(startWatcher, RESTART_MS);
    });
  }

  pi.on("session_start", async (_event, ctx) => {
    stopWatching(ctx);
    chatId = "";
    try {
      const sessionId = ctx.sessionManager.getSessionId();
      const res = await pi.exec("pf", ["chat", "init", "--harness", "pi", "--harness-session", sessionId]);
      const id = res.stdout.trim();
      if (res.code === 0 && /^chat_[A-Za-z0-9_]+$/.test(id)) chatId = id;
    } catch {
      // pf missing or daemon down: run without a chat id
    }
    if (!chatId) return;
    stopped = false;
    startWatcher();
    const id = chatId;
    poller = setInterval(async () => {
      try {
        const res = await pi.exec("pf", ["watch", "--chat", id, "--statusline"]);
        const lines = res.code === 0 ? res.stdout.split("\n").filter((l) => l.trim() !== "") : [];
        ctx.ui.setWidget(WIDGET_KEY, lines.length > 0 ? lines : undefined);
      } catch {
        ctx.ui.setWidget(WIDGET_KEY, undefined);
      }
    }, POLL_MS);
  });

  pi.on("session_shutdown", async (_event, ctx) => {
    stopWatching(ctx);
  });

  pi.on("tool_call", async (event) => {
    if (chatId && isToolCallEventType("bash", event)) {
      event.input.command = `PF_CHAT_ID=${chatId} ${event.input.command}`;
    }
  });
}
