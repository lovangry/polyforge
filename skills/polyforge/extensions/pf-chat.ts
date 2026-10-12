// pf-chat: gives every pi session a polyforge chat id (PF_CHAT_ID).
//
// On session_start it asks the local daemon, via `pf chat init`, for the
// chat id of this pi session (a resumed session gets the same id). Every
// bash tool call is then prefixed with `PF_CHAT_ID=<id> ` so pf commands
// the model runs know which chat they belong to. The id is a notification
// label, not a credential. Without pf or a daemon this does nothing.
import { isToolCallEventType, type ExtensionAPI } from "@earendil-works/pi-coding-agent";

export default function (pi: ExtensionAPI) {
  let chatId = "";

  pi.on("session_start", async (_event, ctx) => {
    chatId = "";
    try {
      const sessionId = ctx.sessionManager.getSessionId();
      const res = await pi.exec("pf", ["chat", "init", "--harness", "pi", "--harness-session", sessionId]);
      const id = res.stdout.trim();
      if (res.code === 0 && /^chat_[A-Za-z0-9_]+$/.test(id)) chatId = id;
    } catch {
      // pf missing or daemon down: run without a chat id
    }
  });

  pi.on("tool_call", async (event) => {
    if (chatId && isToolCallEventType("bash", event)) {
      event.input.command = `PF_CHAT_ID=${chatId} ${event.input.command}`;
    }
  });
}
