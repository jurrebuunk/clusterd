package status

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jurrebuunk/clusterd/internal/api"

	"github.com/jurrebuunk/clusterd/internal/config"
	"github.com/jurrebuunk/clusterd/internal/fakestore"
)

func fakeCollector(t *testing.T) *Collector {
	store, rgw := fakestore.Store(), fakestore.RGW()
	t.Cleanup(store.Close)
	t.Cleanup(rgw.Close)
	cred := filepath.Join(t.TempDir(), "s3")
	os.WriteFile(cred, []byte(`{"access_key":"AK","secret_key":"SK"}`), 0o600)
	cfg := &config.Config{Site: "lab", Host: "hv01", Hosts: []config.Host{{Name: "hv01", API: "http://x", Metrics: store.URL, Logs: store.URL, RGW: rgw.URL}}}
	cfg.Ceph.Command, cfg.Ceph.ID, cfg.Ceph.Keyring = "ceph", "clusterd", "/k"
	cfg.S3.Endpoint, cfg.S3.Credentials, cfg.S3.DataPool = "https://s3.test", cred, "default.rgw.buckets.data"
	c := New(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "osd df"):
			return []byte(fakestore.OSDDF), nil
		case strings.Contains(joined, "osd metadata"):
			return []byte(fakestore.OSDMetadata), nil
		case strings.Contains(joined, "osd dump"):
			return []byte(`{"crush_node_flags":{"hv01":["noout"]}}`), nil
		case strings.Contains(joined, "status"):
			return []byte(cephJSON), nil
		}
		return nil, errors.New("unexpected " + joined)
	}
	return c
}

func TestDisks(t *testing.T) {
	d := fakeCollector(t).Disks(context.Background())
	if len(d.Errors) > 0 {
		t.Fatalf("errors: %v", d.Errors)
	}
	if len(d.Disks) != 2 {
		t.Fatalf("disks = %+v", d.Disks)
	}
	nvme, hdd := d.Disks[0], d.Disks[1]
	// System disk: used = / (50 GB) + /boot (0.1 GB), /nix/store not counted twice.
	if nvme.Role != "system" || nvme.Type != "nvme" || nvme.UsedBytes != 50_100_000_000 || nvme.UsedPercent != 19.6 ||
		nvme.WearPercent == nil || *nvme.WearPercent != 47 || nvme.TemperatureC != 41 || !nvme.Healthy || nvme.PowerOnHours != 10 {
		t.Errorf("nvme = %+v", nvme)
	}
	if hdd.Role != "osd.0" || hdd.Type != "hdd" || hdd.UsedPercent != 1.3 || hdd.WearPercent != nil || hdd.UsedBytes != 12582912*1024 {
		t.Errorf("hdd = %+v", hdd)
	}
	if len(d.Filesystems) != 3 {
		t.Fatalf("filesystems = %+v", d.Filesystems)
	}
	root := d.Filesystems[0]
	// df-style percentage: used / (used + avail) = 50 / 190.
	if root.Mountpoint != "/" || root.UsedBytes != 50e9 || root.UsedPercent != 26.3 {
		t.Errorf("root = %+v", root)
	}
	if vm := d.Filesystems[2]; vm.Host != "vm1" || vm.Mountpoint != "/var/lib" {
		t.Errorf("vm fs = %+v", vm)
	}
	if len(d.OSDs) != 1 || !d.OSDs[0].Up || !d.OSDs[0].In || d.OSDs[0].Host != "hv01" {
		t.Errorf("osds = %+v", d.OSDs)
	}
}

func TestOSDCache(t *testing.T) {
	c := fakeCollector(t)
	calls := 0
	inner := c.run
	c.run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		calls++
		return inner(ctx, name, args...)
	}
	c.osds(context.Background())
	c.osds(context.Background())
	if calls != 3 { // osd df, osd metadata and osd dump, once
		t.Fatalf("ceph ran %d times for two lookups, want 3", calls)
	}
	if !c.maintenance(context.Background())["hv01"] {
		t.Fatal("per-host noout not seen as maintenance")
	}
}

