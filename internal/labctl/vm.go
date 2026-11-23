package labctl

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/jurrebuunk/clusterd/internal/api"
)

// actionPlan is the common part of the plans actions return on a dry run.
type actionPlan struct {
	Host       string   `json:"host"`
	Steps      []string `json:"steps"`
	LockHolder string   `json:"lock_holder"`
}

// runAction asks for the plan, shows it, confirms and follows the action.
func (e *env) runAction(path string, q url.Values, yes, dry bool, what string) error {
	c, err := e.client()
	if err != nil {
		return err
	}
	dq := url.Values{"dry_run": {"1"}}
	for k, v := range q {
		dq[k] = v
	}
	body, err := c.Send(e.ctx, http.MethodPost, path, dq)
	if err != nil {
		return err
	}
	raw, err := io.ReadAll(body)
	body.Close()
	if err != nil {
		return err
	}
	var plan actionPlan
	if err := json.Unmarshal(raw, &plan); err != nil {
		return err
	}
	if dry {
		if e.json {
			_, err := fmt.Fprintf(e.stdout, "%s\n", strings.TrimSpace(string(raw)))
			return err
		}
		for _, s := range plan.Steps {
			fmt.Fprintln(e.stdout, s)
		}
		if plan.LockHolder != "" {
			fmt.Fprintf(e.stderr, "warning: busy now: %s\n", plan.LockHolder)
		}
		return nil
	}
	if plan.LockHolder != "" {
		return &exitError{ExitAPI, fmt.Errorf("another action is running: %s", plan.LockHolder)}
	}
	if !yes {
		if !isTerminal(e.stdin) {
			return usageErr("not a terminal: pass --yes to %s", what)
		}
		for _, s := range plan.Steps {
			fmt.Fprintln(e.stderr, "  "+s)
		}
		fmt.Fprintf(e.stderr, "%s? [y/N] ", title(what))
		answer, _ := bufio.NewReader(e.stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
			return &exitError{ExitAPI, errors.New("aborted")}
		}
	}
	body, err = c.Send(e.ctx, http.MethodPost, path, q)
	if err != nil {
		return err
	}
	defer body.Close()
	return e.followAction(body)
}

const vmUsage = "usage: labctl vm restart|stop|start <vm> [--yes] [--dry-run] | labctl vm roll [vm...] [--yes]"

func cmdVM(e *env, args []string) error {
	if len(args) == 0 {
		return usageErr(vmUsage)
	}
	verb := args[0]
	if verb == "roll" {
		return cmdVMRoll(e, args[1:])
	}
	if verb != "restart" && verb != "stop" && verb != "start" {
		return usageErr(vmUsage)
	}
	fs := newFlags(e, "vm "+verb)
	yes := fs.Bool("yes", false, "don't ask for confirmation")
	fs.BoolVar(yes, "y", false, "short for --yes")
	dry := fs.Bool("dry-run", false, "only show what would happen")
	pos, err := parseArgs(fs, args[1:])
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr(vmUsage)
	}
	return e.runAction("/v1/actions/vms/"+url.PathEscape(pos[0])+"/"+verb, nil, *yes, *dry, verb+" "+pos[0])
}

