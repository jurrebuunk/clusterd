package status

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// sample is one series of an instant query.
type sample struct {
	Metric map[string]string
	Value  float64
}

// query runs an instant PromQL query against the stores, this host's first.
func (c *Collector) query(ctx context.Context, q string) ([]sample, error) {
	var lastErr error
	for _, h := range c.cfg.LocalFirst() {
		u := strings.TrimRight(h.Metrics, "/") + "/api/v1/query?" + url.Values{"query": {q}}.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		resp, err := c.plain.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		var body struct {
			Status string `json:"status"`
			Error  string `json:"error"`
			Data   struct {
				Result []struct {
					Metric map[string]string `json:"metric"`
					Value  [2]any            `json:"value"`
				} `json:"result"`
			} `json:"data"`
		}
		err = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if err != nil || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("%s: %s %v", h.Name, resp.Status, err)
			continue
		}
		if body.Status != "success" {
			return nil, fmt.Errorf("query %q: %s", q, body.Error)
		}
		out := make([]sample, 0, len(body.Data.Result))
		for _, r := range body.Data.Result {
			s, _ := r.Value[1].(string)
			v, _ := strconv.ParseFloat(s, 64)
			out = append(out, sample{Metric: r.Metric, Value: v})
		}
		return out, nil
	}
	return nil, fmt.Errorf("no metrics store answered: %w", lastErr)
}

// byLabels indexes a query result by the given labels joined with "/".
func byLabels(ss []sample, labels ...string) map[string]float64 {
	out := map[string]float64{}
	for _, s := range ss {
		var k []string
		for _, l := range labels {
			k = append(k, s.Metric[l])
		}
		out[strings.Join(k, "/")] = s.Value
	}
	return out
}

func percent(used, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return round1(100 * float64(used) / float64(total))
}
