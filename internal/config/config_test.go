package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sample = `
site = "hgd1"
host = "hv02"
listen = ["127.0.0.1:0"]
tokens_file = "/dev/null"

[[hosts]]
name = "hv01"
api = "https://10.0.0.1"
metrics = "http://10.0.0.1:8428"
logs = "http://10.0.0.1:9428"

[[hosts]]
name = "hv02"
api = "https://10.0.0.2"
metrics = "http://127.0.0.1:8428"
logs = "http://127.0.0.1:9428"

[[vms]]
name = "vm1"
host = "hv01"
enabled = true

[limits]
request_timeout = "10s"
`

func write(t *testing.T, s string) string {
	p := filepath.Join(t.TempDir(), "c.toml")
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad(t *testing.T) {
	c, err := Load(write(t, sample))
	if err != nil {
		t.Fatal(err)
	}
	if c.Limits.RequestTimeout.Seconds() != 10 || c.Limits.LogsLimit != 1000 {
		t.Fatalf("limits: %+v", c.Limits)
	}
	if lf := c.LocalFirst(); lf[0].Name != "hv02" || lf[1].Name != "hv01" {
		t.Fatalf("LocalFirst = %+v", lf)
	}
}

func TestLoadRejects(t *testing.T) {
	for name, s := range map[string]string{
		"unknown key":  sample + "\nbogus = 1\n",
		"unknown host": strings.Replace(sample, `host = "hv02"`, `host = "hv09"`, 1),
		"vm host": strings.Replace(sample, `host = "hv01"
enabled`, `host = "hv07"
enabled`, 1),
		"bad url": strings.Replace(sample, `api = "https://10.0.0.1"`, `api = "10.0.0.1"`, 1),
	} {
		if _, err := Load(write(t, s)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