func TestS3(t *testing.T) {
	st, buckets := fakeCollector(t).S3(context.Background())
	if len(st.Errors) > 0 {
		t.Fatalf("errors: %v", st.Errors)
	}
	if st.Users != 2 || st.Buckets != 2 || len(buckets) != 2 || st.Objects != 51 || st.SizeBytes != 3e9 {
		t.Fatalf("s3 = %+v", st)
	}
	b := buckets[0]
	if b.Name != "backups" || b.QuotaBytes != 4e9 || b.QuotaObjects != 0 || b.Created == nil {
		t.Errorf("bucket = %+v", b)
	}
	if buckets[1].QuotaBytes != 0 {
		t.Errorf("disabled quota counted: %+v", buckets[1])
	}
	if len(st.Gateways) != 2 || !st.Gateways[0].Up || st.Gateways[1].Up {
		t.Errorf("gateways = %+v", st.Gateways)
	}
	if st.Pool.StoredBytes != 3e9 || st.Pool.MaxAvailBytes != 297e9 || st.Pool.UsedPercent != 1 {
		t.Errorf("pool = %+v", st.Pool)
	}
	if st.CertExpires == nil || st.CertExpires.Year() != 2027 {
		t.Errorf("cert = %v", st.CertExpires)
	}
}

func TestS3NotConfigured(t *testing.T) {
	c := fakeCollector(t)
	c.cfg.S3.Credentials = ""
	st, _ := c.S3(context.Background())
	if len(st.Errors) != 1 || !strings.Contains(st.Errors[0], "not configured") || len(st.Gateways) != 2 {
		t.Fatalf("s3 = %+v", st)
	}
}

func TestChecks(t *testing.T) {
	c := fakeCollector(t)
	c.procRoot = fakeProc(t)
	ch := c.Checks(context.Background(), "tok")
	got := map[string]string{}
	msg := map[string]string{}
	for _, x := range ch.Checks {
		got[x.Name], msg[x.Name] = x.Status, x.Message
	}
	want := map[string]string{
		"hosts":      "warn", // hv01 in maintenance (per-host noout)
		"units":      "fail", // broken.service
		"vms":        "ok",
		"nodes":      "fail", // no Kubernetes access in this test
		"ceph":       "warn", // HEALTH_WARN, 1 osds down
		"vips":       "ok",
		"disks":      "ok",
		"s3":         "warn", // gateway hv02 down
		"tunnel":     "ok",
		"monitoring": "ok",
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s: %s (%s), want %s", k, got[k], msg[k], w)
		}
	}
	if ch.Status != "fail" || len(ch.Checks) != len(want) {
		t.Errorf("overall %s with %d checks", ch.Status, len(ch.Checks))
	}
	if !strings.Contains(msg["hosts"], "hv01 in maintenance") || !strings.Contains(msg["units"], "hv01 broken.service") {
		t.Errorf("messages: %v", msg)
	}
}

func TestOperations(t *testing.T) {
	c := fakeCollector(t)
	c.now = func() time.Time { return time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC) }
	ops, err := c.Operations(context.Background(), 24*time.Hour, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 3 {
		t.Fatalf("ops = %+v", ops)
	}
	// Newest first; events only on a single operation.
	if ops[0].ID != "cccccccccccccccc" || ops[0].State != api.OpRunning || ops[0].Finished != nil {
		t.Errorf("running = %+v", ops[0])
	}
	if ops[1].State != api.OpFailed || ops[1].Error != "drain timed out" || ops[1].Host != "hv02" {
		t.Errorf("failed = %+v", ops[1])
	}
	if ops[2].State != api.OpSucceeded || ops[2].Action != "vm restart" || ops[2].Events != nil {
		t.Errorf("ok = %+v", ops[2])
	}

	op, err := c.Operation(context.Background(), "aaaaaaaaaaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	if len(op.Events) != 2 || op.Events[0].Message != "node/w1 cordoned" || op.Events[1].Status != api.EventDone || op.Finished == nil {
		t.Errorf("op = %+v", op)
	}
	if _, err := c.Operation(context.Background(), "../etc"); err != ErrNoOperation {
		t.Errorf("bad id: %v", err)
	}

	// A start older than an action may run without an end is lost.
	c.now = func() time.Time { return time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC) }
	ops, _ = c.Operations(context.Background(), 24*time.Hour, "hv01", 0)
	if len(ops) != 1 || ops[0].State != api.OpUnknown {
		t.Errorf("target filter / unknown = %+v", ops)
	}
}
