package labctl

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/jurrebuunk/clusterd/internal/api"
	"github.com/jurrebuunk/clusterd/internal/auth"
	"github.com/jurrebuunk/clusterd/internal/client"
	"github.com/jurrebuunk/clusterd/internal/version"
)

func cmdAuth(e *env) *cobra.Command {
	c := group("auth", "Log in, log out, see the token in use, make new tokens")
	var apiFlag, tokenFile string
	login := &cobra.Command{
		Use: "login", Short: "Store and check a token for the site (--site)", Args: noArgs,
		RunE: func(*cobra.Command, []string) error { return e.login(apiFlag, tokenFile) },
	}
	login.Flags().StringVar(&apiFlag, "api", "", "API URL for this site (default: the site's current one)")
	login.Flags().StringVar(&tokenFile, "token-file", "", "read the token from this file instead of asking")
	logout := &cobra.Command{
		Use: "logout", Short: "Forget the site's token", Args: noArgs,
		RunE: func(*cobra.Command, []string) error {
			path, err := e.cfg.tokenFile(e.site)
			if err != nil {
				return err
			}
			if err := os.Remove(path); errors.Is(err, os.ErrNotExist) {
				return usageErr("no token stored for site %s", e.site)
			} else if err != nil {
				return err
			}
			fmt.Fprintf(e.stdout, "Logged out of %s.\n", e.site)
			return nil
		},
	}
	status := &cobra.Command{
		Use: "status", Short: "The site, token name and scope, and which server answered", Args: noArgs,
		RunE: func(*cobra.Command, []string) error { return e.whoami() },
	}
	token := group("token", "API tokens")
	token.AddCommand(cmdTokenCreate(e))
	c.AddCommand(login, logout, status, token)
	return c
}

func (e *env) login(apiFlag, tokenFile string) error {
	api := apiFlag
	if api == "" {
		if s, ok := e.cfg.Sites[e.site]; ok {
			api = s.API
		} else {
			return usageErr("site %q is new: give its API with --api URL", e.site)
		}
	}

	var token string
	var err error
	switch {
	case tokenFile != "":
		token, err = readToken(tokenFile)
	case isTerminal(e.stdin):
		fmt.Fprintf(e.stderr, "Token for %s (%s): ", e.site, api)
		var b []byte
		b, err = term.ReadPassword(int(e.stdin.(*os.File).Fd()))
		fmt.Fprintln(e.stderr)
		token = strings.TrimSpace(string(b))
	default:
		token, err = bufio.NewReader(e.stdin).ReadString('\n')
		if errors.Is(err, io.EOF) {
			err = nil
		}
		token = strings.TrimSpace(token)
	}
	if err != nil {
		return err
	}
	if token == "" {
		return usageErr("empty token")
	}

	c, err := client.New(api, token)
	if err != nil {
		return usageErr("%v", err)
	}
	who, err := c.Whoami(e.ctx)
	if err != nil {
		return err
	}

	site := e.cfg.Sites[e.site]
	site.API = api
	e.cfg.Sites[e.site] = site
	if _, ok := e.cfg.Sites[e.cfg.Current]; !ok || len(e.cfg.Sites) == 1 {
		e.cfg.Current = e.site
	}
	path, err := e.cfg.tokenFile(e.site)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(path, []byte(token+"\n"), 0o600); err != nil {
		return err
	}
	if err := e.cfg.save(); err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "Logged in to %s as %s (%s).\n", e.site, who.Name, who.Scope)
	return nil
}

func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

