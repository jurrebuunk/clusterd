package labctl

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/jurrebuunk/clusterd/internal/api"
	"github.com/jurrebuunk/clusterd/internal/client"
)

func cmdOperation(e *env) *cobra.Command {
	c := group("operation", "What actions ran, for whom, and how they ended", "op", "ops")

	var since, target string
	var limit int
	list := &cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "Actions of the last day (or --since), newest first", Args: noArgs,
		RunE: func(*cobra.Command, []string) error {
			d, err := parseSince(since)
			if err != nil {
				return usageErr("--since: %v", err)
			}
			q := url.Values{"since": {d.String()}, "limit": {strconv.Itoa(limit)}}
			if target != "" {
				q.Set("target", target)
			}
			cl, err := e.client()
			if err != nil {
				return err
			}
			var l api.List[api.Operation]
			if err := cl.Get(e.ctx, "/v1/operations", q, &l); err != nil {
				return err
			}
			return e.render(l, namesOf(l.Items, func(o api.Operation) string { return o.ID }), func() error {
				tw := newTable(e.stdout)
				fmt.Fprintln(tw, "OPERATION\tACTION\tTARGET\tSTATUS\tAGE\tDURATION\tBY")
				now := time.Now()
				for _, o := range l.Items {
					dur := "-"
					if o.Finished != nil {
						dur = age(o.Finished.Sub(o.Started))
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", o.ID, o.Action, o.Target, title(o.State), age(now.Sub(o.Started)), dur, o.Token)
				}
				return tw.Flush()
			})
		},
	}
	list.Flags().StringVar(&since, "since", "24h", "how far back (e.g. 1h, 7d; at most 31d)")
	list.Flags().StringVar(&target, "target", "", "only actions on this VM, node or host")
	list.Flags().IntVar(&limit, "limit", 100, "at most this many")

	var watch bool
	show := &cobra.Command{
		Use: "show ID", Short: "One action with its progress", Args: args(1, "ID"),
		ValidArgsFunction: e.complete("/v1/operations"),
		RunE: func(_ *cobra.Command, a []string) error {
			if watch {
				return e.watchOperation(a[0])
			}
			var o api.Operation
			if err := e.get("/v1/operations/"+url.PathEscape(a[0]), &o); err != nil {
				return err
			}
			return e.render(o, []string{o.ID}, func() error {
				if err := e.operationFields(o); err != nil {
					return err
				}
				fmt.Fprintln(e.stdout)
				for _, ev := range o.Events {
					e.printEvent(ev)
				}
				return nil
			})
		},
	}
	show.Flags().BoolVarP(&watch, "watch", "w", false, "keep printing progress until it ends")
	c.AddCommand(list, show)
	return c
}

func (e *env) operationFields(o api.Operation) error {
	finished, dur := "-", "-"
	if o.Finished != nil {
		finished = o.Finished.Local().Format("2006-01-02 15:04:05")
		dur = age(o.Finished.Sub(o.Started))
	}
	rows := [][2]string{{"Operation", o.ID}, {"Action", o.Action}, {"Target", o.Target}, {"Status", title(o.State)},
		{"Ran on", o.Host}, {"By", o.Token}, {"Started", o.Started.Local().Format("2006-01-02 15:04:05")},
		{"Finished", finished}, {"Duration", dur}}
	if o.Error != "" {
		rows = append(rows, [2]string{"Error", o.Error})
	}
	return e.fields(rows)
}

func (e *env) printEvent(ev api.ActionEvent) {
	if e.json() {
		e.printJSON(ev)
		return
	}
	fmt.Fprintf(e.stdout, "%s  %s\n", ev.Time.Local().Format("15:04:05"), ev.Message)
}

// watchOperation polls an operation and prints new progress until it ends.
// The history comes from the logs, a few seconds behind the action.
func (e *env) watchOperation(id string) error {
	cl, err := e.client()
	if err != nil {
		return err
	}
	seen := 0
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var o api.Operation
		err := cl.Get(e.ctx, "/v1/operations/"+url.PathEscape(id), nil, &o)
		var ae *client.APIError
		switch {
		case errors.As(err, &ae) && ae.Status == http.StatusNotFound && time.Now().Before(deadline):
			// Not in the logs yet.
		case err != nil:
			return err
		default:
			for _, ev := range o.Events[min(seen, len(o.Events)):] {
				e.printEvent(ev)
			}
			seen = len(o.Events)
			switch o.State {
			case api.OpSucceeded:
				return nil
			case api.OpFailed:
				return &exitError{ExitAPI, errors.New(o.Error)}
			case api.OpUnknown:
				return &exitError{ExitAPI, errors.New("no end recorded: clusterd stopped while it ran")}
			}
		}
		select {
		case <-e.ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}
