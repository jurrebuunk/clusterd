package labctl

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/jurrebuunk/clusterd/internal/api"
)

func cmdVM(e *env, args []string) error {
	if len(args) == 0 || args[0] != "restart" {
		return usageErr("usage: labctl vm restart <vm> [--yes] [--dry-run]")
	}
	fs := newFlags(e, "vm restart")
	yes := fs.Bool("yes", false, "don't ask for confirmation")
	fs.BoolVar(yes, "y", false, "short for --yes")
	dry := fs.Bool("dry-run", false, "only show what would happen")
	pos, err := parseArgs(fs, args[1:])
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("usage: labctl vm restart <vm> [--yes] [--dry-run]")
	}
	vm := pos[0]
	c, err := e.client()
	if err != nil {
		return err
	}

	plan, err := c.RestartPlan(e.ctx, vm)
	if err != nil {
		return err
	}
	if *dry {
		if e.json {
			return e.printJSON(plan)
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
	if !*yes {
		if !isTerminal(e.stdin) {
			return usageErr("not a terminal: pass --yes to restart %s", vm)
		}
		fmt.Fprintf(e.stderr, "Restart %s on %s: cordon, drain %d pods, restart, wait Ready, uncordon? [y/N] ",
			vm, plan.Host, len(plan.Evict))
		answer, _ := bufio.NewReader(e.stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
			return &exitError{ExitAPI, errors.New("aborted")}
		}
	}

	body, err := c.RestartVM(e.ctx, vm)
	if err != nil {
		return err
	}
	defer body.Close()
	return e.followAction(body)
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
		return fmt.Errorf("lost the progress stream (the action keeps running; see labctl logs --unit clusterd.service): %w", err)
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
	return &exitError{ExitAPI, errors.New("the progress stream ended early; check labctl logs --unit clusterd.service")}
}
