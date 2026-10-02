package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/polyforge/aihubclient"
)

// Context carries the resolved configuration and I/O for one command
// invocation.
type Context struct {
	Cfg    Config
	JSON   bool // --json: machine output, server JSON as-is
	Stdout io.Writer
	Stderr io.Writer
	Stdin  io.Reader
}

// aihub returns the direct-aihub client for the resolved config.
func (ctx *Context) aihub() *aihubclient.Client {
	return aihubclient.New(ctx.Cfg.Server, ctx.Cfg.APIKey)
}

// daemon returns the daemon-socket client for the resolved config.
func (ctx *Context) daemon() *DaemonClient {
	return NewDaemonClient(ctx.Cfg.DaemonSocket)
}

// Command is one `pf <name>` subcommand.
type Command struct {
	Name    string
	Summary string
	Usage   string
	Setup   func(fs *flag.FlagSet)
	Run     func(ctx *Context, fs *flag.FlagSet, args []string) error
}

// commands is the full registry: the 16 routing verbs first — 8 direct
// to aihub (file, queue, cancel, recall, remember + the v1.5 takeover,
// unblock and verify-session) + 8 on the daemon UDS face (submit, observe,
// answer, batch-confirm, pause, resume, wrap, ops) — then ship (the wrap→git
// delivery bridge, daemon face), then the management face M1–M5, then
// the two migrations, then the daemon placeholder (help-only: cmd/pf/
// main.go intercepts the real launch).
var commands = []*Command{
	cmdFile,
	cmdQueue,
	cmdCancel,
	cmdRecall,
	cmdRemember,
	cmdTakeover,
	cmdUnblock,
	cmdVerifySession,
	cmdSubmit,
	cmdObserve,
	cmdAnswer,
	cmdBatchConfirm,
	cmdPause,
	cmdResume,
	cmdWrap,
	cmdOps,
	cmdShip,
	cmdUsers,
	cmdApiKeys,
	cmdMachines,
	cmdProjects,
	cmdBackup,
	cmdRotateSecret,
	cmdMigrateMemories,
	cmdMigrateWorkItems,
	cmdDaemon,
}

// Run executes the CLI with argv (without the program name) and returns
// the process exit code:
//
//	0 success
//	1 client-class failure (usage of the verb, local validation, 4xx
//	  wire answers: not-found, conflict, forbidden, bad input)
//	2 server-class failure (5xx wire answers) and usage/parse errors
func Run(argv []string, stdin io.Reader, stdout, stderr io.Writer) int {
	gfs := flag.NewFlagSet("pf", flag.ContinueOnError)
	gfs.SetOutput(stderr)
	gfs.Usage = func() { printUsage(stderr) }
	jsonFlag := gfs.Bool("json", false, "machine-readable output (server JSON as-is)")
	cfgFlag := gfs.String("config", "", "config file (default ~/.config/polyforge/cli.toml)")
	serverFlag := gfs.String("server", "", "aihub base URL (env PF_SERVER)")
	apiKeyFlag := gfs.String("api-key", "", "aihub api key (env PF_API_KEY)")
	socketFlag := gfs.String("socket", "", "daemon unix socket (env PF_DAEMON_SOCKET)")
	userFlag := gfs.String("user", "", "caller user id, e.g. for answer.answered_by (env PF_USER_ID)")

	if err := gfs.Parse(argv); err != nil {
		return 2
	}
	args := gfs.Args()
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}
	if args[0] == "help" {
		if len(args) == 2 {
			return printCommandUsage(stdout, args[1])
		}
		printUsage(stdout)
		return 0
	}

	cmd := lookupCommand(args[0])
	if cmd == nil {
		fmt.Fprintf(stderr, "pf: unknown command %q (see `pf help`)\n", args[0])
		return 2
	}

	cfg, err := LoadConfig(*cfgFlag)
	if err != nil {
		fmt.Fprintf(stderr, "pf: %v\n", err)
		return 1
	}
	// Flags override environment, environment overrides the file.
	if *serverFlag != "" {
		cfg.Server = *serverFlag
	}
	if *apiKeyFlag != "" {
		cfg.APIKey = *apiKeyFlag
	}
	if *socketFlag != "" {
		cfg.DaemonSocket = *socketFlag
	}
	if *userFlag != "" {
		cfg.UserID = *userFlag
	}

	ctx := &Context{
		Cfg:    cfg,
		JSON:   *jsonFlag,
		Stdout: stdout,
		Stderr: stderr,
		Stdin:  stdin,
	}

	fs := flag.NewFlagSet("pf "+cmd.Name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cmd.Setup(fs)
	pos, perr := parseCommandArgs(fs, args[1:])
	if perr != nil {
		return 2
	}
	if err := cmd.Run(ctx, fs, pos); err != nil {
		renderError(ctx, err)
		return exitCode(err)
	}
	return 0
}

