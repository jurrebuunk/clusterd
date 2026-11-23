package actions

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jurrebuunk/clusterd/internal/api"
	"github.com/jurrebuunk/clusterd/internal/config"
	"github.com/jurrebuunk/clusterd/internal/kube"
)

// VM looks a VM up in the inventory.
func (e *Engine) VM(name string) (config.VM, error) {
	for _, vm := range e.Cfg.VMs {
		if vm.Name == name {
			return vm, nil
		}
	}
	return config.VM{}, fmt.Errorf("%w: no VM %q in the inventory", ErrNotFound, name)
}

func isReady(n api.K3sNode) bool { return n.Ready }

// PlanVMRestart checks whether restarting the VM is safe now and what it
// would do. It changes nothing.
func (e *Engine) PlanVMRestart(ctx context.Context, name string) (api.RestartPlan, []kube.Pod, error) {
	vm, err := e.VM(name)
	if err != nil {
		return api.RestartPlan{}, nil, err
	}
	if !vm.Enabled {
		return api.RestartPlan{}, nil, preconditionf("%s is disabled in the inventory", name)
	}
	plan := api.RestartPlan{VM: vm.Name, Host: vm.Host, Roles: []string{}, Evict: []string{}}

	nodes, err := e.Kube.Nodes(ctx)
	if err != nil {
		return plan, nil, fmt.Errorf("listing nodes: %w", err)
	}
	enabled := 0
	for _, v := range e.Cfg.VMs {
		if v.Enabled {
			enabled++
		}
	}
	if len(nodes) != enabled {
		return plan, nil, preconditionf("%d nodes registered, %d VMs enabled; fix that first", len(nodes), enabled)
	}
	var target *api.K3sNode
	var notReady []string
	servers, serversReadyOthers := 0, 0
	for i, n := range nodes {
		etcd := slices.Contains(n.Roles, "etcd")
		if etcd {
			servers++
		}
		if n.Name == name {
			target = &nodes[i]
			continue
		}
		if !isReady(n) {
			notReady = append(notReady, n.Name)
		} else if etcd {
			serversReadyOthers++
		}
	}
	if target == nil {
		return plan, nil, preconditionf("%s is not a Kubernetes node", name)
	}
	plan.Roles = target.Roles
	if len(notReady) > 0 {
		return plan, nil, preconditionf("other nodes are not Ready: %s", strings.Join(notReady, ", "))
	}
	if slices.Contains(target.Roles, "etcd") && serversReadyOthers < servers/2+1 {
		return plan, nil, preconditionf("etcd would lose quorum: %d of %d servers left", serversReadyOthers, servers)
	}

	pods, err := e.Kube.PodsOnNode(ctx, name)
	if err != nil {
		return plan, nil, fmt.Errorf("listing pods: %w", err)
	}
	var evict []kube.Pod
	var bare []string
	for _, p := range pods {
		switch {
		case p.Phase == "Succeeded" || p.Phase == "Failed":
		case p.Mirror || p.OwnerKind == "DaemonSet":
			plan.Skipped++
		case p.OwnerKind == "":
			bare = append(bare, p.Namespace+"/"+p.Name)
		default:
			evict = append(evict, p)
			plan.Evict = append(plan.Evict, p.Namespace+"/"+p.Name)
		}
	}
	if len(bare) > 0 {
		return plan, nil, preconditionf("pods without a controller would be lost: %s", strings.Join(bare, ", "))
	}
	plan.Steps = []string{
		"cordon node/" + name,
		fmt.Sprintf("drain node/%s (%d pods)", name, len(evict)),
		fmt.Sprintf("restart microvm@%s on %s", name, vm.Host),
		"wait for a new boot to be Ready",
		"uncordon node/" + name,
	}
	if h, err := e.currentHolder(ctx); err == nil {
		plan.LockHolder = h
	}
	return plan, evict, nil
}

// StartVMRestart checks, locks and starts the restart in the background.
// Errors here (not found, precondition, busy) mean nothing happened.
func (e *Engine) StartVMRestart(ctx context.Context, name string, c Caller) (*Run, error) {
	vm, err := e.VM(name)
	if err != nil {
		return nil, err
	}
	if vm.Host != e.Cfg.Host {
		return nil, fmt.Errorf("%s runs on %s, not on %s", name, vm.Host, e.Cfg.Host)
	}
	lk, err := e.acquire(ctx, fmt.Sprintf("vm restart %s by %s (%s, %s)", name, c.Token, e.Cfg.Host, c.RequestID))
	if err != nil {
		return nil, err
	}
	// Check again under the lock: the world may have changed since a dry run.
	_, pods, err := e.PlanVMRestart(ctx, name)
	if err != nil {
		_ = lk.release(context.Background(), e)
		return nil, err
	}
	return e.start("vm restart", name, c, lk, func(ctx context.Context, emit func(string)) error {
		return e.restartVM(ctx, name, pods, emit)
	}), nil
}

