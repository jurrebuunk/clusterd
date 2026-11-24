// Package labctl is the command-line client for the clusterd API.
//
// Commands are nouns with verbs (labctl vm stop, labctl s3 bucket list), as
// in gh and docker; a few site-wide ones (status, health, logs, metrics)
// stand on their own. Every command takes the global flags anywhere on the
// line and answers --help.
package labctl

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/jurrebuunk/clusterd/internal/client"
	"github.com/jurrebuunk/clusterd/internal/version"
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

// Output formats for -o.
const (
	outTable = "table"
	outJSON  = "json"
	outName  = "name"
)

// env is what every command gets.
type env struct {
	ctx      context.Context
	cfg      *Config
	site     string
	siteFlag string
	output   string
	stdin    io.Reader
	stdout   io.Writer
	stderr   io.Writer
	getenv   func(string) string
}

func (e *env) json() bool { return e.output == outJSON }

// Main runs labctl and returns the exit code.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	e := &env{ctx: ctx, stdin: stdin, stdout: stdout, stderr: stderr, getenv: os.Getenv}
	root := newRoot(e)
	root.SetArgs(args)
	root.SetIn(stdin)
	root.SetOut(stdout)
	root.SetErr(stderr)
	return exitCode(stderr, root.ExecuteContext(ctx))
}

func newRoot(e *env) *cobra.Command {
	root := &cobra.Command{
		Use:   "labctl",
		Short: "labctl controls the platform through the clusterd API",
		Long: `labctl controls the platform through the clusterd API.

Environment: LABCTL_API, LABCTL_TOKEN, LABCTL_SITE.
Exit codes: 0 ok, 1 API error, 2 usage, 3 unreachable, 4 token missing or
not allowed, 5 health not ok.`,
		SilenceErrors:     true,
		SilenceUsage:      true,
		Version:           version.Version,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error { return e.load() },
	}
	root.SetVersionTemplate("{{.Version}}\n")
	root.PersistentFlags().StringVar(&e.siteFlag, "site", "", "site to talk to (default: the current one)")
	root.PersistentFlags().StringVarP(&e.output, "output", "o", outTable, "output format: table, json or name")
	root.RegisterFlagCompletionFunc("site", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		if e.load() != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return e.cfg.siteNames(), cobra.ShellCompDirectiveNoFileComp
	})
	root.RegisterFlagCompletionFunc("output", fixed(outTable, outJSON, outName))
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return &exitError{ExitUsage, err} })
	root.CompletionOptions.HiddenDefaultCmd = false

	root.AddGroup(&cobra.Group{ID: "site", Title: "The site:"},
		&cobra.Group{ID: "res", Title: "Resources:"},
		&cobra.Group{ID: "cfg", Title: "Access and configuration:"})
	for _, c := range []*cobra.Command{cmdStatus(e), cmdHealth(e), cmdLogs(e), cmdMetrics(e), cmdOperation(e)} {
		c.GroupID = "site"
		root.AddCommand(c)
	}
	for _, c := range []*cobra.Command{cmdHost(e), cmdVM(e), cmdNode(e), cmdPod(e), cmdCeph(e), cmdOSD(e), cmdDisk(e),
		cmdFilesystem(e), cmdVIP(e), cmdS3(e)} {
		c.GroupID = "res"
		root.AddCommand(c)
	}
	for _, c := range []*cobra.Command{cmdAuth(e), cmdSite(e), cmdVersion(e)} {
		c.GroupID = "cfg"
		root.AddCommand(c)
	}
	root.SetHelpCommandGroupID("cfg")
	root.SetCompletionCommandGroupID("cfg")
	return root
}

// load reads the config once and settles the site.
func (e *env) load() error {
	if e.cfg != nil {
		return nil
	}
	switch e.output {
	case outTable, outJSON, outName:
	default:
		return usageErr("-o must be table, json or name, not %q", e.output)
	}
	cfg, err := loadConfig()
	if err != nil {
		return &exitError{ExitUsage, err}
	}
	e.cfg = cfg
	e.site = cfg.Current
	if s := e.getenv("LABCTL_SITE"); s != "" {
		e.site = s
	}
	if e.siteFlag != "" {
		e.site = e.siteFlag
	}
	return nil
}

