package cmd

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/spf13/cobra"

	"github.com/teraldev/cli/internal/api"
	"github.com/teraldev/cli/internal/client"
	"github.com/teraldev/cli/internal/ui"
)

func newDeploymentsCmd(a *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "deployments",
		Aliases: []string{"deployment", "dp"},
		Short:   "List, inspect, promote, and roll back deployments",
	}

	var limit int
	var status []string
	list := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List recent deployments",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, _, err := a.Client()
			if err != nil {
				return err
			}
			t, err := a.ResolveTarget(ctx, c, NeedApp)
			if err != nil {
				return err
			}
			req := api.V1DeploymentsListDeploymentsRequestBody{Project: &t.Project, App: &t.App, Environment: nonEmpty(t.Environment), Limit: ptr(min(max(limit, 1), 100))}
			if len(status) > 0 {
				s := make([]api.DeploymentStatus, len(status))
				for i, v := range status {
					s[i] = api.DeploymentStatus(strings.ToLower(v))
				}
				req.Status = &s
			}
			res, err := client.Call[api.V1DeploymentsListDeploymentsResponseBody](ctx, c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
				return cl.DeploymentsListDeployments(ctx, req)
			})
			if err != nil {
				return err
			}
			if a.IO.JSON {
				return a.IO.PrintJSON(res.Data)
			}
			if len(res.Data) == 0 {
				a.IO.Info("No deployments yet. Deploy with %s or %s.", a.IO.Code("teral up"), a.IO.Code("teral deploy"))
				return nil
			}
			th := a.IO.Theme
			var rows [][]string
			for _, d := range res.Data {
				id := d.Id
				if d.IsCurrent {
					id += th.Accent.Render(" ★")
				}
				rows = append(rows, []string{id, th.Status(string(d.Status)), d.Environment, deploymentSource(d), ui.Ago(d.CreatedAt)})
			}
			a.IO.Table([]string{"ID", "Status", "Environment", "Source", "Created"}, rows)
			return nil
		},
	}
	list.Flags().IntVarP(&limit, "limit", "n", 20, "Number of deployments")
	list.Flags().StringSliceVar(&status, "status", nil, "Only these statuses, e.g. ready,failed")
	cmd.AddCommand(list)

	var wait bool
	get := &cobra.Command{
		Use:   "get [deployment-id]",
		Short: "Show a deployment (default: the newest)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, _, err := a.Client()
			if err != nil {
				return err
			}
			var dep *api.Deployment
			if len(args) == 1 {
				dep, err = getDeployment(ctx, c, args[0])
			} else {
				var t Target
				t, err = a.ResolveTarget(ctx, c, NeedEnvironment)
				if err == nil {
					dep, err = latestDeployment(ctx, c, t, false)
				}
			}
			if err != nil {
				return err
			}
			if wait && !isSettled(string(dep.Status)) {
				dep, err = a.follow(ctx, c, dep.Id)
				if err != nil {
					return err
				}
			}
			if a.IO.JSON {
				return a.IO.PrintJSON(dep)
			}
			a.printDeployment(*dep)
			return nil
		},
	}
	get.Flags().BoolVar(&wait, "wait", false, "Wait until the deployment is ready or failed")
	cmd.AddCommand(get)

	action := func(use, short, past string, confirm func(id string) string, call func(context.Context, *api.Client, string) (*http.Response, error)) *cobra.Command {
		return &cobra.Command{
			Use:   use + " <deployment-id>",
			Short: short,
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				c, _, err := a.Client()
				if err != nil {
					return err
				}
				if confirm != nil {
					if err := a.IO.Confirm(confirm(args[0]), a.Yes); err != nil {
						return err
					}
				}
				if _, err := client.Call[map[string]any](cmd.Context(), c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
					return call(ctx, cl, args[0])
				}); err != nil {
					return err
				}
				a.IO.Success("%s %s", past, args[0])
				return nil
			},
		}
	}
	cmd.AddCommand(action("promote", "Make a deployment serve production traffic", "Promoted",
		func(id string) string { return fmt.Sprintf("Promote %s to serve production traffic?", id) },
		func(ctx context.Context, cl *api.Client, id string) (*http.Response, error) {
			return cl.DeploymentsPromoteDeployment(ctx, api.V1DeploymentsPromoteDeploymentRequestBody{DeploymentId: id})
		}))
	cmd.AddCommand(action("rollback", "Serve production traffic from an earlier deployment", "Rolled back to",
		func(id string) string { return fmt.Sprintf("Roll production back to %s?", id) },
		func(ctx context.Context, cl *api.Client, id string) (*http.Response, error) {
			return cl.DeploymentsRollbackDeployment(ctx, api.V1DeploymentsRollbackDeploymentRequestBody{DeploymentId: id})
		}))
	cmd.AddCommand(action("stop", "Stop a deployment's instances", "Stopped",
		func(id string) string { return fmt.Sprintf("Stop %s? It stops serving traffic.", id) },
		func(ctx context.Context, cl *api.Client, id string) (*http.Response, error) {
			return cl.DeploymentsStopDeployment(ctx, api.V1DeploymentsStopDeploymentRequestBody{DeploymentId: id})
		}))
	cmd.AddCommand(action("start", "Start a stopped deployment", "Started", nil,
		func(ctx context.Context, cl *api.Client, id string) (*http.Response, error) {
			return cl.DeploymentsStartDeployment(ctx, api.V1DeploymentsStartDeploymentRequestBody{DeploymentId: id})
		}))
	return cmd
}

func isSettled(status string) bool {
	switch status {
	case "ready", "failed", "cancelled", "superseded", "skipped", "stopped", "awaiting_approval":
		return true
	}
	return false
}

func (a *App) printDeployment(d api.Deployment) {
	th := a.IO.Theme
	current := ""
	if d.IsCurrent {
		current = th.Accent.Render("  serving production")
	}
	pairs := [][2]string{
		{"ID", d.Id},
		{"Status", th.Status(string(d.Status)) + current},
		{"App", d.Project + "/" + d.App + th.Muted.Render(" ("+d.Environment+")")},
		{"Source", deploymentSource(d)},
		{"Created", ui.Ago(d.CreatedAt)},
		{"Regions", strings.Join(d.Regions, ", ")},
	}
	if d.Domains != nil && len(*d.Domains) > 0 {
		urls := make([]string, len(*d.Domains))
		for i, dom := range *d.Domains {
			urls[i] = "https://" + dom
		}
		pairs = append(pairs, [2]string{"URLs", strings.Join(urls, "\n"+strings.Repeat(" ", 9))})
	}
	if d.Error != nil {
		pairs = append(pairs, [2]string{"Error", th.Danger.Render(d.Error.Message)})
	}
	if len(d.AvailableActions) > 0 {
		actions := make([]string, len(d.AvailableActions))
		for i, act := range d.AvailableActions {
			actions[i] = string(act)
		}
		pairs = append(pairs, [2]string{"Actions", strings.Join(actions, ", ")})
	}
	a.IO.KV(pairs)
}