func (e *Engine) sleep(ctx context.Context) error {
	t := time.NewTimer(e.Timing.Poll)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (e *Engine) restartVM(ctx context.Context, name string, pods []kube.Pod, emit func(string)) error {
	before, err := e.Kube.Node(ctx, name)
	if err != nil {
		return err
	}
	if err := e.Kube.SetUnschedulable(ctx, name, true); err != nil {
		return fmt.Errorf("cordon: %w", err)
	}
	emit("node/" + name + " cordoned")

	if err := e.drain(ctx, name, pods, emit); err != nil {
		// Nothing was restarted: give the node back.
		if uerr := e.Kube.SetUnschedulable(context.Background(), name, false); uerr == nil {
			emit("node/" + name + " uncordoned after the failed drain")
		}
		return fmt.Errorf("drain: %w", err)
	}
	emit("node/" + name + " drained")

	rctx, cancel := context.WithTimeout(ctx, e.Timing.RestartTimeout)
	_, err = e.Run(rctx, "systemctl", "--no-ask-password", "restart", "microvm@"+name+".service")
	cancel()
	if err != nil {
		return fmt.Errorf("restart microvm@%s (node left cordoned): %w", name, err)
	}
	emit(fmt.Sprintf("microvm@%s restarted on %s", name, e.Cfg.Host))

	deadline := e.now().Add(e.Timing.BootTimeout)
	lastNote := e.now()
	for {
		n, err := e.Kube.Node(ctx, name)
		if err == nil && n.BootID != "" && n.BootID != before.BootID && n.Ready {
			break
		}
		if e.now().After(deadline) {
			return fmt.Errorf("node/%s not Ready with a new boot after %s (left cordoned)", name, e.Timing.BootTimeout)
		}
		if e.now().Sub(lastNote) >= 30*time.Second {
			emit("waiting for node/" + name + " to be Ready")
			lastNote = e.now()
		}
		if err := e.sleep(ctx); err != nil {
			return err
		}
	}
	emit("node/" + name + " Ready (new boot)")

	if err := e.Kube.SetUnschedulable(ctx, name, false); err != nil {
		return fmt.Errorf("uncordon: %w", err)
	}
	emit("node/" + name + " uncordoned")
	if err := e.Kube.Readyz(ctx); err != nil {
		return fmt.Errorf("API not ready after the restart: %w", err)
	}
	emit("Kubernetes API ready")
	return nil
}

// drain evicts the pods, retrying while a PodDisruptionBudget says no, and
// waits until they are gone.
func (e *Engine) drain(ctx context.Context, name string, pods []kube.Pod, emit func(string)) error {
	deadline := e.now().Add(e.Timing.DrainTimeout)
	pending := slices.Clone(pods)
	for len(pending) > 0 {
		var retry []kube.Pod
		for _, p := range pending {
			err := e.Kube.Evict(ctx, p)
			switch {
			case err == nil:
				emit("evicting pod " + p.Namespace + "/" + p.Name)
			case kube.IsStatus(err, http.StatusTooManyRequests):
				retry = append(retry, p) // disruption budget; try again
			default:
				return fmt.Errorf("evict %s/%s: %w", p.Namespace, p.Name, err)
			}
		}
		pending = retry
		if len(pending) == 0 {
			break
		}
		if e.now().After(deadline) {
			return fmt.Errorf("disruption budgets still block %d pods after %s", len(pending), e.Timing.DrainTimeout)
		}
		if err := e.sleep(ctx); err != nil {
			return err
		}
	}
	for _, p := range pods {
		for {
			gone, err := e.Kube.PodGone(ctx, p)
			if err != nil && !errors.Is(err, context.Canceled) {
				return err
			}
			if gone {
				break
			}
			if e.now().After(deadline) {
				return fmt.Errorf("pod %s/%s still running after %s", p.Namespace, p.Name, e.Timing.DrainTimeout)
			}
			if err := e.sleep(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}
