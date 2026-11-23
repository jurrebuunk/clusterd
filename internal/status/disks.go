package status

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jurrebuunk/clusterd/internal/api"
)

// Filesystems worth reporting; pseudo and network filesystems are left out.
const fsTypes = "ext4|xfs|btrfs|vfat|f2fs|zfs"

// The OSD view comes from two slow Ceph CLI calls, so it is cached longer than
// the health status: disks fill slowly.
const osdRefresh = 60 * time.Second

type osdInfo struct {
	at   time.Time
	osds []api.OSD
	err  error
}

type cephOSDDF struct {
	Nodes []struct {
		ID          int     `json:"id"`
		DeviceClass string  `json:"device_class"`
		KB          int64   `json:"kb"`
		KBUsed      int64   `json:"kb_used"`
		Utilization float64 `json:"utilization"`
		Status      string  `json:"status"`
		Reweight    float64 `json:"reweight"`
	} `json:"nodes"`
}

type cephOSDMeta struct {
	ID       int    `json:"id"`
	Hostname string `json:"hostname"`
	Devices  string `json:"devices"`
}

func (c *Collector) fetchOSDs(ctx context.Context) ([]api.OSD, error) {
	cc := c.cfg.Ceph
	if cc.Keyring == "" || cc.ID == "" {
		return []api.OSD{}, fmt.Errorf("ceph not configured")
	}
	args := []string{"--id", cc.ID, "--keyring", cc.Keyring, "--connect-timeout", "5"}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	dfOut, err := c.run(ctx, cc.Command, append(args, "osd", "df", "--format", "json")...)
	if err != nil {
		return nil, err
	}
	metaOut, err := c.run(ctx, cc.Command, append(args, "osd", "metadata", "--format", "json")...)
	if err != nil {
		return nil, err
	}
	return parseOSDs(dfOut, metaOut)
}

func parseOSDs(dfOut, metaOut []byte) ([]api.OSD, error) {
	var df cephOSDDF
	if err := json.Unmarshal(dfOut, &df); err != nil {
		return nil, fmt.Errorf("osd df: %w", err)
	}
	var meta []cephOSDMeta
	if err := json.Unmarshal(metaOut, &meta); err != nil {
		return nil, fmt.Errorf("osd metadata: %w", err)
	}
	byID := map[int]cephOSDMeta{}
	for _, m := range meta {
		byID[m.ID] = m
	}
	out := []api.OSD{}
	for _, n := range df.Nodes {
		m := byID[n.ID]
		o := api.OSD{
			ID: n.ID, Host: m.Hostname, Device: m.Devices, DeviceClass: n.DeviceClass,
			SizeBytes: n.KB * 1024, UsedBytes: n.KBUsed * 1024, UsedPercent: round1(n.Utilization),
			Up: n.Status == "up", In: n.Reweight > 0,
		}
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// osds answers from a cache refreshed every osdRefresh, merging concurrent
// refreshes like the Ceph health cache does.
func (c *Collector) osds(ctx context.Context) ([]api.OSD, error) {
	c.osdMu.Lock()
	v := c.osdVal
	if !v.at.IsZero() && c.now().Sub(v.at) < osdRefresh {
		c.osdMu.Unlock()
		return v.osds, v.err
	}
	if ch := c.osdInflight; ch != nil {
		c.osdMu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		c.osdMu.Lock()
		defer c.osdMu.Unlock()
		return c.osdVal.osds, c.osdVal.err
	}
	ch := make(chan struct{})
	c.osdInflight = ch
	c.osdMu.Unlock()

	osds, err := c.fetchOSDs(context.Background())
	c.osdMu.Lock()
	c.osdVal = osdInfo{at: c.now(), osds: osds, err: err}
	c.osdInflight = nil
	c.osdMu.Unlock()
	close(ch)
	return osds, err
}

// Disks gathers physical disks, filesystems and OSDs.
func (c *Collector) Disks(ctx context.Context) api.DisksStatus {
	st := api.DisksStatus{Disks: []api.Disk{}, Filesystems: []api.Filesystem{}, OSDs: []api.OSD{}}
	var mu sync.Mutex
	fail := func(what string, err error) {
		mu.Lock()
		st.Errors = append(st.Errors, what+": "+err.Error())
		mu.Unlock()
	}

	queries := map[string]string{
		"info":   `smartctl_device`,
		"size":   `smartctl_device_capacity_bytes`,
		"wear":   `smartctl_device_percentage_used`,
		"temp":   `smartctl_device_temperature`,
		"health": `smartctl_device_smart_status`,
		"hours":  `smartctl_device_power_on_seconds`,
		"errors": `smartctl_device_media_errors`,
		"fsSize": `node_filesystem_size_bytes{job=~"node|vm-node",fstype=~"` + fsTypes + `"}`,
		"fsFree": `node_filesystem_free_bytes{job=~"node|vm-node",fstype=~"` + fsTypes + `"}`,
		"fsAvl":  `node_filesystem_avail_bytes{job=~"node|vm-node",fstype=~"` + fsTypes + `"}`,
	}
	res := map[string][]sample{}
	var wg sync.WaitGroup
	for name, q := range queries {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			ss, err := c.query(ctx, q)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if name == "info" || name == "fsSize" {
					st.Errors = append(st.Errors, "metrics: "+err.Error())
				}
				return
			}
			res[name] = ss
		})
	}
	var osds []api.OSD
	wg.Go(func() {
		o, err := c.osds(ctx)
		if err != nil {
			fail("ceph osds", err)
			return
		}
		osds = o
	})
	wg.Wait()
	if osds != nil {
		st.OSDs = osds
	}

	st.Filesystems = filesystems(res["fsSize"], res["fsFree"], res["fsAvl"])

	size, wear, temp := byLabels(res["size"], "instance", "device"), byLabels(res["wear"], "instance", "device"), byLabels(res["temp"], "instance", "device")
	health, hours, mediaErr := byLabels(res["health"], "instance", "device"), byLabels(res["hours"], "instance", "device"), byLabels(res["errors"], "instance", "device")
	for _, s := range res["info"] {
		host, dev := s.Metric["instance"], s.Metric["device"]
		k := host + "/" + dev
		d := api.Disk{
			Host: host, Device: dev, Model: s.Metric["model_name"], Serial: s.Metric["serial_number"],
			SizeBytes: int64(size[k]), TemperatureC: temp[k], Healthy: health[k] == 1,
			PowerOnHours: int64(hours[k] / 3600),
		}
		switch {
		case s.Metric["protocol"] == "NVMe" || strings.HasPrefix(dev, "nvme"):
			d.Type = "nvme"
		case strings.Contains(strings.ToLower(s.Metric["form_factor"]+s.Metric["model_family"]), "ssd"):
			d.Type = "ssd"
		default:
			d.Type = "hdd"
		}
		if w, ok := wear[k]; ok {
			d.WearPercent = &w
		}
		if e, ok := mediaErr[k]; ok {
			n := int64(e)
			d.MediaErrors = &n
		}
		fillDiskUse(&d, st.OSDs, st.Filesystems)
		st.Disks = append(st.Disks, d)
	}
	sort.Slice(st.Disks, func(i, j int) bool {
		if st.Disks[i].Host != st.Disks[j].Host {
			return st.Disks[i].Host < st.Disks[j].Host
		}
		return st.Disks[i].Device < st.Disks[j].Device
	})
	return st
}

