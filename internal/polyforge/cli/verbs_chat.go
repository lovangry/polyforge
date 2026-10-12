package cli

import (
	"encoding/json"
	"flag"
	"fmt"
)

// chat — the harness-chat identity face (v2 identity model). A chat is
// one interactive harness conversation; the daemon maps
// (harness, harness-session) to a stable chat id, so a resumed
// conversation keeps its chat. The id is a notification label, never a
// credential.
var cmdChat = &Command{
	Name:    "chat",
	Group:   GroupExec,
	Summary: "Harness chat identity: get the chat id for a harness session",
	Usage:   "pf chat init --harness claude|pi --harness-session ID [--json]",
	Setup:   func(fs *flag.FlagSet) {},
	Run: func(ctx *Context, fs *flag.FlagSet, args []string) error {
		if len(args) == 0 || args[0] != "init" {
			return fmt.Errorf("chat: expected init")
		}
		f := subFlags(ctx, "chat init")
		harness := f.String("harness", "", "harness: claude|pi")
		session := f.String("harness-session", "", "the harness's own session id")
		rest, err := parseCommandArgs(f, args[1:])
		if err != nil {
			return err
		}
		if len(rest) != 0 {
			return fmt.Errorf("chat init: unexpected arguments %v", rest)
		}
		if *harness != "claude" && *harness != "pi" {
			return fmt.Errorf("chat init: --harness must be claude|pi")
		}
		if *session == "" {
			return fmt.Errorf("chat init: --harness-session is required")
		}
		raw, err := ctx.daemon().ChatInit(*harness, *session)
		if err != nil {
			return err
		}
		ctx.finish(raw, func() {
			var rec ChatRecord
			if err := json.Unmarshal(raw, &rec); err == nil && rec.ChatID != "" {
				fmt.Fprintln(ctx.Stdout, rec.ChatID)
			} else {
				printJSON(ctx.Stdout, raw)
			}
		})
		return nil
	},
}