func cmdSite(e *env) *cobra.Command {
	c := group("site", "Sites labctl knows, and which one is current")
	c.AddCommand(&cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "Known sites; * marks the current one", Args: noArgs,
		RunE: func(*cobra.Command, []string) error {
			names := e.cfg.siteNames()
			return e.render(e.cfg, names, func() error {
				tw := newTable(e.stdout)
				fmt.Fprintln(tw, "CURRENT\tNAME\tAPI")
				for _, n := range names {
					mark := ""
					if n == e.cfg.Current {
						mark = "*"
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\n", mark, n, e.cfg.Sites[n].API)
				}
				return tw.Flush()
			})
		},
	})
	c.AddCommand(&cobra.Command{
		Use: "use SITE", Short: "Make SITE the current site", Args: args(1, "SITE"),
		ValidArgsFunction: func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
			if e.load() != nil {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			return e.cfg.siteNames(), cobra.ShellCompDirectiveNoFileComp
		},
		RunE: func(_ *cobra.Command, a []string) error {
			if _, ok := e.cfg.Sites[a[0]]; !ok {
				return usageErr("unknown site %q; add it with labctl auth login --site %s --api URL", a[0], a[0])
			}
			e.cfg.Current = a[0]
			if err := e.cfg.save(); err != nil {
				return err
			}
			fmt.Fprintf(e.stdout, "Switched to site %q.\n", a[0])
			return nil
		},
	})
	return c
}

func (e *env) whoami() error {
	c, err := e.client()
	if err != nil {
		return err
	}
	w, err := c.Whoami(e.ctx)
	if err != nil {
		return err
	}
	if e.json() {
		return e.printJSON(w)
	}
	tw := newTable(e.stdout)
	fmt.Fprintln(tw, "NAME\tSCOPE\tSITE\tSERVER")
	fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", w.Name, w.Scope, w.Site, w.Host)
	return tw.Flush()
}

func cmdVersion(e *env) *cobra.Command {
	return &cobra.Command{
		Use: "version", Short: "Show labctl's and the server's version", Args: noArgs,
		RunE: func(*cobra.Command, []string) error { return e.version() },
	}
}

func (e *env) version() error {
	out := map[string]string{"labctl": version.Version, "commit": version.Commit}
	c, err := e.client()
	if err == nil {
		var w api.Whoami
		if w, err = c.Whoami(e.ctx); err == nil {
			out["clusterd"] = w.Version
			out["served_by"] = w.Host
		}
	}
	if e.json() {
		return e.printJSON(out)
	}
	fmt.Fprintf(e.stdout, "Client Version: %s\n", version.Version)
	if v, ok := out["clusterd"]; ok {
		fmt.Fprintf(e.stdout, "Server Version: %s\n", v)
	} else if err != nil {
		fmt.Fprintf(e.stderr, "warning: no server version: %v\n", err)
	}
	return nil
}

func cmdTokenCreate(e *env) *cobra.Command {
	var name, scope, out string
	c := &cobra.Command{
		Use: "create", Short: "Generate a token and the line for the hosts' token file (offline)", Args: noArgs,
		RunE: func(*cobra.Command, []string) error { return e.tokenCreate(name, scope, out) },
	}
	c.Flags().StringVar(&name, "name", "", "token name, shown in logs (e.g. laptop-jurre)")
	c.Flags().StringVar(&scope, "scope", "", "read, operator or manage")
	c.Flags().StringVar(&out, "out", "", "write the token to this file (mode 600) instead of printing it")
	c.RegisterFlagCompletionFunc("scope", fixed("read", "operator", "manage"))
	return c
}

func (e *env) tokenCreate(name, scopeFlag, outFile string) error {
	out := &outFile
	if !auth.ValidName(name) {
		return usageErr("--name must be lowercase letters, digits, '.', '_' or '-'")
	}
	scope, err := auth.ParseScope(scopeFlag)
	if err != nil {
		return usageErr("--scope: %v", err)
	}
	tok, err := auth.Generate()
	if err != nil {
		return err
	}
	line := auth.HashLine(name, scope, tok)
	if *out != "" {
		f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		if _, err := f.WriteString(tok + "\n"); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		fmt.Fprintf(e.stderr, "Token written to %s; add this line to the hosts' token file:\n", *out)
		fmt.Fprintln(e.stdout, line)
		return nil
	}
	fmt.Fprintln(e.stdout, tok)
	fmt.Fprintln(e.stdout, line)
	fmt.Fprintln(e.stderr, "Line 1 is the token (shown once), line 2 goes in the hosts' token file.")
	return nil
}
