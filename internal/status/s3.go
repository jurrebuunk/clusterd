package status

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/jurrebuunk/clusterd/internal/api"
	"github.com/jurrebuunk/clusterd/internal/s3admin"
)

// S3 reports the object store (gateways, users, totals, the data pool's room
// and the endpoint's certificate) and its buckets with usage and quotas.
func (c *Collector) S3(ctx context.Context) (api.S3Status, []api.S3Bucket) {
	st := api.S3Status{Endpoint: c.cfg.S3.Endpoint, Gateways: []api.S3Gateway{}, Pool: api.S3Pool{Name: c.cfg.S3.DataPool}}
	buckets := []api.S3Bucket{}
	var mu sync.Mutex
	fail := func(what string, err error) {
		mu.Lock()
		st.Errors = append(st.Errors, what+": "+err.Error())
		mu.Unlock()
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	var wg sync.WaitGroup

	if c.cfg.S3.Credentials == "" {
		st.Errors = append(st.Errors, "admin API: not configured")
	} else {
		cred, err := s3admin.LoadCredentials(c.cfg.S3.Credentials)
		if err != nil {
			st.Errors = append(st.Errors, "admin API: "+err.Error())
		} else {
			var gws []string
			for _, h := range c.cfg.LocalFirst() {
				if h.RGW != "" {
					gws = append(gws, h.RGW)
				}
			}
			cl := &s3admin.Client{Gateways: gws, Cred: cred, HTTP: c.plain, Now: c.now}
			wg.Go(func() {
				bs, err := cl.Buckets(ctx)
				if err != nil {
					fail("buckets", err)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				for _, b := range bs {
					bk := api.S3Bucket{Name: b.Bucket, Owner: b.Owner}
					for _, u := range b.Usage {
						bk.SizeBytes += u.SizeActual
						bk.Objects += u.NumObjects
					}
					if q := b.BucketQuota; q.Enabled {
						bk.QuotaBytes, bk.QuotaObjects = max(q.MaxSize, 0), max(q.MaxObjects, 0)
					}
					if t, err := time.Parse(time.RFC3339Nano, b.CreationTime); err == nil {
						bk.Created = &t
					}
					buckets = append(buckets, bk)
					st.Objects += bk.Objects
					st.SizeBytes += bk.SizeBytes
				}
			})
			wg.Go(func() {
				us, err := cl.Users(ctx)
				if err != nil {
					fail("users", err)
					return
				}
				mu.Lock()
				// The API's own read-only user doesn't count.
				for _, u := range us {
					if u != "clusterd" {
						st.Users++
					}
				}
				mu.Unlock()
			})
		}
	}

	metric := func(q string, f func([]sample)) {
		wg.Go(func() {
			ss, err := c.query(ctx, q)
			if err != nil {
				fail("metrics", err)
				return
			}
			mu.Lock()
			f(ss)
			mu.Unlock()
		})
	}
	// Every host's HAProxy checks every gateway; any one seeing it up is enough.
	metric(`max by (server) (haproxy_server_status{proxy="rgw",state="UP"})`, func(ss []sample) {
		for _, s := range ss {
			st.Gateways = append(st.Gateways, api.S3Gateway{Host: s.Metric["server"], Up: s.Value == 1})
		}
	})
	pool := `* on (pool_id) group_left (name) ceph_pool_metadata{name="` + c.cfg.S3.DataPool + `"}`
	metric(`ceph_pool_stored `+pool, func(ss []sample) {
		if len(ss) > 0 {
			st.Pool.StoredBytes = int64(ss[0].Value)
		}
	})
	metric(`ceph_pool_max_avail `+pool, func(ss []sample) {
		if len(ss) > 0 {
			st.Pool.MaxAvailBytes = int64(ss[0].Value)
		}
	})
	metric(`min(probe_ssl_earliest_cert_expiry{job="probe-s3"})`, func(ss []sample) {
		if len(ss) > 0 && ss[0].Value > 0 {
			t := time.Unix(int64(ss[0].Value), 0).UTC()
			st.CertExpires = &t
		}
	})
	wg.Wait()

	st.Pool.UsedPercent = percent(st.Pool.StoredBytes, st.Pool.StoredBytes+st.Pool.MaxAvailBytes)
	sort.Slice(buckets, func(i, j int) bool { return buckets[i].Name < buckets[j].Name })
	st.Buckets = len(buckets)
	sort.Slice(st.Gateways, func(i, j int) bool { return st.Gateways[i].Host < st.Gateways[j].Host })
	sort.Strings(st.Errors)
	return st, buckets
}
