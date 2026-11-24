package status

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jurrebuunk/clusterd/internal/api"
)

// Thresholds for the checks.
const (
	wearWarn     = 80.0
	fsWarn       = 85.0
	fsFail       = 95.0
	certWarn     = 14 * 24 * time.Hour
	certFail     = 3 * 24 * time.Hour
	clockOffWarn = 0.1 // seconds
)

type checkList struct {
	checks []api.Check
}

// add records one area: the worst finding wins, all findings are listed.
func (l *checkList) add(name string, findings map[string][]string) {
	c := api.Check{Name: name, Status: api.CheckOK, Message: "ok"}
	var parts []string
	for _, level := range []string{api.CheckFail, api.CheckWarn} {
		if len(findings[level]) > 0 {
			if c.Status == api.CheckOK {
				c.Status = level
			}
			parts = append(parts, findings[level]...)
		}
	}
	if len(parts) > 0 {
		c.Message = strings.Join(parts, "; ")
	} else if m := findings["info"]; len(m) > 0 {
		c.Message = strings.Join(m, "; ")
	}
	l.checks = append(l.checks, c)
}

func rank(s string) int {
	switch s {
	case api.CheckFail:
		return 2
	case api.CheckWarn:
		return 1
	}
	return 0
}

// Checks turns the status and a few metrics into one verdict per area.
func (c *Collector) Checks(ctx context.Context, token string) api.Checks {
	var st api.Status
	met := map[string][]sample{}
	var metErr error
	var mu sync.Mutex
	var wg sync.WaitGroup
	var pods []api.Pod
	var podsErr error
	wg.Go(func() { st = c.Status(ctx, token) })
	wg.Go(func() { pods, podsErr = c.Pods(ctx) })
	for name, q := range map[string]string{
		"failed": `node_systemd_unit_state{state="failed"} == 1`,
		"probes": `probe_success == 0`,
		"down":   `up == 0`,
		"tunnel": `cloudflared_tunnel_ha_connections`,
		"clock":  `abs(node_timex_offset_seconds) > ` + fmt.Sprint(clockOffWarn),
	} {
		wg.Go(func() {
			qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			ss, err := c.query(qctx, q)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				metErr = err
				return
			}
			met[name] = ss
		})
	}
	wg.Wait()

	var l checkList
	f := func() map[string][]string { return map[string][]string{} }

	// Hosts: reachable, stores, one clusterd version, one commit, reboots,
	// maintenance.
	h := f()
	versions := map[string]bool{}
	revisions := map[string]bool{}
	inMaint := map[string]bool{}
	for _, hs := range st.Hosts {
		switch {
		case !hs.Reachable:
			h[api.CheckFail] = append(h[api.CheckFail], hs.Name+" unreachable")
		case hs.Maintenance:
			inMaint[hs.Name] = true
			h[api.CheckWarn] = append(h[api.CheckWarn], hs.Name+" in maintenance")
		}
		for _, s := range hs.Stores {
			if !s.OK {
				h[api.CheckWarn] = append(h[api.CheckWarn], hs.Name+" "+s.Name+" down")
			}
		}
		if hs.Version != "" {
			versions[hs.Version] = true
		}
		if n := hs.NixOS; n != nil {
			if n.Revision != "" {
				revisions[shortRev(n.Revision)] = true
			}
			if n.RebootRequired {
				h[api.CheckWarn] = append(h[api.CheckWarn], hs.Name+" needs a reboot (new kernel)")
			}
		}
	}
	if len(revisions) > 1 {
		h[api.CheckWarn] = append(h[api.CheckWarn], "hosts run different commits: "+strings.Join(sortedSet(revisions), ", "))
	}
	if len(versions) > 1 {
		var vs []string
		for v := range versions {
			vs = append(vs, v)
		}
		sort.Strings(vs)
		h[api.CheckWarn] = append(h[api.CheckWarn], "clusterd versions differ: "+strings.Join(vs, ", "))
	}
	h["info"] = []string{fmt.Sprintf("%d hosts up", len(st.Hosts)-len(h[api.CheckFail]))}
	l.add("hosts", h)

	u := f()
	for _, s := range met["failed"] {
		u[api.CheckFail] = append(u[api.CheckFail], s.Metric["instance"]+" "+s.Metric["name"])
	}
	l.add("units", u)

	v := f()
	for _, vm := range st.VMs {
		switch vm.State {
		case api.VMRunning, api.VMDisabled:
		case api.VMStopped, api.VMUnknown:
			level := api.CheckFail
			if inMaint[vm.Host] {
				level = api.CheckWarn
			}
			v[level] = append(v[level], vm.Name+" "+vm.State)
		default:
			v[api.CheckFail] = append(v[api.CheckFail], vm.Name+" "+vm.State)
		}
	}
	l.add("vms", v)

	n := f()
	if !st.K3s.Reachable {
		n[api.CheckFail] = append(n[api.CheckFail], "k3s API unreachable: "+st.K3s.Error)
	}
	for _, node := range st.K3s.Nodes {
		if !node.Ready {
			n[api.CheckFail] = append(n[api.CheckFail], node.Name+" NotReady")
		} else if !node.Schedulable {
			n[api.CheckWarn] = append(n[api.CheckWarn], node.Name+" cordoned")
		}
	}
	l.add("nodes", n)

	// Pods: only when k3s answers; the nodes check reports it otherwise.
	pd := f()
	switch {
	case podsErr != nil && st.K3s.Reachable:
		pd[api.CheckWarn] = append(pd[api.CheckWarn], "pods: "+podsErr.Error())
	case podsErr == nil:
		var bad []string
		for _, p := range pods {
			if p.Failing {
				why := p.Reason
				if why == "" {
					why = p.Phase
				}
				bad = append(bad, p.Namespace+"/"+p.Name+" ("+why+")")
			}
		}
		if len(bad) > 3 {
			bad = append(bad[:3], fmt.Sprintf("and %d more", len(bad)-3))
		}
		pd[api.CheckWarn] = bad
		pd["info"] = []string{fmt.Sprintf("%d pods", len(pods))}
	}
	l.add("pods", pd)

	cp := f()
	switch {
	case st.Ceph.Error != "":
		cp[api.CheckFail] = append(cp[api.CheckFail], st.Ceph.Error)
	case st.Ceph.Health == "HEALTH_ERR":
		cp[api.CheckFail] = append(cp[api.CheckFail], st.Ceph.Checks...)
	case st.Ceph.Health != "HEALTH_OK":
		cp[api.CheckWarn] = append(cp[api.CheckWarn], st.Ceph.Checks...)
		if len(st.Ceph.Checks) == 0 {
			cp[api.CheckWarn] = append(cp[api.CheckWarn], st.Ceph.Health)
		}
	}
	l.add("ceph", cp)

	vp := f()
	for _, vip := range st.VIPs {
		switch len(vip.Owners) {
		case 1:
		case 0:
			vp[api.CheckFail] = append(vp[api.CheckFail], vip.Name+" has no owner")
		default:
			vp[api.CheckFail] = append(vp[api.CheckFail], vip.Name+" on "+strings.Join(vip.Owners, " and ")+" (split brain)")
		}
	}
	l.add("vips", vp)

	d := f()
	for _, e := range st.Disks.Errors {
		d[api.CheckWarn] = append(d[api.CheckWarn], e)
	}
	for _, k := range st.Disks.Disks {
		if !k.Healthy {
			d[api.CheckFail] = append(d[api.CheckFail], k.Host+" "+k.Device+" fails SMART")
		}
		if k.WearPercent != nil && *k.WearPercent >= wearWarn {
			d[api.CheckWarn] = append(d[api.CheckWarn], fmt.Sprintf("%s %s %.0f%% worn", k.Host, k.Device, *k.WearPercent))
		}
	}
	for _, fs := range st.Disks.Filesystems {
		switch {
		case fs.UsedPercent >= fsFail:
			d[api.CheckFail] = append(d[api.CheckFail], fmt.Sprintf("%s %s %.0f%% full", fs.Host, fs.Mountpoint, fs.UsedPercent))
		case fs.UsedPercent >= fsWarn:
			d[api.CheckWarn] = append(d[api.CheckWarn], fmt.Sprintf("%s %s %.0f%% full", fs.Host, fs.Mountpoint, fs.UsedPercent))
		}
	}
	l.add("disks", d)

	s3 := f()
	up := 0
	for _, g := range st.S3.Gateways {
		if g.Up {
			up++
		} else {
			s3[api.CheckWarn] = append(s3[api.CheckWarn], "gateway "+g.Host+" down")
		}
	}
	if len(st.S3.Gateways) > 0 && up == 0 {
		s3[api.CheckFail] = append(s3[api.CheckFail], "no gateway up")
	}
	if ce := st.S3.CertExpires; ce != nil {
		left := ce.Sub(c.now())
		switch {
		case left < certFail:
			s3[api.CheckFail] = append(s3[api.CheckFail], "certificate expires in "+left.Round(time.Hour).String())
		case left < certWarn:
			s3[api.CheckWarn] = append(s3[api.CheckWarn], "certificate expires in "+left.Round(time.Hour).String())
		}
	}
	for _, e := range st.S3.Errors {
		s3[api.CheckWarn] = append(s3[api.CheckWarn], e)
	}
	l.add("s3", s3)

	t := f()
	tunnelBy := map[string]float64{}
	for _, s := range met["tunnel"] {
		tunnelBy[s.Metric["instance"]] = s.Value
	}
	for _, hs := range st.Hosts {
		if hs.Reachable && tunnelBy[hs.Name] < 1 && met["tunnel"] != nil {
			t[api.CheckFail] = append(t[api.CheckFail], hs.Name+" has no tunnel connection")
		}
	}
	l.add("tunnel", t)

	m := f()
	if metErr != nil {
		m[api.CheckWarn] = append(m[api.CheckWarn], "metrics: "+metErr.Error())
	}
	for _, s := range met["probes"] {
		m[api.CheckWarn] = append(m[api.CheckWarn], "probe "+s.Metric["instance"]+" fails from "+s.Metric["probe_from"])
	}
	for _, s := range met["down"] {
		m[api.CheckWarn] = append(m[api.CheckWarn], "target "+s.Metric["job"]+"/"+s.Metric["instance"]+" down")
	}
	for _, s := range met["clock"] {
		m[api.CheckWarn] = append(m[api.CheckWarn], fmt.Sprintf("%s clock off by %.2fs", s.Metric["instance"], math.Abs(s.Value)))
	}
	l.add("monitoring", m)

	out := api.Checks{Status: api.CheckOK, Checks: l.checks}
	for _, ch := range l.checks {
		if rank(ch.Status) > rank(out.Status) {
			out.Status = ch.Status
		}
	}
	return out
}

// shortRev shortens a git commit like git log --oneline; a dirty tree keeps
// its "-dirty".
func shortRev(r string) string {
	base, dirty := strings.CutSuffix(r, "-dirty")
	if len(base) > 7 {
		base = base[:7]
	}
	if dirty {
		base += "-dirty"
	}
	return base
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
