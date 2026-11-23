package labctl

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"golang.org/x/term"

	"github.com/jurrebuunk/clusterd/internal/api"
	"github.com/jurrebuunk/clusterd/internal/auth"
	"github.com/jurrebuunk/clusterd/internal/client"
	"github.com/jurrebuunk/clusterd/internal/version"
)

func (e *env) printJSON(v any) error {
	enc := json.NewEncoder(e.stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func cmdLogin(e *env, args []string) error {
	fs := newFlags(e, "login")
	apiFlag := fs.String("api", "", "API URL for this site (default: the site's current one)")
	tokenFile := fs.String("token-file", "", "read the token from this file instead of asking")
	if pos, err := parseArgs(fs, args); err != nil {
		return err
	} else if len(pos) > 0 {
		return usageErr("login takes no arguments; use --site to pick the site")
	}

	api := *apiFlag
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
	case *tokenFile != "":
		token, err = readToken(*tokenFile)
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

func cmdUse(e *env, args []string) error {
	if len(args) > 1 {
		return usageErr("usage: labctl use [site]")
	}
	if len(args) == 0 {
		tw := tabwriter.NewWriter(e.stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "CURRENT\tNAME\tAPI")
		for _, n := range e.cfg.siteNames() {
			mark := ""
			if n == e.cfg.Current {
				mark = "*"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\n", mark, n, e.cfg.Sites[n].API)
		}
		return tw.Flush()
	}
	if _, ok := e.cfg.Sites[args[0]]; !ok {
		return usageErr("unknown site %q; add it with labctl login --site %s --api URL", args[0], args[0])
	}
	e.cfg.Current = args[0]
	if err := e.cfg.save(); err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "Switched to site %q.\n", args[0])
	return nil
}

func cmdWhoami(e *env, args []string) error {
	if len(args) > 0 {
		return usageErr("whoami takes no arguments")
	}
	c, err := e.client()
	if err != nil {
		return err
	}
	w, err := c.Whoami(e.ctx)
	if err != nil {
		return err
	}
	if e.json {
		return e.printJSON(w)
	}
	tw := tabwriter.NewWriter(e.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSCOPE\tSITE\tSERVER")
	fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", w.Name, w.Scope, w.Site, w.Host)
	return tw.Flush()
}

func cmdVersion(e *env, args []string) error {
	out := map[string]string{"labctl": version.Version, "commit": version.Commit}
	c, err := e.client()
	if err == nil {
		var w api.Whoami
		if w, err = c.Whoami(e.ctx); err == nil {
			out["clusterd"] = w.Version
			out["served_by"] = w.Host
		}
	}
	if e.json {
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

func cmdToken(e *env, args []string) error {
	if len(args) == 0 || args[0] != "new" {
		return usageErr("usage: labctl token new --name N --scope read|operator|manage [--out FILE]")
	}
	fs := newFlags(e, "token new")
	name := fs.String("name", "", "token name, shown in logs (e.g. laptop-jurre)")
	scopeFlag := fs.String("scope", "", "read, operator or manage")
	out := fs.String("out", "", "write the token to this file (mode 600) instead of printing it")
	if pos, err := parseArgs(fs, args[1:]); err != nil {
		return err
	} else if len(pos) > 0 {
		return usageErr("unexpected argument %q", pos[0])
	}
	if !auth.ValidName(*name) {
		return usageErr("--name must be lowercase letters, digits, '.', '_' or '-'")
	}
	scope, err := auth.ParseScope(*scopeFlag)
	if err != nil {
		return usageErr("--scope: %v", err)
	}
	tok, err := auth.Generate()
	if err != nil {
		return err
	}
	line := auth.HashLine(*name, scope, tok)
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
