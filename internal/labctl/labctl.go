// Package labctl is the command-line client for the clusterd API.
package labctl

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/jurrebuunk/clusterd/internal/client"
)

// Exit codes, as documented in the handbook's CLI reference.
const (
	ExitOK          = 0
	ExitAPI         = 1
	ExitUsage       = 2
	ExitUnreachable = 3
	ExitAuth        = 4
	ExitUnhealthy   = 5 // labctl health: not every check is ok
)

type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }

func usageErr(format string, a ...any) error {
	return &exitError{ExitUsage, fmt.Errorf(format, a...)}
}

func authErr(format string, a ...any) error {
	return &exitError{ExitAuth, fmt.Errorf(format, a...)}
}

// env is what every command gets.
type env struct {
	ctx    context.Context
	cfg    *Config
	site   string
	json   bool
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
	getenv func(string) string
}

type command struct {
	name, args, help string
	run              func(e *env, args []string) error
}

func commands() []command {
	return []command{
		{"login", "[--api URL] [--token-file F]", "Store and check a token for the site", cmdLogin},
		{"use", "[site]", "Switch the current site, or list sites", cmdUse},
		{"whoami", "", "Show the site, token name and scope", cmdWhoami},
		{"status", "[host|vm|node|ceph|vip|disk|s3]", "Status of everything, or one part", cmdStatus},
		{"metrics", "query|range <promql> [flags]", "Query metrics (PromQL / MetricsQL)", cmdMetrics},
		{"logs", "[--host H] [--unit U] [--since D] [--limit N] [--follow] [query]", "Search or follow logs (LogsQL)", cmdLogs},
		{"health", "", "One verdict per area; exit code 5 unless everything is ok", cmdHealth},
		{"vm", "restart|stop|start <vm> [--yes] [--dry-run] | roll [vm...]", "Restart, stop or start VMs safely (drain first, uncordon after)", cmdVM},
		{"node", "cordon|uncordon <node>", "Mark a node (un)schedulable", cmdNode},
		{"host", "maintenance|resume <host> [--reboot] [--yes] [--dry-run]", "Take a host out for maintenance and bring it back", cmdHost},
		{"token", "new --name N --scope read|operator|manage [--out F]", "Generate a token and its hash line (local)", cmdToken},
		{"version", "", "Show labctl and server versions", cmdVersion},
		{"completion", "bash|zsh|fish", "Print a shell completion script", cmdCompletion},
	}
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "labctl controls the platform through the clusterd API.")
	fmt.Fprintln(w, "\nUsage:\n  labctl [--site S] [--json] <command> [args]\n\nCommands:")
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	for _, c := range commands() {
		fmt.Fprintf(tw, "  %s %s\t%s\n", c.name, c.args, c.help)
	}
	tw.Flush()
	fmt.Fprintln(w, "\nEnvironment: LABCTL_API, LABCTL_TOKEN, LABCTL_SITE.")
	fmt.Fprintln(w, "Exit codes: 0 ok, 1 API error, 2 usage, 3 unreachable, 4 token missing or not allowed, 5 health not ok.")
}

// Main runs labctl and returns the exit code.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	e := &env{ctx: ctx, stdin: stdin, stdout: stdout, stderr: stderr, getenv: os.Getenv}

	// Global flags may appear anywhere before "--".
	var rest []string
	siteFlag := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			rest = append(rest, args[i:]...)
			i = len(args)
		case a == "--json":
			e.json = true
		case a == "--site":
			if i+1 >= len(args) {
				fmt.Fprintln(stderr, "error: --site needs a value")
				return ExitUsage
			}
			siteFlag = args[i+1]
			i++
		case strings.HasPrefix(a, "--site="):
			siteFlag = strings.TrimPrefix(a, "--site=")
		default:
			rest = append(rest, a)
		}
	}
	if len(rest) == 0 || rest[0] == "help" || rest[0] == "-h" || rest[0] == "--help" {
		usage(stdout)
		if len(rest) == 0 {
			return ExitUsage
		}
		return ExitOK
	}
	if rest[0] == "--version" {
		rest[0] = "version"
	}

	if rest[0] == "completion" {
		return exitCode(stderr, cmdCompletion(e, rest[1:]))
	}
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return ExitUsage
	}
	e.cfg = cfg
	e.site = cfg.Current
	if s := e.getenv("LABCTL_SITE"); s != "" {
		e.site = s
	}
	if siteFlag != "" {
		e.site = siteFlag
	}

	if rest[0] == "__complete" {
		cmdComplete(e, rest[1:]) // for the completion scripts; never fails loudly
		return ExitOK
	}
	for _, c := range commands() {
		if c.name == rest[0] {
			return exitCode(stderr, c.run(e, rest[1:]))
		}
	}
	fmt.Fprintf(stderr, "error: unknown command %q (see labctl help)\n", rest[0])
	return ExitUsage
}

func exitCode(stderr io.Writer, err error) int {
	if err == nil {
		return ExitOK
	}
	if errors.Is(err, flag.ErrHelp) {
		return ExitOK
	}
	if errors.Is(err, context.Canceled) {
		return ExitOK // Ctrl-C while following
	}
	fmt.Fprintln(stderr, "error:", err)
	var ee *exitError
	var ae *client.APIError
	var ue *client.UnreachableError
	switch {
	case errors.As(err, &ee):
		return ee.code
	case errors.As(err, &ue):
		fmt.Fprintln(stderr, "hint: is WARP connected? (or set LABCTL_API)")
		return ExitUnreachable
	case errors.As(err, &ae):
		if ae.Unauthorized() {
			return ExitAuth
		}
		return ExitAPI
	}
	return ExitAPI
}

// apiURL is the API base for the current site.
func (e *env) apiURL() (string, error) {
	if u := e.getenv("LABCTL_API"); u != "" {
		return u, nil
	}
	s, ok := e.cfg.Sites[e.site]
	if !ok {
		return "", usageErr("unknown site %q (known: %s); add it with labctl login --site %s --api URL",
			e.site, strings.Join(e.cfg.siteNames(), ", "), e.site)
	}
	return s.API, nil
}

// client builds an API client with the site's token.
func (e *env) client() (*client.Client, error) {
	api, err := e.apiURL()
	if err != nil {
		return nil, err
	}
	token := e.getenv("LABCTL_TOKEN")
	if token == "" {
		path, err := e.cfg.tokenFile(e.site)
		if err != nil {
			return nil, err
		}
		token, err = readToken(path)
		if errors.Is(err, os.ErrNotExist) || (err == nil && token == "") {
			return nil, authErr("no token for site %s: run labctl login", e.site)
		} else if err != nil {
			return nil, err
		}
	}
	return client.New(api, token)
}

// parseArgs parses flags and positional arguments in any order.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, &exitError{ExitUsage, err}
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func newFlags(e *env, name string) *flag.FlagSet {
	fs := flag.NewFlagSet("labctl "+name, flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	return fs
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