// group is a noun with verbs: alone it prints its help; with an unknown
// verb it is a usage error that suggests the closest one.
func group(use, short string, aliases ...string) *cobra.Command {
	return &cobra.Command{
		Use: use, Short: short, Aliases: aliases,
		Args:                       cobra.ArbitraryArgs,
		SuggestionsMinimumDistance: 2,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			msg := fmt.Sprintf("unknown command %q for %q", args[0], cmd.CommandPath())
			if s := cmd.SuggestionsFor(args[0]); len(s) > 0 {
				msg += "; did you mean " + strings.Join(s, " or ") + "?"
			}
			return usageErr("%s", msg)
		},
	}
}

// args checks the number of positional arguments as a usage error.
func args(n int, names string) cobra.PositionalArgs {
	return func(cmd *cobra.Command, a []string) error {
		if len(a) != n {
			return usageErr("usage: %s %s", cmd.CommandPath(), names)
		}
		return nil
	}
}

func noArgs(cmd *cobra.Command, a []string) error {
	if len(a) > 0 {
		return usageErr("%s takes no arguments", cmd.CommandPath())
	}
	return nil
}

func fixed(values ...string) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return values, cobra.ShellCompDirectiveNoFileComp
	}
}

// complete offers the names of a collection (e.g. /v1/vms) for one argument.
func (e *env) complete(path string) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(_ *cobra.Command, a []string, _ string) ([]string, cobra.ShellCompDirective) {
		if len(a) > 0 || e.load() != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		c, err := e.client()
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		var l struct {
			Items []struct {
				Name string `json:"name"`
				ID   string `json:"id"`
			} `json:"items"`
		}
		if c.Get(e.ctx, path, nil, &l) != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		var out []string
		for _, it := range l.Items {
			out = append(out, it.Name+it.ID)
		}
		return out, cobra.ShellCompDirectiveNoFileComp
	}
}

func exitCode(stderr io.Writer, err error) int {
	if err == nil || errors.Is(err, context.Canceled) {
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
	case strings.HasPrefix(err.Error(), "unknown command"), strings.HasPrefix(err.Error(), "unknown flag"):
		return ExitUsage
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
		return "", usageErr("unknown site %q (known: %s); add it with labctl auth login --site %s --api URL",
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
			return nil, authErr("no token for site %s: run labctl auth login", e.site)
		} else if err != nil {
			return nil, err
		}
	}
	return client.New(api, token)
}

// get fetches path into out.
func (e *env) get(path string, out any) error {
	c, err := e.client()
	if err != nil {
		return err
	}
	return c.Get(e.ctx, path, nil, out)
}

func (e *env) printJSON(v any) error {
	enc := json.NewEncoder(e.stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// render prints v as JSON, its names one per line, or the table.
func (e *env) render(v any, names []string, table func() error) error {
	switch e.output {
	case outJSON:
		return e.printJSON(v)
	case outName:
		if names == nil {
			return usageErr("-o name only works for lists and single resources")
		}
		for _, n := range names {
			fmt.Fprintln(e.stdout, n)
		}
		return nil
	}
	return table()
}

// confirm asks a yes/no question on a terminal; without one it needs --yes.
func (e *env) confirm(question, what string, details []string) error {
	if !isTerminal(e.stdin) {
		return usageErr("not a terminal: pass --yes to %s", what)
	}
	for _, s := range details {
		fmt.Fprintln(e.stderr, "  "+s)
	}
	fmt.Fprintf(e.stderr, "%s? [y/N] ", question)
	answer, _ := bufio.NewReader(e.stdin).ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
		return &exitError{ExitAPI, errors.New("aborted")}
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