// exitCode maps an error to the process exit code from the typed wire
// status: 4xx → 1, 5xx → 2. Transport failures and local validation
// carry no code and stay 1.
func exitCode(err error) int {
	var ae *api.Error
	if errors.As(err, &ae) && ae.HTTPStatus() >= 500 {
		return 2
	}
	return 1
}

// renderError prints an error with the typed code prominent; --json mode
// emits the typed envelope on stderr INSTEAD of the human line (v0
// semantics: machine consumers get one JSON document, not both).
func renderError(ctx *Context, err error) {
	var ae *api.Error
	if errors.As(err, &ae) {
		if ctx.JSON {
			if b, merr := json.Marshal(ae); merr == nil {
				fmt.Fprintln(ctx.Stderr, string(b))
				return
			}
		}
		fmt.Fprintf(ctx.Stderr, "pf: %s\n", ae.Error())
		return
	}
	fmt.Fprintf(ctx.Stderr, "pf: %v\n", err)
}

func lookupCommand(name string) *Command {
	for _, c := range commands {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// parseCommandArgs parses the command tail. For commands that registered
// flags in Setup, flags may appear anywhere (before, between or after
// positionals) — humans write `pf observe wi_a --after 5` as naturally
// as `pf observe --after 5 wi_a`. For flag-less commands parsing stops
// at the first positional and the rest is handed over verbatim for the
// sub-action to parse.
func parseCommandArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	if !fsHasFlags(fs) {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		return fs.Args(), nil
	}
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		switch {
		case len(rest) == 0:
			return pos, nil
		case len(rest) == len(args):
			// No flag was consumed: rest[0] is a positional.
			pos = append(pos, rest[0])
			args = rest[1:]
		default:
			// Flags were consumed; continue after them.
			args = rest
		}
	}
}

func fsHasFlags(fs *flag.FlagSet) bool {
	has := false
	fs.VisitAll(func(*flag.Flag) { has = true })
	return has
}

