package actions

import (
	"context"
	"fmt"
	"net/http"

	"github.com/jurrebuunk/clusterd/internal/api"
	"github.com/jurrebuunk/clusterd/internal/kube"
)

// PlanVMStop: the restart's checks; the steps end with the VM stopped.
func (e *Engine) PlanVMStop(ctx context.Context, name string) (api.RestartPlan, error) {
	plan, _, err := e.PlanVMRestart(ctx, name)
	if err != nil {
		return plan, err
	}
	plan.Steps = []string{
		"cordon node/" + name,
		fmt.Sprintf("drain node/%s (%d pods)", name, len(plan.Evict)),
		fmt.Sprintf("stop microvm@%s on %s", name, plan.Host),
		"node/" + name + " stays cordoned until labctl vm start",
	}
	return plan, nil
}

// StartVMStop drains the node and stops the VM.
func (e *Engine) StartVMStop(ctx context.Context, name string, c Caller) (*Run, error) {
	if _, err := e.localVM(name); err != nil {
		return nil, err
	}
	lk, err := e.acquire(ctx, fmt.Sprintf("vm stop %s by %s (%s, %s)", name, c.Token, e.Cfg.Host, c.RequestID))
	if err != nil {
		return nil, err
	}
	_, pods, err := e.PlanVMRestart(ctx, name)
	if err != nil {
		_ = lk.release(context.Background(), e)
		return nil, err
	}
	return e.start("vm stop", name, c, lk, func(ctx context.Context, emit func(string)) error {
		if err := e.cordonAndDrain(ctx, name, pods, emit); err != nil {
			return err
		}
		if err := e.unit(ctx, "stop", name); err != nil {
			return fmt.Errorf("stop microvm@%s (node left cordoned): %w", name, err)
		}
		emit(fmt.Sprintf("microvm@%s stopped on %s; node/%s stays cordoned", name, e.Cfg.Host, name))
		return nil
	}), nil
}

// PlanVMStart: the VM must be enabled and not running.
func (e *Engine) PlanVMStart(ctx context.Context, name string) (api.RestartPlan, error) {
	vm, err := e.VM(name)
	if err != nil {
		return api.RestartPlan{}, err
	}
	if !vm.Enabled {
		return api.RestartPlan{}, preconditionf("%s is disabled in the inventory", name)
	}
	plan := api.RestartPlan{VM: vm.Name, Host: vm.Host, Roles: []string{}, Evict: []string{}}
	if vm.Host == e.Cfg.Host {
		state, err := e.unitState(ctx, name)
		if err != nil {
			return plan, err
		}
		if state == "active" || state == "activating" {
			return plan, preconditionf("microvm@%s is already %s", name, state)
		}
	}
	plan.Steps = []string{
		fmt.Sprintf("start microvm@%s on %s", name, vm.Host),
		"wait for node/" + name + " to be Ready",
		"uncordon node/" + name,
	}
	if h, err := e.currentHolder(ctx); err == nil {
		plan.LockHolder = h
	}
	return plan, nil
}

// StartVMStart starts the VM, waits until its node is Ready and uncordons it.
func (e *Engine) StartVMStart(ctx context.Context, name string, c Caller) (*Run, error) {
	if _, err := e.localVM(name); err != nil {
		return nil, err
	}
	lk, err := e.acquire(ctx, fmt.Sprintf("vm start %s by %s (%s, %s)", name, c.Token, e.Cfg.Host, c.RequestID))
	if err != nil {
		return nil, err
	}
	if _, err := e.PlanVMStart(ctx, name); err != nil {
		_ = lk.release(context.Background(), e)
		return nil, err
	}
	return e.start("vm start", name, c, lk, func(ctx context.Context, emit func(string)) error {
		before := ""
		if n, err := e.Kube.Node(ctx, name); err == nil {
			before = n.BootID
		}
		if err := e.unit(ctx, "start", name); err != nil {
			return fmt.Errorf("start microvm@%s: %w", name, err)
		}
		emit(fmt.Sprintf("microvm@%s started on %s", name, e.Cfg.Host))
		return e.readyAndUncordon(ctx, name, before, emit)
	}), nil
}

// SetCordon cordons or uncordons a node. It runs anywhere and takes no lock:
// it moves no workloads by itself.
func (e *Engine) SetCordon(ctx context.Context, node string, cordon bool, c Caller) (string, error) {
	if _, err := e.Kube.Node(ctx, node); kube.IsStatus(err, http.StatusNotFound) {
		return "", fmt.Errorf("%w: no node %q", ErrNotFound, node)
	} else if err != nil {
		return "", err
	}
	if err := e.Kube.SetUnschedulable(ctx, node, cordon); err != nil {
		return "", err
	}
	verb := "uncordoned"
	if cordon {
		verb = "cordoned"
	}
	e.Log.Info("audit", "action", "node "+verb, "target", node, "token", c.Token, "scope", c.Scope, "request_id", c.RequestID, "event", "end", "result", "ok")
	return "node/" + node + " " + verb, nil
}