// fillDiskUse says what a drive holds and how full it is: an OSD's usage, or
// the sum of the host's filesystems on its partitions.
func fillDiskUse(d *api.Disk, osds []api.OSD, fss []api.Filesystem) {
	for _, o := range osds {
		if o.Host == d.Host && o.Device != "" && slicesContains(strings.Split(o.Device, ","), d.Device) {
			d.Role = "osd." + strconv.Itoa(o.ID)
			d.UsedBytes = o.UsedBytes
			d.UsedPercent = o.UsedPercent
			return
		}
	}
	var used int64
	found := false
	for _, f := range fss {
		if f.Host == d.Host && strings.HasPrefix(f.Device, "/dev/"+d.Device) {
			found = true
			used += f.UsedBytes
			if f.Mountpoint == "/" {
				d.Role = "system"
			}
		}
	}
	if found {
		if d.Role == "" {
			d.Role = "data"
		}
		d.UsedBytes = used
		d.UsedPercent = percent(used, d.SizeBytes)
	}
}

func slicesContains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// filesystems builds one entry per (host, device): bind mounts of the same
// filesystem (/nix/store on /) show up once, under the shortest mountpoint.
func filesystems(size, free, avail []sample) []api.Filesystem {
	freeBy := byLabels(free, "instance", "device", "mountpoint")
	availBy := byLabels(avail, "instance", "device", "mountpoint")
	best := map[string]api.Filesystem{}
	for _, s := range size {
		host, dev, mp := s.Metric["instance"], s.Metric["device"], s.Metric["mountpoint"]
		k := host + "/" + dev
		if cur, ok := best[k]; ok && len(cur.Mountpoint) <= len(mp) {
			continue
		}
		key := host + "/" + dev + "/" + mp
		total := int64(s.Value)
		used := total - int64(freeBy[key])
		// Like df: the percentage leaves out space reserved for root.
		usable := used + int64(availBy[key])
		best[k] = api.Filesystem{Host: host, Mountpoint: mp, Device: dev, FSType: s.Metric["fstype"],
			SizeBytes: total, UsedBytes: used, UsedPercent: percent(used, usable)}
	}
	out := make([]api.Filesystem, 0, len(best))
	for _, f := range best {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Host != out[j].Host {
			return out[i].Host < out[j].Host
		}
		return out[i].Mountpoint < out[j].Mountpoint
	})
	return out
}
