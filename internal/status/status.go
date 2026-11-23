package status

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jurrebuunk/clusterd/internal/api"
	"github.com/jurrebuunk/clusterd/internal/cmdrun"
	"github.com/jurrebuunk/clusterd/internal/config"
	"github.com/jurrebuunk/clusterd/internal/kube"
)

// Collector answers /v1/node and /v1/status.
type Collector struct {
	cfg   *config.Config
	kube  *kube.Client
	log   *slog.Logger
	peers *http.Client // to other clusterd instances, TLS verified against ServerName
	plain *http.Client // to the local stores

	// Overridable in tests.
	procRoot       string
	run            func(ctx context.Context, name string, args ...string) ([]byte, error)
	interfaceAddrs func() ([]net.Addr, error)
	now            func() time.Time

	// Ceph health is slow to ask (the CLI takes about half a second), so it is
	// refreshed in the background and /v1/status answers from this copy.
	cephMu       sync.Mutex
	cephVal      api.CephStatus
	cephInflight chan struct{}
}

// CephRefresh is how often the background loop asks Ceph.
const CephRefresh = 10 * time.Second

// cephStale is when a cached answer is no longer trusted.
const cephStale = 60 * time.Second

// New builds a collector. kc may be nil when no kubeconfig is configured.
func New(cfg *config.Config, kc *kube.Client, log *slog.Logger) *Collector {
	return &Collector{
		cfg:  cfg,
		kube: kc,
		log:  log,
		peers: &http.Client{
			Timeout: cfg.Limits.PeerTimeout.Duration,
			Transport: &http.Transport{
				TLSClientConfig:     &tls.Config{ServerName: cfg.ServerName, MinVersion: tls.VersionTLS12},
				TLSHandshakeTimeout: 2 * time.Second,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		plain:          &http.Client{Timeout: 5 * time.Second},
		procRoot:       "/proc",
		run:            cmdrun.Run,
		interfaceAddrs: net.InterfaceAddrs,
		now:            time.Now,
	}
}

// Status gathers the whole site. token is the caller's own bearer token: it
// is passed on to the peers' /v1/node, so a peer only answers callers it
// would answer itself and no extra credential is needed between instances.
func (c *Collector) Status(ctx context.Context, token string) api.Status {
	st := api.Status{Site: c.cfg.Site, ServedBy: c.cfg.Host, GeneratedAt: c.now().UTC()}

	var wg sync.WaitGroup
	nodes := make([]*api.Node, len(c.cfg.Hosts))
	errs := make([]error, len(c.cfg.Hosts))
	for i, h := range c.cfg.Hosts {
		wg.Go(func() {
			if h.Name == c.cfg.Host {
				n := c.Node(ctx)
				nodes[i] = &n
				return
			}
			nodes[i], errs[i] = c.peerNode(ctx, h, token)
		})
	}
	wg.Go(func() { st.K3s = c.k3s(ctx) })
	wg.Go(func() { st.Ceph = c.cephCached(ctx) })
	wg.Wait()

	byHost := map[string]*api.Node{}
	for i, h := range c.cfg.Hosts {
		hs := api.HostStatus{Name: h.Name}
		if n := nodes[i]; n != nil {
			byHost[h.Name] = n
			hs.Reachable = true
			hs.Version = n.Version
			hs.UptimeSeconds = n.UptimeSeconds
			hs.Load1 = n.Load1
			if n.MemTotalBytes > 0 {
				hs.MemUsedPercent = round1(100 * float64(n.MemTotalBytes-n.MemAvailableBytes) / float64(n.MemTotalBytes))
			}
			hs.Stores = n.Stores
			hs.Error = strings.Join(n.Errors, "; ")
		} else {
			hs.Error = errs[i].Error()
		}
		st.Hosts = append(st.Hosts, hs)
	}
	st.VMs = vmStates(c.cfg.VMs, byHost)
	st.VIPs = vipOwners(c.cfg.VIPs, c.cfg.Hosts, byHost)
	return st
}

func round1(v float64) float64 { return float64(int64(v*10+0.5)) / 10 }

func (c *Collector) peerNode(ctx context.Context, h config.Host, token string) (*api.Node, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(h.API, "/")+"/v1/node", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.peers.Do(req)
	if err != nil {
		return nil, fmt.Errorf("unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return nil, fmt.Errorf("peer answered %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var n api.Node
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&n); err != nil {
		return nil, fmt.Errorf("peer response: %w", err)
	}
	return &n, nil
}

func vmStates(vms []config.VM, byHost map[string]*api.Node) []api.VMStatus {
	out := []api.VMStatus{}
	for _, vm := range vms {
		s := api.VMStatus{Name: vm.Name, Host: vm.Host}
		n := byHost[vm.Host]
		switch {
		case !vm.Enabled:
			s.State = api.VMDisabled
		case n == nil:
			s.State = api.VMUnknown
		default:
			s.State = api.VMMissing
			for _, u := range n.VMs {
				if u.Name == vm.Name {
					s.State = unitState(u)
				}
			}
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func unitState(u api.VMUnit) string {
	switch u.Active {
	case "active":
		if u.Sub == "running" {
			return api.VMRunning
		}
		return api.VMStarting
	case "activating", "reloading":
		return api.VMStarting
	case "failed":
		return api.VMFailed
	case "inactive", "deactivating":
		return api.VMStopped
	}
	return api.VMUnknown
}

func vipOwners(vips []config.VIP, hosts []config.Host, byHost map[string]*api.Node) []api.VIPStatus {
	out := []api.VIPStatus{}
	for _, v := range vips {
		s := api.VIPStatus{Name: v.Name, Address: v.Address, Owners: []string{}}
		for _, h := range hosts {
			if n := byHost[h.Name]; n != nil {
				for _, held := range n.VIPs {
					if held == v.Name {
						s.Owners = append(s.Owners, h.Name)
					}
				}
			}
		}
		out = append(out, s)
	}
	return out
}

func (c *Collector) k3s(ctx context.Context) api.K3sStatus {
	if c.kube == nil {
		return api.K3sStatus{Nodes: []api.K3sNode{}, Error: "not configured"}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	nodes, err := c.kube.Nodes(ctx)
	if err != nil {
		return api.K3sStatus{Nodes: []api.K3sNode{}, Error: err.Error()}
	}
	return api.K3sStatus{Reachable: true, Nodes: nodes}
}

type cephStatus struct {
	Health struct {
		Status string `json:"status"`
		Checks map[string]struct {
			Severity string `json:"severity"`
			Summary  struct {
				Message string `json:"message"`
			} `json:"summary"`
		} `json:"checks"`
	} `json:"health"`
	OSDMap struct {
		NumOSDs   int `json:"num_osds"`
		NumUpOSDs int `json:"num_up_osds"`
		NumInOSDs int `json:"num_in_osds"`
	} `json:"osdmap"`
	PGMap struct {
		NumPGs     int `json:"num_pgs"`
		PGsByState []struct {
			StateName string `json:"state_name"`
			Count     int    `json:"count"`
		} `json:"pgs_by_state"`
	} `json:"pgmap"`
}

// Run refreshes the Ceph cache every CephRefresh until ctx ends.
func (c *Collector) Run(ctx context.Context) {
	t := time.NewTicker(CephRefresh)
	defer t.Stop()
	for {
		c.refreshCeph(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// cephCached answers from the cache. Before the first refresh, or when the
// cache is stale, it waits for a refresh instead.
func (c *Collector) cephCached(ctx context.Context) api.CephStatus {
	c.cephMu.Lock()
	v := c.cephVal
	c.cephMu.Unlock()
	if !v.AsOf.IsZero() && c.now().Sub(v.AsOf) < cephStale {
		return v
	}
	return c.refreshCeph(ctx)
}

// refreshCeph asks Ceph once. Callers that arrive while a refresh is running
// wait for that one instead of starting their own.
func (c *Collector) refreshCeph(ctx context.Context) api.CephStatus {
	c.cephMu.Lock()
	if ch := c.cephInflight; ch != nil {
		c.cephMu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
		}
		c.cephMu.Lock()
		defer c.cephMu.Unlock()
		return c.cephVal
	}
	ch := make(chan struct{})
	c.cephInflight = ch
	c.cephMu.Unlock()

	// Detached from the caller: a cancelled request must not leave the cache empty.
	v := c.ceph(context.Background())
	v.AsOf = c.now().UTC()

	c.cephMu.Lock()
	c.cephVal = v
	c.cephInflight = nil
	c.cephMu.Unlock()
	close(ch)
	return v
}

func (c *Collector) ceph(ctx context.Context) api.CephStatus {
	cc := c.cfg.Ceph
	if cc.Keyring == "" || cc.ID == "" {
		return api.CephStatus{Health: "unknown", Checks: []string{}, Error: "not configured"}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := c.run(ctx, cc.Command, "--id", cc.ID, "--keyring", cc.Keyring,
		"--connect-timeout", "5", "status", "--format", "json")
	if err != nil {
		return api.CephStatus{Health: "unknown", Checks: []string{}, Error: err.Error()}
	}
	return parseCeph(out)
}

func parseCeph(out []byte) api.CephStatus {
	var cs cephStatus
	if err := json.Unmarshal(out, &cs); err != nil {
		return api.CephStatus{Health: "unknown", Checks: []string{}, Error: "ceph output: " + err.Error()}
	}
	s := api.CephStatus{
		Health:    cs.Health.Status,
		Checks:    []string{},
		OSDsTotal: cs.OSDMap.NumOSDs,
		OSDsUp:    cs.OSDMap.NumUpOSDs,
		OSDsIn:    cs.OSDMap.NumInOSDs,
		PGsTotal:  cs.PGMap.NumPGs,
	}
	for name, chk := range cs.Health.Checks {
		s.Checks = append(s.Checks, fmt.Sprintf("%s %s: %s", chk.Severity, name, chk.Summary.Message))
	}
	sort.Strings(s.Checks)
	for _, p := range cs.PGMap.PGsByState {
		if p.StateName == "active+clean" {
			s.PGsActiveClean = p.Count
		}
	}
	return s
}
