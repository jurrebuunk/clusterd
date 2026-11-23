// Package config is clusterd's configuration file (TOML). The NixOS module
// writes it; every field is documented in nix/module.nix.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"time"

	"github.com/BurntSushi/toml"
)

// Duration is a time.Duration written as "30s" or "1h".
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	d.Duration = v
	return err
}

func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// Host is one hypervisor running clusterd, this one included.
type Host struct {
	Name string `toml:"name"`
	// API is this host's own clusterd address, e.g. "https://10.10.20.12".
	API string `toml:"api"`
	// Metrics and Logs are the host's VictoriaMetrics and VictoriaLogs.
	Metrics string `toml:"metrics"`
	Logs    string `toml:"logs"`
	// RGW is the host's own Ceph object gateway, for the S3 admin API (optional).
	RGW string `toml:"rgw"`
}

// VIP is a floating address whose owner /v1/status reports.
type VIP struct {
	Name    string `toml:"name"`
	Address string `toml:"address"`
}

// VM is a VM from the inventory.
type VM struct {
	Name    string `toml:"name"`
	Host    string `toml:"host"`
	Enabled bool   `toml:"enabled"`
}

type Config struct {
	Site string `toml:"site"`
	// Host is this host's name; it must appear in Hosts.
	Host string `toml:"host"`
	// Listen is where the API listens; empty TLS means plain HTTP (dev only).
	Listen []string `toml:"listen"`
	// MetricsListen serves clusterd's own Prometheus metrics, unauthenticated.
	MetricsListen string `toml:"metrics_listen"`
	// ServerName is the API hostname; peers are verified against it.
	ServerName string `toml:"server_name"`
	TLS        struct {
		Cert string `toml:"cert"`
		Key  string `toml:"key"`
	} `toml:"tls"`
	TokensFile string `toml:"tokens_file"`

	Hosts []Host `toml:"hosts"`
	VIPs  []VIP  `toml:"vips"`
	VMs   []VM   `toml:"vms"`

	Kubernetes struct {
		Kubeconfig string `toml:"kubeconfig"`
	} `toml:"kubernetes"`
	Ceph struct {
		Command string `toml:"command"`
		ID      string `toml:"id"`
		Keyring string `toml:"keyring"`
	} `toml:"ceph"`

	S3 struct {
		// Endpoint is the public S3 address, shown in the status.
		Endpoint string `toml:"endpoint"`
		// Credentials is a JSON file {"access_key", "secret_key"} of an RGW user
		// with read caps on buckets, users, usage and metadata.
		Credentials string `toml:"credentials"`
		// DataPool is the Ceph pool with the object data.
		DataPool string `toml:"data_pool"`
	} `toml:"s3"`

	Limits struct {
		RequestTimeout Duration `toml:"request_timeout"`
		PeerTimeout    Duration `toml:"peer_timeout"`
		ProxyMaxBytes  int64    `toml:"proxy_max_bytes"`
		FollowMax      Duration `toml:"follow_max"`
		LogsLimit      int      `toml:"logs_limit"`
	} `toml:"limits"`
}

// Load reads and validates the file, filling in defaults.
func Load(path string) (*Config, error) {
	var c Config
	md, err := toml.DecodeFile(path, &c)
	if err != nil {
		return nil, err
	}
	if und := md.Undecoded(); len(und) > 0 {
		return nil, fmt.Errorf("%s: unknown keys %v", path, und)
	}
	c.defaults()
	return &c, c.Validate()
}

func (c *Config) defaults() {
	if c.Host == "" {
		c.Host, _ = os.Hostname()
	}
	if c.Ceph.Command == "" {
		c.Ceph.Command = "ceph"
	}
	if c.S3.DataPool == "" {
		c.S3.DataPool = "default.rgw.buckets.data"
	}
	l := &c.Limits
	if l.RequestTimeout.Duration == 0 {
		l.RequestTimeout.Duration = 30 * time.Second
	}
	if l.PeerTimeout.Duration == 0 {
		l.PeerTimeout.Duration = 3 * time.Second
	}
	if l.ProxyMaxBytes == 0 {
		l.ProxyMaxBytes = 64 << 20
	}
	if l.FollowMax.Duration == 0 {
		l.FollowMax.Duration = time.Hour
	}
	if l.LogsLimit == 0 {
		l.LogsLimit = 1000
	}
}

// Validate checks the config is usable.
func (c *Config) Validate() error {
	var errs []error
	if c.Site == "" {
		errs = append(errs, errors.New("site is required"))
	}
	if len(c.Listen) == 0 {
		errs = append(errs, errors.New("listen needs at least one address"))
	}
	if c.TokensFile == "" {
		errs = append(errs, errors.New("tokens_file is required"))
	}
	if (c.TLS.Cert == "") != (c.TLS.Key == "") {
		errs = append(errs, errors.New("tls.cert and tls.key go together"))
	}
	if c.Self() == nil {
		errs = append(errs, fmt.Errorf("host %q is not in hosts", c.Host))
	}
	seen := map[string]bool{}
	for _, h := range c.Hosts {
		if seen[h.Name] {
			errs = append(errs, fmt.Errorf("host %q listed twice", h.Name))
		}
		seen[h.Name] = true
		urls := map[string]string{"api": h.API, "metrics": h.Metrics, "logs": h.Logs}
		if h.RGW != "" {
			urls["rgw"] = h.RGW
		}
		for field, v := range urls {
			if u, err := url.Parse(v); err != nil || u.Scheme == "" || u.Host == "" {
				errs = append(errs, fmt.Errorf("host %q: %s must be an absolute URL", h.Name, field))
			}
		}
	}
	for _, vm := range c.VMs {
		if !seen[vm.Host] {
			errs = append(errs, fmt.Errorf("vm %q: unknown host %q", vm.Name, vm.Host))
		}
	}
	return errors.Join(errs...)
}

// Self is this host's entry.
func (c *Config) Self() *Host {
	for i := range c.Hosts {
		if c.Hosts[i].Name == c.Host {
			return &c.Hosts[i]
		}
	}
	return nil
}

// LocalFirst returns the hosts with this one first, the rest in config order.
func (c *Config) LocalFirst() []Host {
	out := make([]Host, 0, len(c.Hosts))
	if s := c.Self(); s != nil {
		out = append(out, *s)
	}
	for _, h := range c.Hosts {
		if h.Name != c.Host {
			out = append(out, h)
		}
	}
	return out
}
