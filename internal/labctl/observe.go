package labctl

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/jurrebuunk/clusterd/internal/api"
)

func cmdStatus(e *env) *cobra.Command {
	return &cobra.Command{
		Use: "status", Short: "The whole site at a glance (each part is also its own list)", Args: noArgs,
		RunE: func(*cobra.Command, []string) error {
			c, err := e.client()
			if err != nil {
				return err
			}
			st, err := c.Status(e.ctx)
			if err != nil {
				return err
			}
			return e.render(st, nil, func() error {
				printStatus(e.stdout, e.stderr, st, time.Now())
				return nil
			})
		},
	}
}

// cmdHealth prints the site's checks and exits ExitUnhealthy unless all are ok.
func cmdHealth(e *env) *cobra.Command {
	return &cobra.Command{
		Use: "health", Short: "One verdict per area; exit code 5 unless everything is ok", Args: noArgs,
		RunE: func(*cobra.Command, []string) error {
			var ch api.Checks
			if err := e.get("/v1/checks", &ch); err != nil {
				return err
			}
			err := e.render(ch, nil, func() error {
				tw := newTable(e.stdout)
				fmt.Fprintln(tw, "CHECK\tSTATUS\tMESSAGE")
				for _, x := range ch.Checks {
					fmt.Fprintf(tw, "%s\t%s\t%s\n", x.Name, x.Status, x.Message)
				}
				return tw.Flush()
			})
			if err != nil {
				return err
			}
			if ch.Status != api.CheckOK {
				return &exitError{ExitUnhealthy, fmt.Errorf("site is %s", map[string]string{api.CheckWarn: "degraded", api.CheckFail: "unhealthy"}[ch.Status])}
			}
			return nil
		},
	}
}

func newTable(w io.Writer) *tabwriter.Writer { return tabwriter.NewWriter(w, 0, 4, 2, ' ', 0) }

// printStatus prints every section as kubectl-style tables with a blank line
// between them; problems go to warn as "warning:" lines.
func printStatus(w, warn io.Writer, st api.Status, now time.Time) {
	tw := newTable(w)
	gap := func() { tw.Flush(); fmt.Fprintln(w) }
	unreachable := printHosts(tw, warn, st.Hosts)
	gap()
	printVMs(tw, st.VMs)
	gap()
	nodes := api.List[api.K3sNode]{Items: st.K3s.Nodes}
	if !st.K3s.Reachable {
		nodes.Errors = []string{"k3s API unreachable: " + shortError(st.K3s.Error)}
	}
	printNodes(tw, warn, nodes)
	gap()
	printCeph(tw, warn, st.Ceph, now)
	gap()
	printVIPs(tw, warn, st.VIPs, unreachable > 0)
	gap()
	for _, e := range st.Disks.Errors {
		fmt.Fprintf(warn, "warning: disks: %s\n", e)
	}
	printDisks(tw, warn, st.Disks.Disks)
	gap()
	printFilesystems(tw, warn, st.Disks.Filesystems)
	gap()
	printS3(tw, warn, st.S3, now)
	tw.Flush()
}

