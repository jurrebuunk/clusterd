package labctl

import (
	"fmt"
	"text/tabwriter"

	"github.com/jurrebuunk/clusterd/internal/api"
)

// cmdHealth prints the site's checks and exits ExitUnhealthy unless all are ok.
func cmdHealth(e *env, args []string) error {
	if len(args) > 0 {
		return usageErr("health takes no arguments")
	}
	c, err := e.client()
	if err != nil {
		return err
	}
	var ch api.Checks
	if err := c.Get(e.ctx, "/v1/checks", nil, &ch); err != nil {
		return err
	}
	if e.json {
		if err := e.printJSON(ch); err != nil {
			return err
		}
	} else {
		tw := tabwriter.NewWriter(e.stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "CHECK\tSTATUS\tMESSAGE")
		for _, x := range ch.Checks {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", x.Name, x.Status, x.Message)
		}
		tw.Flush()
	}
	if ch.Status != api.CheckOK {
		return &exitError{ExitUnhealthy, fmt.Errorf("site is %s", map[string]string{api.CheckWarn: "degraded", api.CheckFail: "unhealthy"}[ch.Status])}
	}
	return nil
}
