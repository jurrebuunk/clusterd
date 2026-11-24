// Package status gathers what /v1/internal/node and /v1/status report: this host's
// facts, its peers' facts, Kubernetes nodes and Ceph health.
package status

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jurrebuunk/clusterd/internal/api"
	"github.com/jurrebuunk/clusterd/internal/version"
)

// Node gathers this host's facts. Parts that fail are listed in Errors; the
// rest is still returned.
func (c *Collector) Node(ctx context.Context) api.Node {
	n := api.Node{Host: c.cfg.Host, Version: version.Version, VIPs: []string{}, VMs: []api.VMUnit{}}
	fail := func(what string, err error) { n.Errors = append(n.Errors, what+": "+err.Error()) }

	if up, err := readUptime(c.procRoot); err == nil {
		n.UptimeSeconds = up
	} else {
		fail("uptime", err)
	}
	if l, err := readLoad1(c.procRoot); err == nil {
		n.Load1 = l
	} else {
		fail("load", err)
	}
	if total, avail, err := readMem(c.procRoot); err == nil {
		n.MemTotalBytes, n.MemAvailableBytes = total, avail
	} else {
		fail("memory", err)
	}
	if vips, err := c.heldVIPs(); err == nil {
		n.VIPs = vips
	} else {
		fail("vips", err)
	}
	if vms, err := c.listVMs(ctx); err == nil {
		n.VMs = vms
	} else {
		fail("vms", err)
	}
	if sys, err := c.nixos(ctx); err != nil {
		fail("nixos", err)
	} else if sys.Version != "" {
		n.NixOS = &sys
	}
	self := c.cfg.Self()
	n.Stores = []api.StoreHealth{
		{Name: "victoriametrics", OK: c.storeOK(ctx, self.Metrics)},
		{Name: "victorialogs", OK: c.storeOK(ctx, self.Logs)},
	}
	return n
}

func readUptime(proc string) (int64, error) {
	b, err := os.ReadFile(proc + "/uptime")
	if err != nil {
		return 0, err
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0, fmt.Errorf("empty uptime")
	}
	v, err := strconv.ParseFloat(f[0], 64)
	return int64(v), err
}

func readLoad1(proc string) (float64, error) {
	b, err := os.ReadFile(proc + "/loadavg")
	if err != nil {
		return 0, err
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0, fmt.Errorf("empty loadavg")
	}
	return strconv.ParseFloat(f[0], 64)
}

func readMem(proc string) (total, avail uint64, err error) {
	f, err := os.Open(proc + "/meminfo")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		kb, _ := strconv.ParseUint(fields[1], 10, 64)
		switch fields[0] {
		case "MemTotal:":
			total = kb * 1024
		case "MemAvailable:":
			avail = kb * 1024
		}
	}
	if total == 0 {
		return 0, 0, fmt.Errorf("no MemTotal in meminfo")
	}
	return total, avail, sc.Err()
}

// heldVIPs lists the names of configured VIPs bound on this host.
func (c *Collector) heldVIPs() ([]string, error) {
	addrs, err := c.interfaceAddrs()
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok {
			have[ipn.IP.String()] = true
		}
	}
	out := []string{}
	for _, v := range c.cfg.VIPs {
		if have[v.Address] {
			out = append(out, v.Name)
		}
	}
	return out, nil
}

type systemdUnit struct {
	Unit   string `json:"unit"`
	Active string `json:"active"`
	Sub    string `json:"sub"`
}

// listVMs asks systemd for the microvm@ units. Read-only, no privileges.
func (c *Collector) listVMs(ctx context.Context) ([]api.VMUnit, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := c.run(ctx, "systemctl", "list-units", "--all", "--no-pager", "--output=json", "microvm@*.service")
	if err != nil {
		return nil, err
	}
	return parseUnits(out)
}

func parseUnits(out []byte) ([]api.VMUnit, error) {
	var units []systemdUnit
	if err := json.Unmarshal(out, &units); err != nil {
		return nil, fmt.Errorf("systemctl output: %w", err)
	}
	vms := []api.VMUnit{}
	for _, u := range units {
		name, ok := strings.CutPrefix(u.Unit, "microvm@")
		if !ok {
			continue
		}
		vms = append(vms, api.VMUnit{Name: strings.TrimSuffix(name, ".service"), Active: u.Active, Sub: u.Sub})
	}
	sort.Slice(vms, func(i, j int) bool { return vms[i].Name < vms[j].Name })
	return vms, nil
}

func (c *Collector) storeOK(ctx context.Context, base string) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/health", nil)
	if err != nil {
		return false
	}
	resp, err := c.plain.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// nixos reads what system runs: nixos-version --json of the current system
// (version, and the flake commit when system.configurationRevision is set),
// the system profile's generation, and whether a reboot is needed to boot
// into it. It only changes on a switch, so it is read again only then.
func (c *Collector) nixos(ctx context.Context) (api.NixOS, error) {
	cur, err := os.Readlink(c.sysRoot + "/run/current-system")
	if errors.Is(err, os.ErrNotExist) {
		return api.NixOS{}, nil // not NixOS
	} else if err != nil {
		return api.NixOS{}, err
	}
	c.nixosMu.Lock()
	defer c.nixosMu.Unlock()
	if cur == c.nixosKey {
		return c.nixosVal, nil
	}
	out, err := c.run(ctx, c.sysRoot+"/run/current-system/sw/bin/nixos-version", "--json")
	if err != nil {
		return api.NixOS{}, err
	}
	var v struct {
		Version  string `json:"nixosVersion"`
		Revision string `json:"configurationRevision"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return api.NixOS{}, fmt.Errorf("nixos-version: %w", err)
	}
	n := api.NixOS{Version: v.Version, Revision: v.Revision}
	if p, err := os.Readlink(c.sysRoot + "/nix/var/nix/profiles/system"); err == nil {
		g := strings.TrimSuffix(strings.TrimPrefix(p, "system-"), "-link")
		n.Generation, _ = strconv.Atoi(g)
	}
	for _, part := range []string{"kernel", "initrd", "kernel-modules"} {
		b, errB := os.Readlink(c.sysRoot + "/run/booted-system/" + part)
		r, errC := os.Readlink(c.sysRoot + "/run/current-system/" + part)
		if errB == nil && errC == nil && b != r {
			n.RebootRequired = true
		}
	}
	c.nixosKey, c.nixosVal = cur, n
	return n, nil
}
