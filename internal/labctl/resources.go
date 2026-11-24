package labctl

import (
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/jurrebuunk/clusterd/internal/api"
)

// listCmd is a read-only "list" for a collection.
func listCmd[T any](e *env, short, path string, name func(T) string, table func(l api.List[T]) error) *cobra.Command {
	return &cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: short, Args: noArgs,
		RunE: func(*cobra.Command, []string) error {
			var l api.List[T]
			if err := e.get(path, &l); err != nil {
				return err
			}
			return e.render(l, namesOf(l.Items, name), func() error { return table(l) })
		},
	}
}

func cmdPod(e *env) *cobra.Command {
	c := group("pod", "Kubernetes pods")
	var failing bool
	list := &cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "Pods of every namespace; --failing for the ones that need a look", Args: noArgs,
		RunE: func(*cobra.Command, []string) error {
			path := "/v1/pods"
			if failing {
				path += "?failing=true"
			}
			var l api.List[api.Pod]
			if err := e.get(path, &l); err != nil {
				return err
			}
			return e.render(l, namesOf(l.Items, func(p api.Pod) string { return p.Namespace + "/" + p.Name }), func() error {
				warnAll(e, "pods", l.Errors)
				tw := newTable(e.stdout)
				fmt.Fprintln(tw, "NAMESPACE\tPOD\tREADY\tSTATUS\tRESTARTS\tAGE\tNODE")
				now := time.Now()
				for _, p := range l.Items {
					status := p.Phase
					if p.Reason != "" {
						status = p.Reason
					}
					restarts := strconv.Itoa(p.Restarts)
					if p.LastRestart != nil {
						restarts += " (" + age(now.Sub(*p.LastRestart)) + " ago)"
					}
					fmt.Fprintf(tw, "%s\t%s\t%d/%d\t%s\t%s\t%s\t%s\n", p.Namespace, p.Name, p.Ready, p.Containers, status, restarts, age(now.Sub(p.Created)), p.Node)
				}
				return tw.Flush()
			})
		},
	}
	list.Flags().BoolVar(&failing, "failing", false, "only pods that crash, wait, can't pull or restarted in the last hour")
	c.AddCommand(list)
	return c
}

func cmdCeph(e *env) *cobra.Command {
	c := group("ceph", "Ceph: health and capacity")
	c.AddCommand(&cobra.Command{
		Use: "status", Short: "Ceph health, OSDs, placement groups and raw capacity", Args: noArgs,
		RunE: func(*cobra.Command, []string) error {
			var cp api.CephStatus
			if err := e.get("/v1/ceph", &cp); err != nil {
				return err
			}
			return e.render(cp, nil, func() error {
				tw := newTable(e.stdout)
				printCeph(tw, e.stderr, cp, time.Now())
				return tw.Flush()
			})
		},
	})
	return c
}

func cmdOSD(e *env) *cobra.Command {
	c := group("osd", "Ceph OSDs")
	c.AddCommand(listCmd(e, "OSDs: host, device, size, usage, up and in", "/v1/ceph/osds",
		func(o api.OSD) string { return "osd." + strconv.Itoa(o.ID) },
		func(l api.List[api.OSD]) error {
			warnAll(e, "osds", l.Errors)
			tw := newTable(e.stdout)
			fmt.Fprintln(tw, "OSD\tHOST\tDEVICE\tCLASS\tSIZE\tUSED\tFULL\tSTATUS")
			for _, o := range l.Items {
				st := map[bool]string{true: "Up", false: "Down"}[o.Up] + map[bool]string{true: ",In", false: ",Out"}[o.In]
				fmt.Fprintf(tw, "osd.%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", o.ID, o.Host, o.Device, o.DeviceClass,
					bytesHuman(o.SizeBytes), bytesHuman(o.UsedBytes), pct(o.UsedPercent), st)
			}
			return tw.Flush()
		}))
	return c
}

