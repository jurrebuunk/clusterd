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

	"github.com/jurrebuunk/clusterd/internal/api"
)

func cmdStatus(e *env, args []string) error {
	if len(args) > 0 {
		return usageErr("status takes no arguments")
	}
	c, err := e.client()
	if err != nil {
		return err
	}
	st, err := c.Status(e.ctx)
	if err != nil {
		return err
	}
	if e.json {
		return e.printJSON(st)
	}
	printStatus(e.stdout, st)
	return nil
}

func printStatus(w io.Writer, st api.Status) {
	fmt.Fprintf(w, "Site %s · served by %s · %s\n", st.Site, st.ServedBy, st.GeneratedAt.Local().Format("2006-01-02 15:04:05"))

	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "\nHOST\tSTATE\tCLUSTERD\tUPTIME\tLOAD\tMEM\tSTORES")
	for _, h := range st.Hosts {
		if !h.Reachable {
			fmt.Fprintf(tw, "%s\tunreachable\t-\t-\t-\t-\t%s\n", h.Name, h.Error)
			continue
		}
		stores := "ok"
		for _, s := range h.Stores {
			if !s.OK {
				stores = s.Name + " down"
			}
		}
		state := "up"
		if h.Error != "" {
			state = "degraded"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%.2f\t%.0f%%\t%s\n", h.Name, state, h.Version, uptime(h.UptimeSeconds), h.Load1, h.MemUsedPercent, stores)
	}
	tw.Flush()

	fmt.Fprintln(tw, "\nVM\tHOST\tSTATE")
	for _, vm := range st.VMs {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", vm.Name, vm.Host, vm.State)
	}
	tw.Flush()

	if !st.K3s.Reachable {
		fmt.Fprintf(w, "\nk3s: unreachable (%s)\n", st.K3s.Error)
	} else {
		ready := 0
		for _, n := range st.K3s.Nodes {
			if n.Ready {
				ready++
			}
		}
		fmt.Fprintf(w, "\nk3s: %d/%d nodes ready\n", ready, len(st.K3s.Nodes))
		fmt.Fprintln(tw, "NODE\tREADY\tSCHEDULING\tROLES\tVERSION\tZONE")
		for _, n := range st.K3s.Nodes {
			sched := "enabled"
			if !n.Schedulable {
				sched = "cordoned"
			}
			roles := strings.Join(n.Roles, ",")
			if roles == "" {
				roles = "worker"
			}
			fmt.Fprintf(tw, "%s\t%v\t%s\t%s\t%s\t%s\n", n.Name, n.Ready, sched, roles, n.Version, n.Zone)
		}
		tw.Flush()
	}

	cp := st.Ceph
	if cp.Error != "" {
		fmt.Fprintf(w, "\nceph: %s (%s)\n", cp.Health, cp.Error)
	} else {
		fmt.Fprintf(w, "\nceph: %s · OSDs %d up, %d in, of %d · PGs %d/%d active+clean\n",
			cp.Health, cp.OSDsUp, cp.OSDsIn, cp.OSDsTotal, cp.PGsActiveClean, cp.PGsTotal)
		for _, chk := range cp.Checks {
			fmt.Fprintf(w, "  %s\n", chk)
		}
	}

	fmt.Fprintln(tw, "\nVIP\tADDRESS\tOWNER")
	for _, v := range st.VIPs {
		owner := strings.Join(v.Owners, ", ")
		switch len(v.Owners) {
		case 0:
			owner = "none"
		case 1:
		default:
			owner += " (split brain!)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", v.Name, v.Address, owner)
	}
	tw.Flush()
}

func uptime(s int64) string {
	d := time.Duration(s) * time.Second
	if days := int(d.Hours()) / 24; days > 0 {
		return fmt.Sprintf("%dd%dh", days, int(d.Hours())%24)
	}
	return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
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

func cmdMetrics(e *env, args []string) error {
	if len(args) == 0 || (args[0] != "query" && args[0] != "range") {
		return usageErr("usage: labctl metrics query <promql> [--time T] | metrics range <promql> [--since 1h] [--step 1m]")
	}
	mode := args[0]
	fs := newFlags(e, "metrics "+mode)
	at := fs.String("time", "", "evaluation time (RFC 3339 or Unix seconds; default now)")
	since := fs.String("since", "1h", "range: how far back")
	step := fs.String("step", "", "range: resolution (default: since/60, at least 15s)")
	pos, err := parseArgs(fs, args[1:])
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("give exactly one query (quote it)")
	}
	q := url.Values{"query": {pos[0]}}
	path := "/v1/metrics/query"
	if mode == "query" {
		if *at != "" {
			q.Set("time", *at)
		}
	} else {
		path = "/v1/metrics/query_range"
		d, err := parseSince(*since)
		if err != nil {
			return usageErr("--since: %v", err)
		}
		st := *step
		if st == "" {
			st = (max(d/60, 15*time.Second)).Round(time.Second).String()
		}
		end := time.Now()
		q.Set("start", strconv.FormatInt(end.Add(-d).Unix(), 10))
		q.Set("end", strconv.FormatInt(end.Unix(), 10))
		q.Set("step", st)
	}

	c, err := e.client()
	if err != nil {
		return err
	}
	body, err := c.Do(e.ctx, path, q)
	if err != nil {
		return err
	}
	defer body.Close()
	if e.json {
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
	for i, s := range samples {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintln(w, labelString(s.Metric))
		for _, p := range s.Values {
			ts, _ := p[0].(float64)
			fmt.Fprintf(w, "  %s  %v\n", time.Unix(int64(ts), 0).Local().Format("2006-01-02 15:04:05"), p[1])
		}
	}
	return nil
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
	if unit != "" {
		filters = append(filters, "unit:="+strconv.Quote(unit))
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

func cmdLogs(e *env, args []string) error {
	fs := newFlags(e, "logs")
	host := fs.String("host", "", "only this host (e.g. hgd1-hv01, hgd1-fw01)")
	unit := fs.String("unit", "", "only this systemd unit or syslog app")
	since := fs.String("since", "1h", "how far back")
	limit := fs.Int("limit", 100, "at most this many lines")
	follow := fs.Bool("follow", false, "keep printing new lines (Ctrl-C stops)")
	fs.BoolVar(follow, "f", false, "short for --follow")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	d, err := parseSince(*since)
	if err != nil {
		return usageErr("--since: %v", err)
	}
	if *limit < 1 {
		return usageErr("--limit must be at least 1")
	}
	query := buildLogsQuery(*host, *unit, d, *limit, *follow, strings.Join(pos, " "))

	q := url.Values{"query": {query}}
	if *follow {
		q.Set("follow", "1")
	} else {
		q.Set("limit", strconv.Itoa(*limit))
	}
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
	if *follow {
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
	if !strings.Contains(strings.Join(pos, " "), "|") {
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
	if e.json {
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
	fmt.Fprintf(e.stdout, "%s %s %s: %s\n", ts, host, unit, msg)
}
