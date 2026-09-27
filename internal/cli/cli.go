package cli

import (
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
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

// Command is one `pf <name>` subcommand.
type Command struct {
	// Name is the subcommand word (one of the 12 routing verbs or a
	// management verb).
	Name string
	// Summary is one line for the usage listing.
	Summary string
	// Usage shows the full invocation grammar (printed by `pf help <name>`).
	Usage string
	// Setup registers the command's flags on a fresh FlagSet.
	Setup func(fs *flag.FlagSet)
	// Run executes; args are the positional arguments left after flags.
	Run func(ctx *Context, fs *flag.FlagSet, args []string) error
}

// commands is the full registry, in routing-table order (00 §5.1) with
// the management verbs after the 12.
var commands = []*Command{
	cmdFile,
	cmdQueue,
	cmdCancel,
	cmdRecall,
	cmdSubmit,
	cmdObserve,
	cmdAnswer,
	cmdBatchConfirm,
	cmdPause,
	cmdResume,
	cmdWrap,
	cmdOps,
	// Supporting faces (also `pf` CLI callers per 02 §5.1 rows 11/12/26)
	// plus the row-21 machine face (the operator's registration duty).
	cmdDeps,
	cmdBudget,
	cmdMachines,
	// Management face M1–M5 (02 §5.1 F group).
	cmdUsers,
	cmdApiKeys,
	cmdProjects,
	cmdBackup,
	cmdRotateSecret,
}

// Run executes the CLI with argv (without the program name) and returns
// the process exit code: 0 success, 1 runtime error, 2 usage error.
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
		// `pf help` and `pf help <name>` print to stdout and exit 0
		// (human-facing reference output belongs on stdout).
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
		return 1
	}
	return 0
}

// parseCommandArgs parses the command tail. For commands that registered
// flags in Setup, flags may appear anywhere (before, between or after
// positionals) — humans write `pf observe wi_a --after 5` as naturally as
// `pf observe --after 5 wi_a`. For flag-less commands (multi-action verbs
// whose sub-actions own the flags) parsing stops at the first positional
// and the rest is handed over verbatim for the sub-action to parse.
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

// fsHasFlags reports whether the FlagSet has any registered flag.
func fsHasFlags(fs *flag.FlagSet) bool {
	has := false
	fs.VisitAll(func(*flag.Flag) { has = true })
	return has
}

// renderError prints an error with the typed code prominent; --json mode
// emits the raw wire envelope when we have it.
func renderError(ctx *Context, err error) {
	if ctx.JSON {
		var ae *APIError
		if ok := asAPIError(err, &ae); ok && len(ae.Raw) > 0 {
			fmt.Fprintln(ctx.Stderr, string(ae.Raw))
			return
		}
	}
	fmt.Fprintf(ctx.Stderr, "pf: %v\n", err)
}

// asAPIError unwraps an *APIError.
func asAPIError(err error, target **APIError) bool {
	if ae, ok := err.(*APIError); ok {
		*target = ae
		return true
	}
	return false
}

func lookupCommand(name string) *Command {
	for _, c := range commands {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// printUsage lists all commands grouped by target face.
func printUsage(w io.Writer) {
	fmt.Fprintln(w, "pf — Polyforge client CLI (vNext)")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Data-plane verbs (direct to AIHub, 00 §5.1):")
	for _, c := range commands[:4] {
		fmt.Fprintf(w, "  %-14s %s\n", c.Name, c.Summary)
	}
	fmt.Fprintln(w, "Execution-plane verbs (local daemon face, 00 §5.1):")
	for _, c := range commands[4:12] {
		fmt.Fprintf(w, "  %-14s %s\n", c.Name, c.Summary)
	}
	fmt.Fprintln(w, "Supporting faces (rows 11/12/26) + machines (row 21):")
	for _, c := range commands[12:15] {
		fmt.Fprintf(w, "  %-14s %s\n", c.Name, c.Summary)
	}
	fmt.Fprintln(w, "Management face (M1–M5):")
	for _, c := range commands[15:] {
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
// PATCH semantics need "flag absent" (send nothing) distinct from "flag
// set to false". IsBoolFlag makes the bare `--flag` form work like the
// built-in bool flags.
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

// kvList collects repeatable key=value flags in declaration order (repo
// inventory position matters, 02 §4.6).
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