func cmdDisk(e *env) *cobra.Command {
	c := group("disk", "Physical disks")
	c.AddCommand(listCmd(e, "Disks: role, size, usage, wear, temperature, SMART", "/v1/disks",
		func(d api.Disk) string { return d.Host + "/" + d.Device },
		func(l api.List[api.Disk]) error {
			warnAll(e, "disks", l.Errors)
			tw := newTable(e.stdout)
			printDisks(tw, e.stderr, l.Items)
			return tw.Flush()
		}))
	return c
}

func cmdFilesystem(e *env) *cobra.Command {
	c := group("filesystem", "Filesystems of hosts and VMs", "fs")
	c.AddCommand(listCmd(e, "Filesystems: size, usage, how full", "/v1/filesystems",
		func(f api.Filesystem) string { return f.Host + ":" + f.Mountpoint },
		func(l api.List[api.Filesystem]) error {
			warnAll(e, "filesystems", l.Errors)
			tw := newTable(e.stdout)
			printFilesystems(tw, e.stderr, l.Items)
			return tw.Flush()
		}))
	return c
}

func cmdVIP(e *env) *cobra.Command {
	c := group("vip", "Floating addresses")
	c.AddCommand(listCmd(e, "VIPs and which host holds each", "/v1/vips",
		func(v api.VIPStatus) string { return v.Name },
		func(l api.List[api.VIPStatus]) error {
			tw := newTable(e.stdout)
			// Owners we can't see are reported by host list; here only VIPs.
			printVIPs(tw, e.stderr, l.Items, false)
			return tw.Flush()
		}))
	return c
}

func cmdS3(e *env) *cobra.Command {
	c := group("s3", "The S3 object store and its buckets")
	c.AddCommand(&cobra.Command{
		Use: "status", Short: "Gateways, users, buckets, what's stored, room left, certificate", Args: noArgs,
		RunE: func(*cobra.Command, []string) error {
			var s api.S3Status
			if err := e.get("/v1/s3", &s); err != nil {
				return err
			}
			return e.render(s, nil, func() error {
				tw := newTable(e.stdout)
				printS3(tw, e.stderr, s, time.Now())
				return tw.Flush()
			})
		},
	})
	b := group("bucket", "Buckets")
	b.AddCommand(listCmd(e, "Buckets with owner, size and quota", "/v1/s3/buckets",
		func(b api.S3Bucket) string { return b.Name },
		func(l api.List[api.S3Bucket]) error {
			warnAll(e, "s3", l.Errors)
			tw := newTable(e.stdout)
			printBuckets(tw, e.stderr, l.Items)
			return tw.Flush()
		}))
	b.AddCommand(&cobra.Command{
		Use: "show BUCKET", Short: "One bucket", Args: args(1, "BUCKET"), ValidArgsFunction: e.complete("/v1/s3/buckets"),
		RunE: func(_ *cobra.Command, a []string) error {
			var bk api.S3Bucket
			if err := e.get("/v1/s3/buckets/"+url.PathEscape(a[0]), &bk); err != nil {
				return err
			}
			return e.render(bk, []string{bk.Name}, func() error {
				quota, objQuota, created := "-", "-", "-"
				if bk.QuotaBytes > 0 {
					quota = fmt.Sprintf("%s (%s full)", bytesHuman(bk.QuotaBytes), pct(percentOf(bk.SizeBytes, bk.QuotaBytes)))
				}
				if bk.QuotaObjects > 0 {
					objQuota = strconv.FormatInt(bk.QuotaObjects, 10)
				}
				if bk.Created != nil {
					created = bk.Created.Local().Format("2006-01-02 15:04")
				}
				return e.fields([][2]string{{"Name", bk.Name}, {"Owner", bk.Owner}, {"Objects", strconv.FormatInt(bk.Objects, 10)},
					{"Size", bytesHuman(bk.SizeBytes)}, {"Quota", quota}, {"Object quota", objQuota}, {"Created", created}})
			})
		},
	})
	c.AddCommand(b)
	return c
}

func warnAll(e *env, part string, errs []string) {
	for _, m := range errs {
		fmt.Fprintf(e.stderr, "warning: %s: %s\n", part, m)
	}
}