func printHosts(tw *tabwriter.Writer, warn io.Writer, hosts []api.HostStatus) (unreachable int) {
	fmt.Fprintln(tw, "HOST\tSTATUS\tCLUSTERD\tCOMMIT\tUPTIME\tLOAD\tMEM")
	revs := map[string]bool{}
	for _, h := range hosts {
		if !h.Reachable {
			unreachable++
			fmt.Fprintf(tw, "%s\tUnreachable\t-\t-\t-\t-\t-\n", h.Name)
			fmt.Fprintf(warn, "warning: %s: %s\n", h.Name, shortError(h.Error))
			continue
		}
		status := hostState(h)
		if h.Error != "" {
			fmt.Fprintf(warn, "warning: %s: %s\n", h.Name, h.Error)
		}
		rev := commit(h)
		if rev != "-" {
			revs[rev] = true
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%.2f\t%.0f%%\n", h.Name, status, h.Version, rev, age(time.Duration(h.UptimeSeconds)*time.Second), h.Load1, h.MemUsedPercent)
	}
	if len(revs) > 1 {
		fmt.Fprintln(warn, "warning: hosts run different commits; deploy the rest")
	}
	return unreachable
}

// hostState is Up, Unreachable, plus Maintenance and stores that are down.
func hostState(h api.HostStatus) string {
	if !h.Reachable {
		return "Unreachable"
	}
	status := "Up"
	if h.Maintenance {
		status += ",Maintenance"
	}
	if h.NixOS != nil && h.NixOS.RebootRequired {
		status += ",RebootNeeded"
	}
	for _, s := range h.Stores {
		if !s.OK {
			status += "," + map[string]string{"victoriametrics": "MetricsDown", "victorialogs": "LogsDown"}[s.Name]
		}
	}
	return status
}

// commit is the short git commit a host was built from, "-" when unknown.
func commit(h api.HostStatus) string {
	if h.NixOS == nil || h.NixOS.Revision == "" {
		return "-"
	}
	r, dirty := strings.CutSuffix(h.NixOS.Revision, "-dirty")
	if len(r) > 7 {
		r = r[:7]
	}
	if dirty {
		r += "-dirty"
	}
	return r
}

func printVMs(tw *tabwriter.Writer, vms []api.VMStatus) {
	fmt.Fprintln(tw, "VM\tHOST\tSTATUS")
	for _, vm := range vms {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", vm.Name, vm.Host, title(vm.State))
	}
}

func printNodes(tw *tabwriter.Writer, warn io.Writer, l api.List[api.K3sNode]) {
	for _, e := range l.Errors {
		fmt.Fprintf(warn, "warning: %s\n", e)
	}
	fmt.Fprintln(tw, "NODE\tSTATUS\tROLES\tVERSION\tZONE")
	for _, n := range l.Items {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", n.Name, nodeState(n), roles(n), n.Version, n.Zone)
	}
}

func nodeState(n api.K3sNode) string {
	status := "Ready"
	if !n.Ready {
		status = "NotReady"
	}
	if !n.Schedulable {
		status += ",SchedulingDisabled"
	}
	return status
}

func roles(n api.K3sNode) string {
	if len(n.Roles) == 0 {
		return "<none>"
	}
	return strings.Join(n.Roles, ",")
}

func printCeph(tw *tabwriter.Writer, warn io.Writer, cp api.CephStatus, now time.Time) {
	if cp.Error != "" {
		fmt.Fprintf(warn, "warning: ceph: %s\n", cp.Error)
	}
	for _, chk := range cp.Checks {
		fmt.Fprintf(warn, "warning: ceph %s\n", chk)
	}
	ago := "-"
	if !cp.AsOf.IsZero() {
		ago = age(now.Sub(cp.AsOf))
	}
	fmt.Fprintln(tw, "CEPH\tOSDS UP\tOSDS IN\tPGS CLEAN\tRAW USED\tRAW SIZE\tFULL\tAGE")
	fmt.Fprintf(tw, "%s\t%d/%d\t%d/%d\t%d/%d\t%s\t%s\t%s\t%s\n", cp.Health, cp.OSDsUp, cp.OSDsTotal, cp.OSDsIn, cp.OSDsTotal,
		cp.PGsActiveClean, cp.PGsTotal, bytesHuman(cp.BytesUsed), bytesHuman(cp.BytesTotal), pct(percentOf(cp.BytesUsed, cp.BytesTotal)), ago)
}

func printVIPs(tw *tabwriter.Writer, warn io.Writer, vips []api.VIPStatus, someUnreachable bool) {
	fmt.Fprintln(tw, "VIP\tADDRESS\tOWNER")
	for _, v := range vips {
		owner := strings.Join(v.Owners, ",")
		switch {
		case len(v.Owners) > 1:
			fmt.Fprintf(warn, "warning: VIP %s is on %d hosts (split brain)\n", v.Name, len(v.Owners))
		case len(v.Owners) == 0 && someUnreachable:
			owner = "<unknown>" // it may be on a host we can't ask
		case len(v.Owners) == 0:
			owner = "<none>"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", v.Name, v.Address, owner)
	}
}

func printDisks(tw *tabwriter.Writer, warn io.Writer, disks []api.Disk) {
	fmt.Fprintln(tw, "DISK\tHOST\tROLE\tTYPE\tSIZE\tUSED\tFULL\tWEAR\tTEMP\tSMART")
	for _, k := range disks {
		wear := "-"
		if k.WearPercent != nil {
			wear = pct(*k.WearPercent)
			if *k.WearPercent >= 80 {
				fmt.Fprintf(warn, "warning: %s %s is %s worn\n", k.Host, k.Device, wear)
			}
		}
		smart := "OK"
		if !k.Healthy {
			smart = "FAILING"
			fmt.Fprintf(warn, "warning: %s %s fails its SMART check\n", k.Host, k.Device)
		}
		role := k.Role
		if role == "" {
			role = "<none>"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%.0fC\t%s\n", k.Device, k.Host, role, k.Type,
			bytesHuman(k.SizeBytes), bytesHuman(k.UsedBytes), pct(k.UsedPercent), wear, k.TemperatureC, smart)
	}
}

func printFilesystems(tw *tabwriter.Writer, warn io.Writer, fss []api.Filesystem) {
	fmt.Fprintln(tw, "FILESYSTEM\tHOST\tDEVICE\tSIZE\tUSED\tFULL")
	for _, f := range fss {
		if f.UsedPercent >= 85 {
			fmt.Fprintf(warn, "warning: %s %s is %s full\n", f.Host, f.Mountpoint, pct(f.UsedPercent))
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", f.Mountpoint, f.Host, f.Device, bytesHuman(f.SizeBytes), bytesHuman(f.UsedBytes), pct(f.UsedPercent))
	}
}

func printS3(tw *tabwriter.Writer, warn io.Writer, s api.S3Status, now time.Time) {
	for _, e := range s.Errors {
		fmt.Fprintf(warn, "warning: s3: %s\n", e)
	}
	up := 0
	for _, g := range s.Gateways {
		if g.Up {
			up++
		} else {
			fmt.Fprintf(warn, "warning: s3 gateway on %s is down\n", g.Host)
		}
	}
	cert := "-"
	if s.CertExpires != nil {
		cert = age(s.CertExpires.Sub(now))
		if s.CertExpires.Sub(now) < 14*24*time.Hour {
			fmt.Fprintf(warn, "warning: the S3 certificate expires in %s\n", cert)
		}
	}
	endpoint := strings.TrimPrefix(s.Endpoint, "https://")
	fmt.Fprintln(tw, "S3\tGATEWAYS\tUSERS\tBUCKETS\tOBJECTS\tSTORED\tROOM\tFULL\tCERT")
	fmt.Fprintf(tw, "%s\t%d/%d\t%d\t%d\t%d\t%s\t%s\t%s\t%s\n", endpoint, up, len(s.Gateways), s.Users, s.Buckets, s.Objects,
		bytesHuman(s.SizeBytes), bytesHuman(s.Pool.MaxAvailBytes), pct(s.Pool.UsedPercent), cert)
}

func printBuckets(tw *tabwriter.Writer, warn io.Writer, buckets []api.S3Bucket) {
	fmt.Fprintln(tw, "BUCKET\tOWNER\tOBJECTS\tSIZE\tQUOTA\tFULL")
	for _, b := range buckets {
		quota, full := "-", "-"
		if b.QuotaBytes > 0 {
			quota = bytesHuman(b.QuotaBytes)
			p := percentOf(b.SizeBytes, b.QuotaBytes)
			full = pct(p)
			if p >= 90 {
				fmt.Fprintf(warn, "warning: bucket %s is %s of its quota\n", b.Name, full)
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\n", b.Name, b.Owner, b.Objects, bytesHuman(b.SizeBytes), quota, full)
	}
}

// bytesHuman formats like df -H: decimal units, one decimal below 10.
func bytesHuman(b int64) string {
	if b < 1000 {
		return fmt.Sprintf("%dB", b)
	}
	v := float64(b)
	for _, u := range []string{"K", "M", "G", "T", "P"} {
		v /= 1000
		if v < 1000 || u == "P" {
			if v < 10 {
				return fmt.Sprintf("%.1f%s", v, u)
			}
			return fmt.Sprintf("%.0f%s", v, u)
		}
	}
	return ""
}

func percentOf(part, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return 100 * float64(part) / float64(total)
}

func pct(p float64) string {
	switch {
	case p > 0 && p < 0.1:
		return "<0.1%"
	case p > 0 && p < 1:
		return fmt.Sprintf("%.1f%%", p)
	}
	return fmt.Sprintf("%.0f%%", p)
}

func title(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// shortError keeps the last, most specific part of a Go error chain, e.g.
// "connection refused" or "context deadline exceeded".
func shortError(s string) string {
	if i := strings.LastIndex(s, ": "); i >= 0 {
		s = s[i+2:]
	}
	if j := strings.Index(s, " (Client.Timeout"); j >= 0 {
		s = s[:j]
	}
	return s
}

// age formats a duration like kubectl's AGE column: 45s, 12m, 5h3m, 3d4h, 40d.
func age(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 8*time.Hour:
		if m := int(d.Minutes()) % 60; m > 0 {
			return fmt.Sprintf("%dh%dm", int(d.Hours()), m)
		}
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 8*24*time.Hour:
		if h := int(d.Hours()) % 24; h > 0 {
			return fmt.Sprintf("%dd%dh", int(d.Hours())/24, h)
		}
		return fmt.Sprintf("%dd", int(d.Hours())/24)
	}
	return fmt.Sprintf("%dd", int(d.Hours())/24)
}

// parseSince accepts Go durations plus d (days) and w (weeks).
func parseSince(s string) (time.Duration, error) {
	for suffix, unit := range map[string]time.Duration{"d": 24 * time.Hour, "w": 7 * 24 * time.Hour} {
		if n, ok := strings.CutSuffix(s, suffix); ok {
			v, err := strconv.ParseFloat(n, 64)
			if err != nil || v <= 0 {
				return 0, fmt.Errorf("invalid duration %q", s)
			}
			return time.Duration(v * float64(unit)), nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	return d, nil
}

type promResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string          `json:"resultType"`
		Result     json.RawMessage `json:"result"`
	} `json:"data"`
	Error string `json:"error"`
}

type promSample struct {
	Metric map[string]string `json:"metric"`
	Value  [2]any            `json:"value"`
	Values [][2]any          `json:"values"`
}

func cmdMetrics(e *env) *cobra.Command {
	c := group("metrics", "Query metrics (PromQL / MetricsQL)")
	var at string
	query := &cobra.Command{
		Use: "query PROMQL", Short: "Instant query", Args: args(1, "PROMQL (quote it)"),
		RunE: func(_ *cobra.Command, a []string) error {
			q := url.Values{"query": {a[0]}}
			if at != "" {
				q.Set("time", at)
			}
			return e.metrics("/v1/metrics/query", q)
		},
	}
	query.Flags().StringVar(&at, "time", "", "evaluation time (RFC 3339 or Unix seconds; default now)")
	var since, step string
	rng := &cobra.Command{
		Use: "range PROMQL", Short: "Range query over the last --since", Args: args(1, "PROMQL (quote it)"),
		RunE: func(_ *cobra.Command, a []string) error {
			d, err := parseSince(since)
			if err != nil {
				return usageErr("--since: %v", err)
			}
			st := step
			if st == "" {
				st = (max(d/60, 15*time.Second)).Round(time.Second).String()
			}
			end := time.Now()
			q := url.Values{"query": {a[0]}, "start": {strconv.FormatInt(end.Add(-d).Unix(), 10)},
				"end": {strconv.FormatInt(end.Unix(), 10)}, "step": {st}}
			return e.metrics("/v1/metrics/query_range", q)
		},
	}
	rng.Flags().StringVar(&since, "since", "1h", "how far back")
	rng.Flags().StringVar(&step, "step", "", "resolution (default: since/60, at least 15s)")
	c.AddCommand(query, rng)
	return c
}

func (e *env) metrics(path string, q url.Values) error {
	c, err := e.client()
	if err != nil {
		return err
	}
	body, err := c.Do(e.ctx, path, q)
	if err != nil {
		return err
	}
	defer body.Close()
	if e.json() {
		_, err := io.Copy(e.stdout, body)
		return err
	}
	var resp promResponse
	if err := json.NewDecoder(body).Decode(&resp); err != nil {
		return err
	}
	return printProm(e.stdout, resp)
}

func printProm(w io.Writer, resp promResponse) error {
	switch resp.Data.ResultType {
	case "scalar", "string":
		var v [2]any
		if err := json.Unmarshal(resp.Data.Result, &v); err != nil {
			return err
		}
		fmt.Fprintln(w, v[1])
		return nil
	}
	var samples []promSample
	if err := json.Unmarshal(resp.Data.Result, &samples); err != nil {
		return err
	}
	if len(samples) == 0 {
		fmt.Fprintln(w, "(no data)")
		return nil
	}
	sort.Slice(samples, func(i, j int) bool { return labelString(samples[i].Metric) < labelString(samples[j].Metric) })
	if resp.Data.ResultType == "vector" {
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		for _, s := range samples {
			fmt.Fprintf(tw, "%s\t%v\n", labelString(s.Metric), s.Value[1])
		}
		return tw.Flush()
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SERIES\tTIME\tVALUE")
	for _, s := range samples {
		name := labelString(s.Metric)
		for _, p := range s.Values {
			ts, _ := p[0].(float64)
			fmt.Fprintf(tw, "%s\t%s\t%v\n", name, time.Unix(int64(ts), 0).Local().Format("15:04:05"), p[1])
		}
	}
	return tw.Flush()
}

func labelString(m map[string]string) string {
	name := m["__name__"]
	var parts []string
	for _, k := range sortedKeys(m) {
		if k != "__name__" {
			parts = append(parts, fmt.Sprintf("%s=%q", k, m[k]))
		}
	}
	if len(parts) == 0 && name != "" {
		return name
	}
	if len(parts) == 0 {
		return "{}"
	}
	return name + "{" + strings.Join(parts, ", ") + "}"
}

// buildLogsQuery turns labctl's flags into LogsQL.
func buildLogsQuery(host, unit string, since time.Duration, limit int, follow bool, user string) string {
	var filters []string
	if !follow {
		filters = append(filters, "_time:"+fmtLogsDuration(since))
	}
	if host != "" {
		filters = append(filters, "host:="+strconv.Quote(host))
	}
	if f := unitFilter(unit); f != "" {
		filters = append(filters, f)
	}
	user = strings.TrimSpace(user)
	hasPipe := strings.Contains(user, "|")
	switch {
	case user == "" && len(filters) == 0:
		filters = append(filters, "*")
	case user != "" && hasPipe:
		filters = append(filters, user)
	case user != "":
		filters = append(filters, "("+user+")")
	}
	q := strings.Join(filters, " ")
	if !follow && !hasPipe {
		// Newest N, printed oldest first.
		q += fmt.Sprintf(" | sort by (_time desc) limit %d", limit)
	}
	return q
}

// systemdSuffixes are the unit types a --unit value may already end in.
var systemdSuffixes = []string{".service", ".socket", ".timer", ".mount", ".scope", ".slice", ".target", ".path", ".swap", ".device"}

// unitFilter turns --unit into LogsQL:
//   - "ceph-osd*" matches every unit starting with ceph-osd;
//   - "ceph-osd-0.service" matches exactly;
//   - anything else matches with or without ".service", like journalctl -u,
//     so "clusterd" and the router's syslog app "filterlog" both work, and so
//     do names with dots ("acme-order-renew-s3.lan.buunk.org").
func unitFilter(unit string) string {
	switch {
	case unit == "":
		return ""
	case strings.HasSuffix(unit, "*"):
		return "unit:" + strconv.Quote(strings.TrimSuffix(unit, "*")) + "*"
	}
	for _, suf := range systemdSuffixes {
		if strings.HasSuffix(unit, suf) {
			return "unit:=" + strconv.Quote(unit)
		}
	}
	return fmt.Sprintf("unit:in(%s, %s)", strconv.Quote(unit), strconv.Quote(unit+".service"))
}

func fmtLogsDuration(d time.Duration) string {
	if d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	}
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", d/time.Hour)
	}
	if d%time.Minute == 0 {
		return fmt.Sprintf("%dm", d/time.Minute)
	}
	return fmt.Sprintf("%ds", int64(d.Seconds()))
}

func cmdLogs(e *env) *cobra.Command {
	var host, unit, since string
	var limit int
	var follow bool
	c := &cobra.Command{
		Use: "logs [LOGSQL]", Short: "Search or follow logs of hosts, VMs, the router and the switch (LogsQL)",
		RunE: func(_ *cobra.Command, pos []string) error {
			d, err := parseSince(since)
			if err != nil {
				return usageErr("--since: %v", err)
			}
			if limit < 1 {
				return usageErr("--limit must be at least 1")
			}
			user := strings.Join(pos, " ")
			q := url.Values{"query": {buildLogsQuery(host, unit, d, limit, follow, user)}}
			if follow {
				q.Set("follow", "1")
			} else {
				q.Set("limit", strconv.Itoa(limit))
			}
			return e.logs(q, follow, strings.Contains(user, "|"))
		},
	}
	c.Flags().StringVar(&host, "host", "", "only this host (e.g. hgd1-hv01, hgd1-fw01)")
	c.Flags().StringVar(&unit, "unit", "", "only this systemd unit or syslog app; \"ceph-osd*\" for a prefix")
	c.Flags().StringVar(&since, "since", "1h", "how far back")
	c.Flags().IntVar(&limit, "limit", 100, "at most this many lines")
	c.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing new lines (Ctrl-C stops)")
	c.RegisterFlagCompletionFunc("host", e.complete("/v1/hosts"))
	return c
}

func (e *env) logs(q url.Values, follow, piped bool) error {
	c, err := e.client()
	if err != nil {
		return err
	}
	body, err := c.Do(e.ctx, "/v1/logs/query", q)
	if err != nil {
		return err
	}
	defer body.Close()

	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	if follow {
		for sc.Scan() {
			e.printLogLine(sc.Bytes())
		}
		if e.ctx.Err() != nil {
			return nil
		}
		return sc.Err()
	}
	var lines [][]byte
	for sc.Scan() {
		lines = append(lines, append([]byte(nil), sc.Bytes()...))
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if !piped {
		// The query sorted newest first; print in reading order.
		for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
			lines[i], lines[j] = lines[j], lines[i]
		}
	}
	for _, l := range lines {
		e.printLogLine(l)
	}
	return nil
}

func (e *env) printLogLine(line []byte) {
	if e.json() {
		fmt.Fprintf(e.stdout, "%s\n", line)
		return
	}
	var rec map[string]any
	if err := json.Unmarshal(line, &rec); err != nil {
		fmt.Fprintf(e.stdout, "%s\n", line)
		return
	}
	ts := ""
	if s, ok := rec["_time"].(string); ok {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			ts = t.Local().Format("2006-01-02 15:04:05")
		}
	}
	msg, hasMsg := rec["_msg"].(string)
	if !hasMsg {
		// Pipes like stats return other fields: show them as key=value.
		var parts []string
		for _, k := range sortedKeys(rec) {
			parts = append(parts, fmt.Sprintf("%s=%v", k, rec[k]))
		}
		fmt.Fprintln(e.stdout, strings.Join(parts, " "))
		return
	}
	host, _ := rec["host"].(string)
	unit, _ := rec["unit"].(string)
	fmt.Fprintf(e.stdout, "%s %s %s %s\n", ts, host, strings.TrimSuffix(unit, ".service"), msg)
}
