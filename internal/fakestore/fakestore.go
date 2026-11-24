// Package fakestore serves canned answers for the monitoring store and the
// RGW admin API, shaped like the hgd1 cluster's, for tests.
package fakestore

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
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

// OpLogs are the audit and progress records the fake log store returns, as
// VictoriaLogs answers after unpack_logfmt: one finished restart, one failed
// stop, and a start that is still running (Time relative to OpNow).
var OpLogs = []map[string]string{
	{"_time": "2026-11-20T10:00:00Z", "host": "hv01", "msg": "audit", "action": "vm restart", "target": "w1", "token": "laptop", "request_id": "aaaaaaaaaaaaaaaa", "event": "start"},
	{"_time": "2026-11-20T10:00:05Z", "host": "hv01", "msg": "action progress", "action": "vm restart", "target": "w1", "token": "laptop", "request_id": "aaaaaaaaaaaaaaaa", "message": "node/w1 cordoned"},
	{"_time": "2026-11-20T10:00:40Z", "host": "hv01", "msg": "audit", "action": "vm restart", "target": "w1", "token": "laptop", "request_id": "aaaaaaaaaaaaaaaa", "event": "end", "result": "ok"},
	{"_time": "2026-11-20T11:00:00Z", "host": "hv02", "msg": "audit", "action": "vm stop", "target": "m1", "token": "laptop", "request_id": "bbbbbbbbbbbbbbbb", "event": "start"},
	{"_time": "2026-11-20T11:00:09Z", "host": "hv02", "msg": "audit", "action": "vm stop", "target": "m1", "token": "laptop", "request_id": "bbbbbbbbbbbbbbbb", "event": "end", "result": "error", "error": "drain timed out"},
	{"_time": "2026-11-20T11:59:00Z", "host": "hv01", "msg": "audit", "action": "host resume", "target": "hv01", "token": "ci", "request_id": "cccccccccccccccc", "event": "start"},
}

// Store is a fake VictoriaMetrics (/api/v1/query) and VictoriaLogs
// (/select/logsql/query, answering OpLogs).
func Store() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/select/logsql/query" {
			q := r.URL.Query().Get("query")
			enc := json.NewEncoder(w)
			for _, rec := range OpLogs {
				if strings.Contains(q, `"request_id=`) && !strings.Contains(q, `"request_id=`+rec["request_id"]+`"`) {
					continue
				}
				if strings.Contains(q, "target:=") && !strings.Contains(q, `target:="`+rec["target"]+`"`) {
					continue
				}
				enc.Encode(rec)
			}
			return
		}
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

// FakeRGW is a gateway with an admin API and enough of the S3 API to create
// a bucket: users app and jurre (plus the API's own clusterd), buckets
// backups (app, 40 objects, 4 GB quota) and media (jurre). It checks only
// that a SigV4 header is present.
type FakeRGW struct {
	*httptest.Server
	mu      sync.Mutex
	users   map[string]*fakeUser
	buckets map[string]*fakeBucket
	keyN    int
}

type fakeUser struct {
	keys []map[string]string
}

type fakeBucket struct {
	owner, created string
	size, objects  int64
	multimeta      int64 // objects of an unfinished multipart upload
	quota          map[string]any
}

