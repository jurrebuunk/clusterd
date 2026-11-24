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
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/jurrebuunk/clusterd/internal/api"
)

// actionFlags are the same on every action.
type actionFlags struct {
	yes, dry, noWait bool
}

func (f *actionFlags) add(c *cobra.Command) {
	c.Flags().BoolVarP(&f.yes, "yes", "y", false, "don't ask for confirmation")
	c.Flags().BoolVar(&f.dry, "dry-run", false, "only show what would happen")
	c.Flags().BoolVar(&f.noWait, "no-wait", false, "start it and return; follow with labctl operation show --watch")
}

// actionPlan is the common part of the plans actions return on a dry run.
type actionPlan struct {
	Host       string   `json:"host"`
	Steps      []string `json:"steps"`
	LockHolder string   `json:"lock_holder"`
}

// runAction asks for the plan, shows it, confirms and runs the action,
// following its progress unless --no-wait.
func (e *env) runAction(path string, q url.Values, f actionFlags, what string) error {
	c, err := e.client()
	if err != nil {
		return err
	}
	with := func(k, v string) url.Values {
		out := url.Values{k: {v}}
		for k, v := range q {
			out[k] = v
		}
		return out
	}
	body, err := c.Send(e.ctx, http.MethodPost, path, with("dry_run", "true"))
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
	if f.dry {
		if e.json() {
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
	if !f.yes {
		if err := e.confirm(title(what), what, plan.Steps); err != nil {
			return err
		}
	}
	if f.noWait {
		body, err := c.Send(e.ctx, http.MethodPost, path, q)
		if err != nil {
			return err
		}
		defer body.Close()
		var op api.Operation
		if err := json.NewDecoder(body).Decode(&op); err != nil {
			return err
		}
		if e.json() {
			return e.printJSON(op)
		}
		fmt.Fprintf(e.stdout, "operation/%s started\n", op.ID)
		return nil
	}
	resp, err := c.Request(e.ctx, http.MethodPost, path, with("wait", "true"))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return e.followAction(resp.Body, strings.TrimPrefix(resp.Header.Get("Location"), "/v1/operations/"))
}

// followAction prints an action's progress stream and fails if it failed.
func (e *env) followAction(body io.Reader, opID string) error {
	sc := bufio.NewScanner(body)
	var last api.ActionEvent
	for sc.Scan() {
		if e.json() {
			fmt.Fprintf(e.stdout, "%s\n", sc.Bytes())
		}
		if err := json.Unmarshal(sc.Bytes(), &last); err != nil {
			continue
		}
		if !e.json() && last.Status != api.EventError {
			fmt.Fprintln(e.stdout, last.Message)
		}
	}
	see := "labctl operation show " + opID
	if err := sc.Err(); err != nil && e.ctx.Err() == nil {
		return fmt.Errorf("lost the progress stream; the action keeps running (%s): %w", see, err)
	}
	switch last.Status {
	case api.EventDone:
		return nil
	case api.EventError:
		return &exitError{ExitAPI, errors.New(last.Message)}
	}
	if e.ctx.Err() != nil {
		fmt.Fprintf(e.stderr, "Stopped following; the action keeps running (%s).\n", see)
		return nil
	}
	return &exitError{ExitAPI, fmt.Errorf("the progress stream ended early; check %s", see)}
}

// VMs.

func cmdVM(e *env) *cobra.Command {
	c := group("vm", "VMs: list, start, stop, restart one safely, or roll them all")
	names := e.complete("/v1/vms")

	c.AddCommand(&cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "VMs from the inventory and their state", Args: noArgs,
		RunE: func(*cobra.Command, []string) error {
			var l api.List[api.VMStatus]
			if err := e.get("/v1/vms", &l); err != nil {
				return err
			}
			return e.render(l, namesOf(l.Items, func(v api.VMStatus) string { return v.Name }), func() error {
				tw := newTable(e.stdout)
				printVMs(tw, l.Items)
				return tw.Flush()
			})
		},
	})
	c.AddCommand(&cobra.Command{
		Use: "show VM", Short: "One VM", Args: args(1, "VM"), ValidArgsFunction: names,
		RunE: func(_ *cobra.Command, a []string) error {
			var v api.VMStatus
			if err := e.get("/v1/vms/"+url.PathEscape(a[0]), &v); err != nil {
				return err
			}
			return e.render(v, []string{v.Name}, func() error {
				return e.fields([][2]string{{"Name", v.Name}, {"Host", v.Host}, {"State", title(v.State)}})
			})
		},
	})
	for _, verb := range []struct{ name, short string }{
		{"restart", "Restart a VM safely: cordon, drain, restart, wait Ready, uncordon"},
		{"stop", "Stop a VM safely: cordon, drain, stop (the node stays cordoned)"},
		{"start", "Start a stopped VM, wait until its node is Ready, uncordon"},
	} {
		var f actionFlags
		cmd := &cobra.Command{
			Use: verb.name + " VM", Short: verb.short, Args: args(1, "VM"), ValidArgsFunction: names,
			RunE: func(_ *cobra.Command, a []string) error {
				return e.runAction("/v1/vms/"+url.PathEscape(a[0])+"/"+verb.name, nil, f, verb.name+" "+a[0])
			},
		}
		f.add(cmd)
		c.AddCommand(cmd)
	}
	c.AddCommand(cmdVMRoll(e))
	return c
}

// cmdVMRoll restarts VMs one at a time, agents before servers, and stops at
// the first failure.
func cmdVMRoll(e *env) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use: "roll [VM...]", Short: "Restart every enabled VM (or the given ones) one at a time, agents first",
		ValidArgsFunction: e.complete("/v1/vms"),
		RunE: func(_ *cobra.Command, pos []string) error {
			var vms api.List[api.VMStatus]
			var nodes api.List[api.K3sNode]
			if err := e.get("/v1/vms", &vms); err != nil {
				return err
			}
			if err := e.get("/v1/nodes", &nodes); err != nil {
				return err
			}
			if len(nodes.Errors) > 0 {
				// Without roles the order could put servers first; better not roll.
				return fmt.Errorf("can't tell agents from servers: %s", strings.Join(nodes.Errors, "; "))
			}
			server := map[string]bool{}
			for _, n := range nodes.Items {
				server[n.Name] = slices.Contains(n.Roles, "etcd")
			}
			var order []string
			for _, vm := range vms.Items {
				if vm.State != api.VMDisabled && (len(pos) == 0 || slices.Contains(pos, vm.Name)) {
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
			if !yes {
				list := strings.Join(order, ", ")
				if err := e.confirm("Restart one at a time: "+list, "restart "+list, nil); err != nil {
					return err
				}
			}
			for _, vm := range order {
				if err := e.runAction("/v1/vms/"+url.PathEscape(vm)+"/restart", nil, actionFlags{yes: true}, "restart "+vm); err != nil {
					return fmt.Errorf("%s: %w (stopped the roll here)", vm, err)
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "don't ask for confirmation")
	return cmd
}

// Hosts.

func cmdHost(e *env) *cobra.Command {
	c := group("host", "Hypervisors: list, show, maintenance and resume")
	names := e.complete("/v1/hosts")
	c.AddCommand(&cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "Hypervisors: status, version, uptime, load, memory", Args: noArgs,
		RunE: func(*cobra.Command, []string) error {
			var l api.List[api.HostStatus]
			if err := e.get("/v1/hosts", &l); err != nil {
				return err
			}
			return e.render(l, namesOf(l.Items, func(h api.HostStatus) string { return h.Name }), func() error {
				tw := newTable(e.stdout)
				printHosts(tw, e.stderr, l.Items)
				return tw.Flush()
			})
		},
	})
	c.AddCommand(&cobra.Command{
		Use: "show HOST", Short: "One hypervisor", Args: args(1, "HOST"), ValidArgsFunction: names,
		RunE: func(_ *cobra.Command, a []string) error {
			var h api.HostStatus
			if err := e.get("/v1/hosts/"+url.PathEscape(a[0]), &h); err != nil {
				return err
			}
			return e.render(h, []string{h.Name}, func() error {
				status := hostState(h)
				rows := [][2]string{{"Name", h.Name}, {"Status", status}}
				if h.Reachable {
					rows = append(rows, [2]string{"Clusterd", h.Version})
					if n := h.NixOS; n != nil {
						rows = append(rows, [2]string{"NixOS", n.Version}, [2]string{"Commit", n.Revision},
							[2]string{"Generation", strconv.Itoa(n.Generation)}, [2]string{"Reboot needed", map[bool]string{true: "yes", false: "no"}[n.RebootRequired]})
					}
					rows = append(rows, [2]string{"Uptime", age(time.Duration(h.UptimeSeconds) * time.Second)},
						[2]string{"Load", fmt.Sprintf("%.2f", h.Load1)}, [2]string{"Memory", fmt.Sprintf("%.0f%% used", h.MemUsedPercent)})
					for _, s := range h.Stores {
						rows = append(rows, [2]string{"Store " + s.Name, map[bool]string{true: "OK", false: "Down"}[s.OK]})
					}
				}
				if h.Error != "" {
					rows = append(rows, [2]string{"Error", h.Error})
				}
				return e.fields(rows)
			})
		},
	})
	var mf actionFlags
	var reboot bool
	maint := &cobra.Command{
		Use: "maintenance HOST", Short: "Take a host out: drain and stop its VMs, set noout for its OSDs",
		Args: args(1, "HOST"), ValidArgsFunction: names,
		RunE: func(_ *cobra.Command, a []string) error {
			q := url.Values{}
			if reboot {
				q.Set("reboot", "true")
			}
			return e.runAction("/v1/hosts/"+url.PathEscape(a[0])+"/maintenance", q, mf, "take "+a[0]+" into maintenance")
		},
	}
	mf.add(maint)
	maint.Flags().BoolVar(&reboot, "reboot", false, "reboot the host once its VMs are stopped")
	var rf actionFlags
	resume := &cobra.Command{
		Use: "resume HOST", Short: "Bring a host back: start its VMs, uncordon, unset noout, wait for clean PGs",
		Args: args(1, "HOST"), ValidArgsFunction: names,
		RunE: func(_ *cobra.Command, a []string) error {
			return e.runAction("/v1/hosts/"+url.PathEscape(a[0])+"/resume", nil, rf, "resume "+a[0])
		},
	}
	rf.add(resume)
	c.AddCommand(maint, resume)
	return c
}

// Nodes.

func cmdNode(e *env) *cobra.Command {
	c := group("node", "Kubernetes nodes: list, show, cordon and uncordon")
	names := e.complete("/v1/nodes")
	c.AddCommand(&cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "Kubernetes nodes", Args: noArgs,
		RunE: func(*cobra.Command, []string) error {
			var l api.List[api.K3sNode]
			if err := e.get("/v1/nodes", &l); err != nil {
				return err
			}
			return e.render(l, namesOf(l.Items, func(n api.K3sNode) string { return n.Name }), func() error {
				tw := newTable(e.stdout)
				printNodes(tw, e.stderr, l)
				return tw.Flush()
			})
		},
	})
	c.AddCommand(&cobra.Command{
		Use: "show NODE", Short: "One Kubernetes node", Args: args(1, "NODE"), ValidArgsFunction: names,
		RunE: func(_ *cobra.Command, a []string) error {
			var n api.K3sNode
			if err := e.get("/v1/nodes/"+url.PathEscape(a[0]), &n); err != nil {
				return err
			}
			return e.render(n, []string{n.Name}, func() error {
				return e.fields([][2]string{{"Name", n.Name}, {"Status", nodeState(n)}, {"Roles", roles(n)},
					{"Version", n.Version}, {"Zone", n.Zone}})
			})
		},
	})
	for _, verb := range []string{"cordon", "uncordon"} {
		c.AddCommand(&cobra.Command{
			Use: verb + " NODE", Short: map[string]string{"cordon": "Mark a node unschedulable", "uncordon": "Mark a node schedulable again"}[verb],
			Args: args(1, "NODE"), ValidArgsFunction: names,
			RunE: func(_ *cobra.Command, a []string) error {
				cl, err := e.client()
				if err != nil {
					return err
				}
				body, err := cl.Send(e.ctx, http.MethodPost, "/v1/nodes/"+url.PathEscape(a[0])+"/"+verb, nil)
				if err != nil {
					return err
				}
				defer body.Close()
				var op api.Operation
				if err := json.NewDecoder(body).Decode(&op); err != nil {
					return err
				}
				if e.json() {
					return e.printJSON(op)
				}
				for _, ev := range op.Events {
					fmt.Fprintln(e.stdout, ev.Message)
				}
				return nil
			},
		})
	}
	return c
}

// fields prints name/value pairs, kubectl describe style.
func (e *env) fields(rows [][2]string) error {
	tw := tabwriter.NewWriter(e.stdout, 0, 4, 2, ' ', 0)
	for _, r := range rows {
		v := r[1]
		if v == "" {
			v = "-"
		}
		fmt.Fprintf(tw, "%s:\t%s\n", r[0], v)
	}
	return tw.Flush()
}

func namesOf[T any](items []T, name func(T) string) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, name(it))
	}
	return out
}