// printUsage lists all commands grouped by target face.
func printUsage(w io.Writer) {
	fmt.Fprintln(w, "pf — polyforge v1 client CLI")
	fmt.Fprintln(w)
	// The registry layout: 8 data-plane verbs (direct to aihub) — file,
	// queue, cancel, recall, remember + the v1.5 takeover, unblock and
	// verify-session — then the 8 execution-plane verbs, ship, the M1–M5
	// management face, the two migrations and the daemon placeholder. The
	// group indices below MUST track that layout.
	fmt.Fprintln(w, "Data-plane verbs (direct to aihub):")
	for _, c := range commands[0:8] {
		fmt.Fprintf(w, "  %-14s %s\n", c.Name, c.Summary)
	}
	fmt.Fprintln(w, "Execution-plane verbs (local daemon UDS face):")
	for _, c := range commands[8:16] {
		fmt.Fprintf(w, "  %-14s %s\n", c.Name, c.Summary)
	}
	fmt.Fprintln(w, "Delivery bridge (wrap→git, local daemon UDS face):")
	for _, c := range commands[16:17] {
		fmt.Fprintf(w, "  %-14s %s\n", c.Name, c.Summary)
	}
	fmt.Fprintln(w, "Management face (M1–M5):")
	for _, c := range commands[17:22] {
		fmt.Fprintf(w, "  %-14s %s\n", c.Name, c.Summary)
	}
	fmt.Fprintln(w, "Migration (admin; one-shot v0→v1 cutover):")
	for _, c := range commands[22:24] {
		fmt.Fprintf(w, "  %-14s %s\n", c.Name, c.Summary)
	}
	fmt.Fprintln(w, "Process:")
	for _, c := range commands[24:] {
		fmt.Fprintf(w, "  %-14s %s\n", c.Name, c.Summary)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Global flags (before the command):")
	fmt.Fprintln(w, "  --json          machine-readable output (server JSON as-is)")
	fmt.Fprintln(w, "  --config PATH   config file (default ~/.config/polyforge/cli.toml)")
	fmt.Fprintln(w, "  --server URL    aihub base URL (env PF_SERVER)")
	fmt.Fprintln(w, "  --api-key KEY   aihub api key (env PF_API_KEY)")
	fmt.Fprintln(w, "  --socket PATH   daemon unix socket (env PF_DAEMON_SOCKET)")
	fmt.Fprintln(w, "  --user ID       caller identity (env PF_USER_ID)")
}

// printCommandUsage prints one command's usage.
func printCommandUsage(w io.Writer, name string) int {
	cmd := lookupCommand(name)
	if cmd == nil {
		fmt.Fprintf(w, "pf: unknown command %q\n", name)
		return 2
	}
	fmt.Fprintf(w, "usage: %s\n\n%s", cmd.Usage, cmd.Summary)
	fmt.Fprintln(w)
	return 0
}

// ---------------------------------------------------------------------------
// Flag value helpers.
// ---------------------------------------------------------------------------

// subFlags builds the flag set of one sub-action, wired to the CLI's
// stderr (the flag package writes its usage errors there).
func subFlags(ctx *Context, name string) *flag.FlagSet {
	f := flag.NewFlagSet("pf "+name, flag.ContinueOnError)
	f.SetOutput(ctx.Stderr)
	return f
}

// optString is a flag.Value that records whether it was explicitly set,
// distinguishing "flag absent" from "flag set to empty string" (PATCH
// semantics need both).
type optString struct {
	val string
	set bool
}

func (o *optString) String() string { return o.val }
func (o *optString) Set(s string) error {
	o.val, o.set = s, true
	return nil
}
func (o *optString) IsSet() bool { return o.set }
func (o *optString) Val() string { return o.val }
func (o *optString) Ptr() *string {
	if !o.set {
		return nil
	}
	v := o.val
	return &v
}

// optBool is a flag.Value that records whether it was explicitly set —
// PATCH semantics need "flag absent" distinct from "flag set to false".
type optBool struct {
	val bool
	set bool
}

func (o *optBool) String() string { return fmt.Sprintf("%v", o.val) }
func (o *optBool) Set(s string) error {
	v, err := strconv.ParseBool(s)
	if err != nil {
		return err
	}
	o.val, o.set = v, true
	return nil
}
func (o *optBool) IsBoolFlag() bool { return true }
func (o *optBool) IsSet() bool      { return o.set }
func (o *optBool) Ptr() *bool {
	if !o.set {
		return nil
	}
	v := o.val
	return &v
}

// flagChanged reports whether the named flag was explicitly set on the
// command line (PATCH semantics: absent must be distinguishable from a
// set-to-zero-value spelling).
func flagChanged(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// parseRepoRefs parses the -repos spelling "name=url[,name=url...]" into
// the wire's full-replacement inventory list. An EMPTY string answers an
// empty non-nil list (the explicit clear); a malformed entry is an error,
// never a silent drop.
func parseRepoRefs(s string) ([]aihubclient.RepoRef, error) {
	out := []aihubclient.RepoRef{}
	s = strings.TrimSpace(s)
	if s == "" {
		return out, nil
	}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		name, url, ok := strings.Cut(pair, "=")
		name, url = strings.TrimSpace(name), strings.TrimSpace(url)
		if !ok || name == "" || url == "" {
			return nil, fmt.Errorf("-repos entry %q must be name=url", pair)
		}
		out = append(out, aihubclient.RepoRef{Name: name, URL: url})
	}
	return out, nil
}

// stringList collects a repeatable string flag (--label a --label b).
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }
func (l *stringList) Set(s string) error {
	*l = append(*l, s)
	return nil
}

// kvPair is one key=value flag occurrence, order-preserving.
type kvPair struct {
	Key   string
	Value string
}

// kvList collects repeatable key=value flags in declaration order.
type kvList []kvPair

func (l *kvList) String() string {
	parts := make([]string, len(*l))
	for i, kv := range *l {
		parts[i] = kv.Key + "=" + kv.Value
	}
	return strings.Join(parts, ",")
}
func (l *kvList) Set(s string) error {
	eq := strings.IndexByte(s, '=')
	if eq <= 0 {
		return fmt.Errorf("expected key=value, got %q", s)
	}
	*l = append(*l, kvPair{Key: s[:eq], Value: s[eq+1:]})
	return nil
}

// ---------------------------------------------------------------------------
// Flag accessors for flags registered in Setup.
// ---------------------------------------------------------------------------

func boolFlag(fs *flag.FlagSet, name string) bool {
	return fs.Lookup(name).Value.(flag.Getter).Get().(bool)
}
func stringFlag(fs *flag.FlagSet, name string) string {
	return fs.Lookup(name).Value.(flag.Getter).Get().(string)
}
func int64Flag(fs *flag.FlagSet, name string) int64 {
	return fs.Lookup(name).Value.(flag.Getter).Get().(int64)
}
func intFlag(fs *flag.FlagSet, name string) int {
	return fs.Lookup(name).Value.(flag.Getter).Get().(int)
}

// strPtr wraps a plain string in a pointer.
func strPtr(s string) *string { return &s }
