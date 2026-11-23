package actions

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jurrebuunk/clusterd/internal/api"
	"github.com/jurrebuunk/clusterd/internal/config"
	"github.com/jurrebuunk/clusterd/internal/kube"
)

// Host maintenance is Ceph's per-host noout flag: it survives the reboot and
// needs no state of our own.

type osdDump struct {
	CrushNodeFlags map[string][]string `json:"crush_node_flags"`
}

type osdMeta struct {
	ID       int    `json:"id"`
	Hostname string `json:"hostname"`
}

// MaintenanceHosts lists hosts that are in maintenance now.
func (e *Engine) MaintenanceHosts(ctx context.Context) ([]string, error) {
	var d osdDump
	if err := e.ceph(ctx, &d, "osd", "dump"); err != nil {
		return nil, err
	}
	var out []string
	for h, flags := range d.CrushNodeFlags {
		if slices.Contains(flags, "noout") {
			out = append(out, h)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (e *Engine) hostOSDs(ctx context.Context, host string) ([]int, error) {
	var meta []osdMeta
	if err := e.ceph(ctx, &meta, "osd", "metadata"); err != nil {
		return nil, err
	}
	var ids []int
	for _, m := range meta {
		if m.Hostname == host {
			ids = append(ids, m.ID)
		}
	}
	sort.Ints(ids)
	return ids, nil
}

// hostVMs returns the host's enabled VMs, agents before servers.
func (e *Engine) hostVMs(ctx context.Context, host string) ([]config.VM, map[string]api.K3sNode, error) {
	nodes, err := e.Kube.Nodes(ctx)
	if err != nil {
		return nil, nil, err
	}
	byName := map[string]api.K3sNode{}
	for _, n := range nodes {
		byName[n.Name] = n
	}
	var vms []config.VM
	for _, vm := range e.Cfg.VMs {
		if vm.Host == host && vm.Enabled {
			vms = append(vms, vm)
		}
	}
	server := func(v config.VM) bool { return slices.Contains(byName[v.Name].Roles, "etcd") }
	sort.SliceStable(vms, func(i, j int) bool { return !server(vms[i]) && server(vms[j]) })
	return vms, byName, nil
}

func (e *Engine) hostExists(name string) error {
	for _, h := range e.Cfg.Hosts {
		if h.Name == name {
			if name != e.Cfg.Host {
				return fmt.Errorf("host actions run on the host itself, not on %s", e.Cfg.Host)
			}
			return nil
		}
	}
	return fmt.Errorf("%w: no host %q", ErrNotFound, name)
}

// PlanHostMaintenance checks that the host can go and lists what happens.
func (e *Engine) PlanHostMaintenance(ctx context.Context, host string, reboot bool) (api.HostPlan, error) {
	plan := api.HostPlan{Host: host, VMs: []string{}, OSDs: []int{}}
	if err := e.hostExists(host); err != nil {
		return plan, err
	}
	inMaint, err := e.MaintenanceHosts(ctx)
	if err != nil {
		return plan, fmt.Errorf("ceph: %w", err)
	}
	for _, h := range inMaint {
		if h == host {
			return plan, preconditionf("%s is already in maintenance", host)
		}
		return plan, preconditionf("%s is in maintenance; resume it first", h)
	}
	vms, byName, err := e.hostVMs(ctx, host)
	if err != nil {
		return plan, fmt.Errorf("listing nodes: %w", err)
	}
	ours := map[string]bool{}
	for _, vm := range vms {
		ours[vm.Name] = true
		plan.VMs = append(plan.VMs, vm.Name)
	}
	var notReady []string
	servers, serversLeft := 0, 0
	for name, n := range byName {
		etcd := slices.Contains(n.Roles, "etcd")
		if etcd {
			servers++
		}
		if ours[name] {
			continue
		}
		if !n.Ready {
			notReady = append(notReady, name)
		} else if etcd {
			serversLeft++
		}
	}
	sort.Strings(notReady)
	if len(notReady) > 0 {
		return plan, preconditionf("nodes on other hosts are not Ready: %s", strings.Join(notReady, ", "))
	}
	if servers > 0 && serversLeft < servers/2+1 {
		return plan, preconditionf("etcd would lose quorum: %d of %d servers left", serversLeft, servers)
	}
	if plan.OSDs, err = e.hostOSDs(ctx, host); err != nil {
		return plan, fmt.Errorf("ceph: %w", err)
	}
	if len(plan.OSDs) > 0 {
		var ok struct {
			OKToStop bool     `json:"ok_to_stop"`
			BadPGs   []string `json:"bad_become_inactive"`
		}
		args := []string{"osd", "ok-to-stop"}
		for _, id := range plan.OSDs {
			args = append(args, strconv.Itoa(id))
		}
		err := e.ceph(ctx, &ok, args...)
		if err != nil || !ok.OKToStop {
			return plan, preconditionf("ceph says its OSDs can't stop now (PGs would go inactive)%s", errSuffix(err))
		}
	}
	for _, vm := range plan.VMs {
		plan.Steps = append(plan.Steps, fmt.Sprintf("drain node/%s and stop microvm@%s", vm, vm))
	}
	plan.Steps = append(plan.Steps, "set noout for "+host+"'s OSDs")
	if reboot {
		plan.Steps = append(plan.Steps, "reboot "+host)
	}
	if h, err := e.currentHolder(ctx); err == nil {
		plan.LockHolder = h
	}
	return plan, nil
}

func errSuffix(err error) string {
	if err == nil {
		return ""
	}
	return ": " + err.Error()
}

// StartHostMaintenance drains and stops the host's VMs and sets noout. With
// reboot, the host reboots once the answer is complete.
func (e *Engine) StartHostMaintenance(ctx context.Context, host string, reboot bool, c Caller) (*Run, error) {
	if err := e.hostExists(host); err != nil {
		return nil, err
	}
	lk, err := e.acquire(ctx, fmt.Sprintf("host maintenance %s by %s (%s)", host, c.Token, c.RequestID))
	if err != nil {
		return nil, err
	}
	if _, err := e.PlanHostMaintenance(ctx, host, reboot); err != nil {
		_ = lk.release(context.Background(), e)
		return nil, err
	}
	var after []func()
	if reboot {
		after = append(after, func() {
			// Give the client a moment to read the last line.
			time.Sleep(3 * time.Second)
			e.Log.Info("audit", "action", "host reboot", "target", host, "token", c.Token, "request_id", c.RequestID, "event", "start")
			if _, err := e.Run(context.Background(), "systemctl", "--no-ask-password", "reboot"); err != nil {
				e.Log.Error("reboot failed", "err", err)
			}
		})
	}
	return e.start("host maintenance", host, c, lk, func(ctx context.Context, emit func(string)) error {
		vms, _, err := e.hostVMs(ctx, host)
		if err != nil {
			return err
		}
		for _, vm := range vms {
			pods, err := e.Kube.PodsOnNode(ctx, vm.Name)
			if err != nil {
				return err
			}
			if err := e.cordonAndDrain(ctx, vm.Name, evictable(pods), emit); err != nil {
				return err
			}
			if err := e.unit(ctx, "stop", vm.Name); err != nil {
				return fmt.Errorf("stop microvm@%s: %w", vm.Name, err)
			}
			emit("microvm@" + vm.Name + " stopped")
		}
		if err := e.ceph(ctx, nil, "osd", "set-group", "noout", host); err != nil {
			return fmt.Errorf("set noout: %w", err)
		}
		emit("noout set for " + host)
		if reboot {
			emit(host + " reboots in a few seconds; afterwards: labctl host resume " + host)
		} else {
			emit(host + " is in maintenance; afterwards: labctl host resume " + host)
		}
		return nil
	}, after...), nil
}

// evictable drops pods a drain leaves alone (DaemonSets, static, finished).
// Bare pods were refused by the plan already.
func evictable(pods []kube.Pod) []kube.Pod {
	var out []kube.Pod
	for _, p := range pods {
		if p.Phase == "Succeeded" || p.Phase == "Failed" || p.Mirror || p.OwnerKind == "DaemonSet" || p.OwnerKind == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// PlanHostResume lists what resume would do.
func (e *Engine) PlanHostResume(ctx context.Context, host string) (api.HostPlan, error) {
	plan := api.HostPlan{Host: host, VMs: []string{}, OSDs: []int{}}
	if err := e.hostExists(host); err != nil {
		return plan, err
	}
	vms, _, err := e.hostVMs(ctx, host)
	if err != nil {
		return plan, fmt.Errorf("listing nodes: %w", err)
	}
	for _, vm := range vms {
		plan.VMs = append(plan.VMs, vm.Name)
		plan.Steps = append(plan.Steps, fmt.Sprintf("start microvm@%s if stopped, wait Ready, uncordon", vm.Name))
	}
	plan.Steps = append(plan.Steps, "unset noout for "+host, "wait until all PGs are clean")
	if h, err := e.currentHolder(ctx); err == nil {
		plan.LockHolder = h
	}
	return plan, nil
}

type cephPGs struct {
	PGMap struct {
		NumPGs     int `json:"num_pgs"`
		PGsByState []struct {
			StateName string `json:"state_name"`
			Count     int    `json:"count"`
		} `json:"pgs_by_state"`
	} `json:"pgmap"`
}

// StartHostResume brings the host's VMs back and lifts noout.
func (e *Engine) StartHostResume(ctx context.Context, host string, c Caller) (*Run, error) {
	if err := e.hostExists(host); err != nil {
		return nil, err
	}
	lk, err := e.acquire(ctx, fmt.Sprintf("host resume %s by %s (%s)", host, c.Token, c.RequestID))
	if err != nil {
		return nil, err
	}
	return e.start("host resume", host, c, lk, func(ctx context.Context, emit func(string)) error {
		vms, _, err := e.hostVMs(ctx, host)
		if err != nil {
			return err
		}
		for _, vm := range vms {
			state, err := e.unitState(ctx, vm.Name)
			if err != nil {
				return err
			}
			if state != "active" && state != "activating" {
				if err := e.unit(ctx, "start", vm.Name); err != nil {
					return fmt.Errorf("start microvm@%s: %w", vm.Name, err)
				}
				emit("microvm@" + vm.Name + " started")
			}
			if err := e.readyAndUncordon(ctx, vm.Name, "", emit); err != nil {
				return err
			}
		}
		if err := e.ceph(ctx, nil, "osd", "unset-group", "noout", host); err != nil {
			return fmt.Errorf("unset noout: %w", err)
		}
		emit("noout unset for " + host)
		deadline := e.now().Add(e.Timing.RecoverTimeout)
		lastNote := e.now()
		for {
			var st cephPGs
			if err := e.ceph(ctx, &st, "status"); err == nil {
				clean := 0
				for _, s := range st.PGMap.PGsByState {
					if s.StateName == "active+clean" {
						clean = s.Count
					}
				}
				if st.PGMap.NumPGs > 0 && clean == st.PGMap.NumPGs {
					emit(fmt.Sprintf("all %d PGs active+clean", clean))
					break
				}
				if e.now().Sub(lastNote) >= 30*time.Second {
					emit(fmt.Sprintf("Ceph recovering: %d/%d PGs clean", clean, st.PGMap.NumPGs))
					lastNote = e.now()
				}
			}
			if e.now().After(deadline) {
				return fmt.Errorf("PGs not clean after %s; check labctl status ceph", e.Timing.RecoverTimeout)
			}
			if err := e.sleep(ctx); err != nil {
				return err
			}
		}
		emit(host + " is back in service")
		return nil
	}), nil
}
