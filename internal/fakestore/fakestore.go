// Package fakestore serves canned answers for the monitoring store and the
// RGW admin API, shaped like the hgd1 cluster's, for tests.
package fakestore

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
)

type series struct {
	metric map[string]string
	value  string
}

func s(value string, kv ...string) series {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return series{m, value}
}

// answers maps a substring of the PromQL query to its result; the first
// matching key wins, so more specific keys come first.
var answers = []struct {
	match  string
	result []series
}{
	{"smartctl_device_capacity_bytes", []series{
		s("256060514304", "instance", "hv01", "device", "nvme0"),
		s("1000204886016", "instance", "hv01", "device", "sda"),
	}},
	{"smartctl_device_percentage_used", []series{s("47", "instance", "hv01", "device", "nvme0")}},
	{"smartctl_device_temperature", []series{s("41", "instance", "hv01", "device", "nvme0"), s("33", "instance", "hv01", "device", "sda")}},
	{"smartctl_device_smart_status", []series{s("1", "instance", "hv01", "device", "nvme0"), s("1", "instance", "hv01", "device", "sda")}},
	{"smartctl_device_power_on_seconds", []series{s("36000", "instance", "hv01", "device", "nvme0"), s("7200", "instance", "hv01", "device", "sda")}},
	{"smartctl_device_media_errors", []series{s("0", "instance", "hv01", "device", "nvme0")}},
	{"smartctl_device", []series{
		s("1", "instance", "hv01", "device", "nvme0", "model_name", "SKhynix 256", "serial_number", "N1", "protocol", "NVMe"),
		s("1", "instance", "hv01", "device", "sda", "model_name", "HGST 1TB", "serial_number", "H1", "protocol", "ATA", "form_factor", "2.5 inches"),
	}},
	{"node_filesystem_size_bytes", []series{
		s("200000000000", "instance", "hv01", "device", "/dev/nvme0n1p2", "mountpoint", "/", "fstype", "ext4"),
		s("200000000000", "instance", "hv01", "device", "/dev/nvme0n1p2", "mountpoint", "/nix/store", "fstype", "ext4"),
		s("1000000000", "instance", "hv01", "device", "/dev/nvme0n1p1", "mountpoint", "/boot", "fstype", "vfat"),
		s("60000000000", "instance", "vm1", "device", "/dev/vda", "mountpoint", "/var/lib", "fstype", "ext4"),
	}},
	{"node_filesystem_free_bytes", []series{
		s("150000000000", "instance", "hv01", "device", "/dev/nvme0n1p2", "mountpoint", "/"),
		s("150000000000", "instance", "hv01", "device", "/dev/nvme0n1p2", "mountpoint", "/nix/store"),
		s("900000000", "instance", "hv01", "device", "/dev/nvme0n1p1", "mountpoint", "/boot"),
		s("45000000000", "instance", "vm1", "device", "/dev/vda", "mountpoint", "/var/lib"),
	}},
	{"node_filesystem_avail_bytes", []series{
		s("140000000000", "instance", "hv01", "device", "/dev/nvme0n1p2", "mountpoint", "/"),
		s("140000000000", "instance", "hv01", "device", "/dev/nvme0n1p2", "mountpoint", "/nix/store"),
		s("900000000", "instance", "hv01", "device", "/dev/nvme0n1p1", "mountpoint", "/boot"),
		s("42000000000", "instance", "vm1", "device", "/dev/vda", "mountpoint", "/var/lib"),
	}},
	{"haproxy_server_status", []series{s("1", "server", "hv01"), s("0", "server", "hv02")}},
	{"ceph_pool_stored", []series{s("3000000000", "pool_id", "8", "name", "default.rgw.buckets.data")}},
	{"ceph_pool_max_avail", []series{s("297000000000", "pool_id", "8", "name", "default.rgw.buckets.data")}},
	{"probe_ssl_earliest_cert_expiry", []series{s("1803167241")}},
	{"cloudflared_tunnel_ha_connections", []series{s("4", "instance", "hv01")}},
	{"node_systemd_unit_state", []series{s("1", "instance", "hv01", "name", "broken.service")}},
}

// Store is a fake VictoriaMetrics: /health and /api/v1/query.
func Store() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" {
			return // /health and anything else: 200, empty
		}
		q := r.URL.Query().Get("query")
		result := []map[string]any{}
		for _, a := range answers {
			if strings.Contains(q, a.match) {
				for _, s := range a.result {
					result = append(result, map[string]any{"metric": s.metric, "value": []any{1.7e9, s.value}})
				}
				break
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": result}})
	}))
}

// RGW is a fake gateway admin API with two buckets and two users (plus the
// API's own user). It checks only that a SigV4 header is present.
func RGW() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/admin/bucket":
			json.NewEncoder(w).Encode([]map[string]any{
				{"bucket": "backups", "owner": "app", "creation_time": "2026-11-23T10:00:00.000000Z",
					"usage":        map[string]any{"rgw.main": map[string]any{"size_actual": 2000000000, "num_objects": 40}},
					"bucket_quota": map[string]any{"enabled": true, "max_size": 4000000000, "max_objects": -1}},
				{"bucket": "media", "owner": "jurre", "creation_time": "2026-11-22T10:00:00.000000Z",
					"usage":        map[string]any{"rgw.main": map[string]any{"size_actual": 1000000000, "num_objects": 10}, "rgw.multimeta": map[string]any{"size_actual": 0, "num_objects": 1}},
					"bucket_quota": map[string]any{"enabled": false, "max_size": -1, "max_objects": -1}},
			})
		case "/admin/metadata/user":
			json.NewEncoder(w).Encode([]string{"app", "clusterd", "jurre"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// OSDDF and OSDMetadata are canned `ceph osd df` / `ceph osd metadata` JSON.
const (
	OSDDF       = `{"nodes":[{"id":0,"device_class":"hdd","name":"osd.0","kb":976762584,"kb_used":12582912,"utilization":1.29,"status":"up","reweight":1}]}`
	OSDMetadata = `[{"id":0,"hostname":"hv01","devices":"sda"}]`
)
