package status

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jurrebuunk/clusterd/internal/api"
)

// Operations are not stored anywhere new: every action already writes
// "audit" (start and end) and "action progress" records to the journal, and
// Vector ships those to VictoriaLogs. The history is a query over them.

// ErrNoOperation means no record with that ID exists in the logs.
var ErrNoOperation = errors.New("no such operation")

// opTimeout is how long an action may run (actions.Timing.Total) before a
// start without an end counts as lost.
const opTimeout = 60 * time.Minute

// opRecord is one audit or progress line, unpacked from logfmt.
type opRecord struct {
	Time      string `json:"_time"`
	Host      string `json:"host"`
	Msg       string `json:"msg"`
	Action    string `json:"action"`
	Target    string `json:"target"`
	Token     string `json:"token"`
	RequestID string `json:"request_id"`
	Event     string `json:"event"`
	Result    string `json:"result"`
	Error     string `json:"error"`
	Message   string `json:"message"`
}

const opFields = `| unpack_logfmt | filter msg:in("audit", "action progress") ` +
	`| fields _time, host, msg, action, target, token, request_id, event, result, error, message`

// ValidOperationID: request IDs are 16 lowercase hex characters.
func ValidOperationID(id string) bool {
	if len(id) != 16 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// Operations lists the actions of the last since, newest first; target
// narrows it to one VM, node or host.
func (c *Collector) Operations(ctx context.Context, since time.Duration, target string, limit int) ([]api.Operation, error) {
	q := fmt.Sprintf(`_time:%ds {unit="clusterd.service"} "msg=audit" %s`, int64(since.Seconds()), opFields)
	if target != "" {
		q += " | filter target:=" + strconv.Quote(target)
	}
	recs, err := c.logQuery(ctx, q, 10000)
	if err != nil {
		return nil, err
	}
	ops := c.foldOperations(recs)
	sort.Slice(ops, func(i, j int) bool { return ops[i].Started.After(ops[j].Started) })
	if limit > 0 && len(ops) > limit {
		ops = ops[:limit]
	}
	for i := range ops {
		ops[i].Events = nil
	}
	return ops, nil
}

// Operation is one action with its progress lines.
func (c *Collector) Operation(ctx context.Context, id string) (api.Operation, error) {
	if !ValidOperationID(id) {
		return api.Operation{}, ErrNoOperation
	}
	q := fmt.Sprintf(`_time:30d {unit="clusterd.service"} "request_id=%s" %s`, id, opFields)
	recs, err := c.logQuery(ctx, q, 1000)
	if err != nil {
		return api.Operation{}, err
	}
	for _, op := range c.foldOperations(recs) {
		if op.ID == id {
			return op, nil
		}
	}
	return api.Operation{}, ErrNoOperation
}

// foldOperations groups records by request ID into operations.
func (c *Collector) foldOperations(recs []opRecord) []api.Operation {
	sort.SliceStable(recs, func(i, j int) bool { return recs[i].Time < recs[j].Time })
	byID := map[string]*api.Operation{}
	var order []string
	for _, r := range recs {
		if r.RequestID == "" || r.Action == "" {
			continue
		}
		t, _ := time.Parse(time.RFC3339Nano, r.Time)
		op, ok := byID[r.RequestID]
		if !ok {
			op = &api.Operation{ID: r.RequestID, Action: r.Action, Target: r.Target, Host: r.Host, Token: r.Token, State: api.OpRunning, Started: t}
			byID[r.RequestID] = op
			order = append(order, r.RequestID)
		}
		switch {
		case r.Msg == "action progress":
			op.Events = append(op.Events, api.ActionEvent{Time: t, Status: api.EventProgress, Message: r.Message})
		case r.Event == "end":
			op.Finished = &t
			if r.Result == "ok" {
				op.State = api.OpSucceeded
				op.Events = append(op.Events, api.ActionEvent{Time: t, Status: api.EventDone, Message: r.Action + " " + r.Target + " done"})
			} else {
				op.State, op.Error = api.OpFailed, r.Error
				op.Events = append(op.Events, api.ActionEvent{Time: t, Status: api.EventError, Message: r.Error})
			}
		}
	}
	out := make([]api.Operation, 0, len(order))
	for _, id := range order {
		op := byID[id]
		if op.State == api.OpRunning && c.now().Sub(op.Started) > opTimeout {
			op.State = api.OpUnknown
		}
		out = append(out, *op)
	}
	return out
}

// logQuery runs a LogsQL query against the log stores, this host's first.
func (c *Collector) logQuery(ctx context.Context, q string, limit int) ([]opRecord, error) {
	var lastErr error
	for _, h := range c.cfg.LocalFirst() {
		u := strings.TrimRight(h.Logs, "/") + "/select/logsql/query?" +
			url.Values{"query": {q}, "limit": {strconv.Itoa(limit)}}.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		resp, err := c.plain.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			lastErr = fmt.Errorf("%s: %s", h.Name, resp.Status)
			continue
		}
		var out []opRecord
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			var r opRecord
			if json.Unmarshal(sc.Bytes(), &r) == nil {
				out = append(out, r)
			}
		}
		err = sc.Err()
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		return out, nil
	}
	return nil, fmt.Errorf("no log store answered: %w", lastErr)
}