// RGW starts a FakeRGW.
func RGW() *FakeRGW {
	f := &FakeRGW{users: map[string]*fakeUser{}, buckets: map[string]*fakeBucket{}}
	for _, u := range []string{"app", "clusterd", "jurre"} {
		f.users[u] = &fakeUser{keys: []map[string]string{f.newKey(u)}}
	}
	f.buckets["backups"] = &fakeBucket{owner: "app", created: "2026-11-23T10:00:00.000000Z", size: 2000000000, objects: 40,
		quota: map[string]any{"enabled": true, "max_size": 4000000000, "max_objects": -1}}
	f.buckets["media"] = &fakeBucket{owner: "jurre", created: "2026-11-22T10:00:00.000000Z", size: 1000000000, objects: 10, multimeta: 1,
		quota: map[string]any{"enabled": false, "max_size": -1, "max_objects": -1}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

func (f *FakeRGW) newKey(uid string) map[string]string {
	f.keyN++
	return map[string]string{"user": uid, "access_key": fmt.Sprintf("AK%d", f.keyN), "secret_key": fmt.Sprintf("SK%d", f.keyN)}
}

// Bucket reports a bucket's owner and quota setting, for tests.
func (f *FakeRGW) Bucket(name string) (owner string, quota map[string]any, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.buckets[name]
	if !ok {
		return "", nil, false
	}
	return b.owner, b.quota, true
}

// UserKeys lists a user's access keys, for tests.
func (f *FakeRGW) UserKeys(uid string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	if u, ok := f.users[uid]; ok {
		for _, k := range u.keys {
			out = append(out, k["access_key"])
		}
	}
	return out
}

func (f *FakeRGW) serve(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	q := r.URL.Query()
	w.Header().Set("Content-Type", "application/json")
	fail := func(code int, c string) {
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(map[string]string{"Code": c})
	}
	switch {
	case r.URL.Path == "/admin/bucket" && r.Method == http.MethodGet:
		out := []map[string]any{}
		for _, name := range slices.Sorted(maps.Keys(f.buckets)) {
			b := f.buckets[name]
			usage := map[string]any{"rgw.main": map[string]any{"size_actual": b.size, "num_objects": b.objects}}
			if b.multimeta > 0 {
				usage["rgw.multimeta"] = map[string]any{"size_actual": 0, "num_objects": b.multimeta}
			}
			out = append(out, map[string]any{"bucket": name, "owner": b.owner, "creation_time": b.created, "usage": usage, "bucket_quota": b.quota})
		}
		json.NewEncoder(w).Encode(out)
	case r.URL.Path == "/admin/bucket" && r.Method == http.MethodPut && q.Has("quota"):
		b, ok := f.buckets[q.Get("bucket")]
		if !ok || b.owner != q.Get("uid") {
			fail(http.StatusNotFound, "NoSuchBucket")
			return
		}
		var quota map[string]any
		json.NewDecoder(r.Body).Decode(&quota)
		b.quota = quota
	case r.URL.Path == "/admin/bucket" && r.Method == http.MethodDelete:
		b, ok := f.buckets[q.Get("bucket")]
		switch {
		case !ok:
			fail(http.StatusNotFound, "NoSuchBucket")
		case b.objects > 0 && q.Get("purge-objects") != "true":
			fail(http.StatusConflict, "BucketNotEmpty")
		default:
			delete(f.buckets, q.Get("bucket"))
		}
	case r.URL.Path == "/admin/metadata/user":
		json.NewEncoder(w).Encode(slices.Sorted(maps.Keys(f.users)))
	case r.URL.Path == "/admin/user" && q.Has("key"):
		u, ok := f.users[q.Get("uid")]
		if !ok {
			fail(http.StatusNotFound, "NoSuchUser")
			return
		}
		if r.Method == http.MethodPut {
			u.keys = append(u.keys, f.newKey(q.Get("uid")))
			json.NewEncoder(w).Encode(u.keys)
			return
		}
		u.keys = slices.DeleteFunc(u.keys, func(k map[string]string) bool { return k["access_key"] == q.Get("access-key") })
	case r.URL.Path == "/admin/user":
		uid := q.Get("uid")
		u, ok := f.users[uid]
		switch r.Method {
		case http.MethodGet:
			if !ok {
				fail(http.StatusNotFound, "NoSuchUser")
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"user_id": uid, "display_name": strings.ToUpper(uid), "suspended": 0,
				"keys": u.keys, "stats": map[string]any{"size_actual": 0, "num_objects": 0}})
		case http.MethodPut:
			if ok {
				fail(http.StatusConflict, "UserAlreadyExists")
				return
			}
			u = &fakeUser{keys: []map[string]string{f.newKey(uid)}}
			f.users[uid] = u
			json.NewEncoder(w).Encode(map[string]any{"user_id": uid, "display_name": q.Get("display-name"), "keys": u.keys})
		case http.MethodDelete:
			if !ok {
				fail(http.StatusNotFound, "NoSuchUser")
				return
			}
			delete(f.users, uid)
		}
	case r.Method == http.MethodPut && strings.Count(r.URL.Path, "/") == 1:
		// S3 CreateBucket, as the user whose access key signed it.
		ak, _, _ := strings.Cut(strings.TrimPrefix(auth, "AWS4-HMAC-SHA256 Credential="), "/")
		owner := ""
		for uid, u := range f.users {
			for _, k := range u.keys {
				if k["access_key"] == ak {
					owner = uid
				}
			}
		}
		name := strings.TrimPrefix(r.URL.Path, "/")
		switch {
		case owner == "":
			w.WriteHeader(http.StatusForbidden)
		case f.buckets[name] != nil:
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusConflict)
			fmt.Fprint(w, "<Error><Code>BucketAlreadyExists</Code></Error>")
		default:
			f.buckets[name] = &fakeBucket{owner: owner, created: "2026-11-24T10:00:00.000000Z",
				quota: map[string]any{"enabled": false, "max_size": -1, "max_objects": -1}}
		}
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// OSDDF and OSDMetadata are canned `ceph osd df` / `ceph osd metadata` JSON.
const (
	OSDDF       = `{"nodes":[{"id":0,"device_class":"hdd","name":"osd.0","kb":976762584,"kb_used":12582912,"utilization":1.29,"status":"up","reweight":1}]}`
	OSDMetadata = `[{"id":0,"hostname":"hv01","devices":"sda"}]`
)
