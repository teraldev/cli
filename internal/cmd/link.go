package cmd

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/teraldev/cli/internal/api"
	"github.com/teraldev/cli/internal/client"
	"github.com/teraldev/cli/internal/link"
	"github.com/teraldev/cli/internal/ui"
)

// workspaceOf returns the slug of the workspace a client acts in, asking the
// API for API keys.
func (a *App) workspaceOf(ctx context.Context, c *client.Client, cred *Credential) (api.WorkspaceSummary, error) {
	if cred.Account != nil {
		return api.WorkspaceSummary{Id: cred.Account.WorkspaceID, Name: cred.Account.WorkspaceName, Slug: cred.Workspace}, nil
	}
	res, err := client.Call[api.V1AuthWhoamiResponseBody](ctx, c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
		return cl.AuthWhoami(ctx, api.AuthWhoamiJSONRequestBody{})
	})
	if err != nil {
		return api.WorkspaceSummary{}, err
	}
	return res.Data.Workspace, nil
}

func newLinkCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:   "link",
		Short: "Link this directory to an app",
		Long: `Remember which project, app, and environment this directory deploys to, so
later commands need no flags. The link is stored in .teral/link.json, which is
added to .gitignore; it applies to every directory below this one.`,
		Example: "  teral link\n  teral link --project acme --app api --env production",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, cred, err := a.Client()
			if err != nil {
				return err
			}
			// Linking chooses afresh instead of inheriting an existing link.
			a.link, a.linkLoaded = nil, true
			t, err := a.ResolveTarget(ctx, c, NeedEnvironment)
			if err != nil {
				return err
			}
			ws, err := a.workspaceOf(ctx, c, cred)
			if err != nil {
				return err
			}
			// Validate the target, and store slugs even when ids were passed.
			env, err := getEnvironment(ctx, c, t)
			if err != nil {
				return err
			}
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			l := link.Link{Workspace: ws.Slug, Project: t.Project, App: t.App, Environment: env.Slug}
			ignored, err := link.Write(cwd, l)
			if err != nil {
				return err
			}
			if a.IO.JSON {
				return a.IO.PrintJSON(l)
			}
			a.IO.Success("Linked %s to %s", displayPath(cwd), a.IO.ErrTheme.Bold.Render(t.Project+"/"+t.App)+a.IO.ErrTheme.Muted.Render(" ("+env.Slug+")"))
			if ignored {
				a.IO.Hint("Added .teral/ to .gitignore.")
			}
			a.IO.Hint("Deploy this directory with `teral up`.")
			return nil
		},
	}
}

func newUnlinkCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:   "unlink",
		Short: "Remove this directory's link",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			l := a.Link()
			if l == nil {
				a.IO.Info("This directory isn't linked.")
				return nil
			}
			if err := link.Remove(l.Dir); err != nil {
				return err
			}
			a.IO.Success("Unlinked %s from %s/%s", displayPath(l.Dir), l.Project, l.App)
			return nil
		},
	}
}

func newStatusCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show what this directory is linked to and its live deployment",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			l := a.Link()
			c, cred, err := a.Client()
			if err != nil {
				return err
			}
			ws, err := a.workspaceOf(ctx, c, cred)
			if err != nil {
				return err
			}
			if l == nil && a.Project == "" && a.AppSlug == "" {
				if a.IO.JSON {
					return a.IO.PrintJSON(map[string]any{"workspace": ws, "linked": false})
				}
				a.IO.KV([][2]string{{"Workspace", ws.Name + a.IO.Theme.Muted.Render(" ("+ws.Slug+")")}, {"Directory", "not linked"}})
				a.IO.Hint("Run `teral link` to link this directory to an app.")
				return nil
			}
			t, err := a.ResolveTarget(ctx, c, NeedEnvironment)
			if err != nil {
				return err
			}
			app, err := getApp(ctx, c, t.Project, t.App)
			if err != nil {
				return err
			}
			var current *api.Deployment
			if latest, err := latestDeployment(ctx, c, t, true); err == nil {
				current = latest
			}
			if a.IO.JSON {
				return a.IO.PrintJSON(map[string]any{"workspace": ws, "linked": l != nil, "target": t, "app": app, "deployment": current})
			}
			pairs := [][2]string{
				{"Workspace", ws.Name + a.IO.Theme.Muted.Render(" ("+ws.Slug+")")},
				{"Project", t.Project},
				{"App", app.Name + a.IO.Theme.Muted.Render(" ("+app.Slug+")")},
				{"Environment", t.Environment},
			}
			if app.Git != nil {
				pairs = append(pairs, [2]string{"Repository", app.Git.Repository})
			}
			if l != nil {
				pairs = append(pairs, [2]string{"Linked at", displayPath(l.Dir)})
			}
			if current != nil {
				pairs = append(pairs,
					[2]string{"Deployment", current.Id + "  " + a.IO.Theme.Status(string(current.Status))},
					[2]string{"Source", deploymentSource(*current)},
					[2]string{"Deployed", ui.Ago(current.CreatedAt)},
				)
				if current.Domains != nil && len(*current.Domains) > 0 {
					pairs = append(pairs, [2]string{"URL", "https://" + (*current.Domains)[0]})
				}
			} else {
				pairs = append(pairs, [2]string{"Deployment", a.IO.Theme.Muted.Render("none yet")})
			}
			a.IO.KV(pairs)
			return nil
		},
	}
}

func newOpenCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:   "open",
		Short: "Open the linked app in the dashboard",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, cred, err := a.Client()
			if err != nil {
				return err
			}
			ws, err := a.workspaceOf(ctx, c, cred)
			if err != nil {
				return err
			}
			base := a.Config.DashboardEndpoint() + "/" + ws.Slug
			if a.Link() == nil && a.Project == "" {
				return a.openURL(base + "/projects")
			}
			t, err := a.ResolveTarget(ctx, c, NeedApp)
			if err != nil {
				return err
			}
			project, err := getProject(ctx, c, t.Project)
			if err != nil {
				return err
			}
			app, err := getApp(ctx, c, t.Project, t.App)
			if err != nil {
				return err
			}
			return a.openURL(fmt.Sprintf("%s/projects/%s/apps/%s/overview", base, project.Id, app.Id))
		},
	}
}

func displayPath(p string) string {
	if home, err := os.UserHomeDir(); err == nil {
		if rel, err := filepath.Rel(home, p); err == nil && !filepath.IsAbs(rel) && rel != ".." && len(rel) >= 2 && rel[:2] != ".." {
			return filepath.Join("~", rel)
		}
	}
	return p
}

func getProject(ctx context.Context, c *client.Client, project string) (api.Project, error) {
	res, err := client.Call[api.V1ProjectsGetProjectResponseBody](ctx, c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
		return cl.ProjectsGetProject(ctx, api.V1ProjectsGetProjectRequestBody{Project: project})
	})
	if err != nil {
		return api.Project{}, err
	}
	return res.Data, nil
}

func getApp(ctx context.Context, c *client.Client, project, app string) (api.App, error) {
	res, err := client.Call[api.V1AppsGetAppResponseBody](ctx, c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
		return cl.AppsGetApp(ctx, api.V1AppsGetAppRequestBody{Project: project, App: app})
	})
	if err != nil {
		return api.App{}, err
	}
	return res.Data, nil
}

func getEnvironment(ctx context.Context, c *client.Client, t Target) (api.Environment, error) {
	res, err := client.Call[api.V1EnvironmentsGetEnvironmentResponseBody](ctx, c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
		return cl.EnvironmentsGetEnvironment(ctx, api.V1EnvironmentsGetEnvironmentRequestBody{Project: t.Project, App: t.App, Environment: t.Environment})
	})
	if err != nil {
		return api.Environment{}, err
	}
	return res.Data, nil
}