// cmdVMRoll restarts VMs one at a time, agents before servers, and stops at
// the first failure: what scripts/k3s-roll.sh did.
func cmdVMRoll(e *env, args []string) error {
	fs := newFlags(e, "vm roll")
	yes := fs.Bool("yes", false, "don't ask for confirmation")
	fs.BoolVar(yes, "y", false, "short for --yes")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	c, err := e.client()
	if err != nil {
		return err
	}
	st, err := c.Status(e.ctx)
	if err != nil {
		return err
	}
	if !st.K3s.Reachable {
		// Without roles the order could put servers first; better not roll.
		return fmt.Errorf("k3s API unreachable, can't tell agents from servers: %s", st.K3s.Error)
	}
	server := map[string]bool{}
	for _, n := range st.K3s.Nodes {
		server[n.Name] = slices.Contains(n.Roles, "etcd")
	}
	var order []string
	for _, vm := range st.VMs {
		if vm.State == api.VMDisabled {
			continue
		}
		if len(pos) == 0 || slices.Contains(pos, vm.Name) {
			order = append(order, vm.Name)
		}
	}
	for _, p := range pos {
		if !slices.Contains(order, p) {
			return usageErr("unknown or disabled VM %q", p)
		}
	}
	slices.SortStableFunc(order, func(a, b string) int {
		switch {
		case !server[a] && server[b]:
			return -1
		case server[a] && !server[b]:
			return 1
		}
		return strings.Compare(a, b)
	})
	if len(order) == 0 {
		return usageErr("no VMs to restart")
	}
	if !*yes {
		if !isTerminal(e.stdin) {
			return usageErr("not a terminal: pass --yes to restart %s", strings.Join(order, ", "))
		}
		fmt.Fprintf(e.stderr, "Restart one at a time: %s? [y/N] ", strings.Join(order, ", "))
		answer, _ := bufio.NewReader(e.stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
			return &exitError{ExitAPI, errors.New("aborted")}
		}
	}
	for _, vm := range order {
		if err := e.runAction("/v1/actions/vms/"+url.PathEscape(vm)+"/restart", nil, true, false, "restart "+vm); err != nil {
			return fmt.Errorf("%s: %w (stopped the roll here)", vm, err)
		}
	}
	return nil
}

func cmdNode(e *env, args []string) error {
	if len(args) != 2 || (args[0] != "cordon" && args[0] != "uncordon") {
		return usageErr("usage: labctl node cordon|uncordon <node>")
	}
	c, err := e.client()
	if err != nil {
		return err
	}
	body, err := c.Send(e.ctx, http.MethodPost, "/v1/actions/nodes/"+url.PathEscape(args[1])+"/"+args[0], nil)
	if err != nil {
		return err
	}
	defer body.Close()
	var ev api.ActionEvent
	if err := json.NewDecoder(body).Decode(&ev); err != nil {
		return err
	}
	if e.json {
		return e.printJSON(ev)
	}
	fmt.Fprintln(e.stdout, ev.Message)
	return nil
}

const hostUsage = "usage: labctl host maintenance <host> [--reboot] [--yes] [--dry-run] | labctl host resume <host> [--yes] [--dry-run]"

func cmdHost(e *env, args []string) error {
	if len(args) == 0 || (args[0] != "maintenance" && args[0] != "resume") {
		return usageErr(hostUsage)
	}
	verb := args[0]
	fs := newFlags(e, "host "+verb)
	yes := fs.Bool("yes", false, "don't ask for confirmation")
	fs.BoolVar(yes, "y", false, "short for --yes")
	dry := fs.Bool("dry-run", false, "only show what would happen")
	reboot := false
	if verb == "maintenance" {
		fs.BoolVar(&reboot, "reboot", false, "reboot the host once its VMs are stopped")
	}
	pos, err := parseArgs(fs, args[1:])
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr(hostUsage)
	}
	q := url.Values{}
	if reboot {
		q.Set("reboot", "1")
	}
	what := "take " + pos[0] + " into maintenance"
	if verb == "resume" {
		what = "resume " + pos[0]
	}
	return e.runAction("/v1/actions/hosts/"+url.PathEscape(pos[0])+"/"+verb, q, *yes, *dry, what)
}

// followAction prints an action's progress stream and fails if it failed.
func (e *env) followAction(body io.Reader) error {
	sc := bufio.NewScanner(body)
	var last api.ActionEvent
	for sc.Scan() {
		if e.json {
			fmt.Fprintf(e.stdout, "%s\n", sc.Bytes())
		}
		if err := json.Unmarshal(sc.Bytes(), &last); err != nil {
			continue
		}
		if !e.json && last.Status != api.EventError {
			fmt.Fprintln(e.stdout, last.Message)
		}
	}
	if err := sc.Err(); err != nil && e.ctx.Err() == nil {
		return fmt.Errorf("lost the progress stream (the action keeps running; see labctl logs --unit clusterd): %w", err)
	}
	switch last.Status {
	case api.EventDone:
		return nil
	case api.EventError:
		return &exitError{ExitAPI, errors.New(last.Message)}
	}
	if e.ctx.Err() != nil {
		fmt.Fprintln(e.stderr, "Stopped following; the action keeps running on the server.")
		return nil
	}
	return &exitError{ExitAPI, errors.New("the progress stream ended early; check labctl logs --unit clusterd")}
}
