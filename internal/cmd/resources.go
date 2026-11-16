package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/teraldev/cli/internal/api"
	"github.com/teraldev/cli/internal/client"
	"github.com/teraldev/cli/internal/gitinfo"
	"github.com/teraldev/cli/internal/link"
	"github.com/teraldev/cli/internal/ui"
)

var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)

// slugify turns a name into a slug suggestion: "My API" → "my-api".
func slugify(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func validateSlug(s string) error {
	if !slugPattern.MatchString(s) {
		return errors.New("use 2-63 lowercase letters, digits, and dashes, starting with a letter or digit")
	}
	return nil
}

func newInitCmd(a *App) *cobra.Command {
	var projectName, appName, repo string
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create a project and app for this directory and link it",
		Long: `Create a project (or pick an existing one) and an app, then link this
directory to it. Afterwards, "teral up" deploys the directory.

The app tracks a GitHub repository when --repo is given (the Teral GitHub App
must have access); otherwise it is deployed with "teral up" or "teral deploy".`,
		Example: "  teral init\n  teral init --project acme --name api --repo acme/api",
		Args:    cobra.NoArgs,
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
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			if l := a.Link(); l != nil && l.Dir == cwd && !a.Yes {
				if err := a.IO.Confirm(fmt.Sprintf("This directory is already linked to %s/%s. Replace the link?", l.Project, l.App), false); err != nil {
					return err
				}
			}

			// Project: existing (flag or pick) or new.
			project := firstNonEmpty(a.Project, os.Getenv("TERAL_PROJECT"))
			if project == "" {
				projects, err := listProjects(ctx, c)
				if err != nil {
					return err
				}
				options := []ui.Option{{Label: "+ Create a new project", Value: ""}}
				for _, p := range projects {
					options = append(options, ui.Option{Label: fmt.Sprintf("%s  %s", p.Name, a.IO.Theme.Muted.Render(p.Slug)), Value: p.Slug})
				}
				if a.IO.Interactive() && len(projects) > 0 {
					project, err = a.IO.Select("Project", "--project", options)
					if err != nil {
						return err
					}
				}
			}
			if project == "" {
				name := projectName
				if name == "" {
					name, err = a.IO.Input("Project name", "--project-name", filepath.Base(cwd), nil)
					if err != nil {
						return err
					}
					if name == "" {
						name = filepath.Base(cwd)
					}
				}
				slug := slugify(name)
				if err := validateSlug(slug); err != nil {
					return &UserError{Message: fmt.Sprintf("Can't derive a project slug from %q: %v.", name, err)}
				}
				if _, err := client.Call[api.V1ProjectsCreateProjectResponseBody](ctx, c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
					return cl.ProjectsCreateProject(ctx, api.V1ProjectsCreateProjectRequestBody{Name: name, Slug: slug})
				}); err != nil {
					return err
				}
				project = slug
				a.IO.Success("Created project %s", a.IO.ErrTheme.Bold.Render(name))
			}

			// App.
			name := appName
			if name == "" {
				name, err = a.IO.Input("App name", "--name", filepath.Base(cwd), nil)
				if err != nil {
					return err
				}
				if name == "" {
					name = filepath.Base(cwd)
				}
			}
			slug := slugify(name)
			if err := validateSlug(slug); err != nil {
				return &UserError{Message: fmt.Sprintf("Can't derive an app slug from %q: %v.", name, err)}
			}
			if repo == "" && a.IO.Interactive() {
				if info, ok := gitinfo.Read(cwd); ok {
					if guess := githubRepo(info.Root); guess != "" {
						if err := a.IO.Confirm(fmt.Sprintf("Connect GitHub repository %s? Pushes will deploy automatically.", guess), false); err == nil {
							repo = guess
						} else if !errors.Is(err, ui.ErrCanceled) {
							return err
						}
					}
				}
			}
			git := &api.AppGitCreateInput{}
			if repo != "" {
				git.Repository = &repo
			}
			if _, err := client.Call[api.V1AppsCreateAppResponseBody](ctx, c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
				return cl.AppsCreateApp(ctx, api.V1AppsCreateAppRequestBody{Project: project, Name: name, Slug: slug, Git: git})
			}); err != nil {
				return err
			}
			a.IO.Success("Created app %s", a.IO.ErrTheme.Bold.Render(name))

			ignored, err := link.Write(cwd, link.Link{Workspace: ws.Slug, Project: project, App: slug})
			if err != nil {
				return err
			}
			a.IO.Success("Linked %s to %s/%s", displayPath(cwd), project, slug)
			if ignored {
				a.IO.Hint("Added .teral/ to .gitignore.")
			}
			a.IO.Blank()
			a.IO.Info("Next: %s to deploy this directory, or %s to configure the build.", a.IO.Code("teral up"), a.IO.Code("teral config init"))
			return nil
		},
	}
	cmd.Flags().StringVar(&projectName, "project-name", "", "Name of the project to create (when not using --project)")
	cmd.Flags().StringVar(&appName, "name", "", "App name (default: directory name)")
	cmd.Flags().StringVar(&repo, "repo", "", "GitHub repository to connect, as owner/repo")
	return cmd
}

var githubRemote = regexp.MustCompile(`github\.com[:/]([A-Za-z0-9._-]+/[A-Za-z0-9._-]+?)(\.git)?$`)

// githubRepo guesses owner/repo from the origin remote.
func githubRepo(root string) string {
	raw, err := os.ReadFile(filepath.Join(root, ".git", "config"))
	if err != nil {
		return ""
	}
	inOrigin := false
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			inOrigin = line == `[remote "origin"]`
			continue
		}
		if inOrigin && strings.HasPrefix(line, "url") {
			if m := githubRemote.FindStringSubmatch(strings.TrimSpace(strings.SplitN(line, "=", 2)[1])); m != nil {
				return m[1]
			}
		}
	}
	return ""
}

func newProjectsCmd(a *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "projects",
		Aliases: []string{"project"},
		Short:   "List, create, and delete projects",
	}
	cmd.AddCommand(&cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List projects",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := a.Client()
			if err != nil {
				return err
			}
			projects, err := listProjects(cmd.Context(), c)
			if err != nil {
				return err
			}
			if a.IO.JSON {
				return a.IO.PrintJSON(projects)
			}
			if len(projects) == 0 {
				a.IO.Info("No projects yet. Create one with %s.", a.IO.Code("teral init"))
				return nil
			}
			var rows [][]string
			for _, p := range projects {
				rows = append(rows, []string{p.Slug, p.Name, p.Id, ui.Ago(p.CreatedAt)})
			}
			a.IO.Table([]string{"Slug", "Name", "ID", "Created"}, rows)
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "create <name>",
		Short: "Create a project",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := a.Client()
			if err != nil {
				return err
			}
			slug := slugify(args[0])
			if err := validateSlug(slug); err != nil {
				return &UserError{Message: fmt.Sprintf("Can't derive a slug from %q: %v.", args[0], err)}
			}
			res, err := client.Call[api.V1ProjectsCreateProjectResponseBody](cmd.Context(), c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
				return cl.ProjectsCreateProject(ctx, api.V1ProjectsCreateProjectRequestBody{Name: args[0], Slug: slug})
			})
			if err != nil {
				return err
			}
			if a.IO.JSON {
				return a.IO.PrintJSON(res.Data)
			}
			a.IO.Success("Created project %s %s", args[0], a.IO.ErrTheme.Muted.Render("("+slug+", "+res.Data.Id+")"))
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "delete <project>",
		Short: "Delete a project and everything in it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := a.Client()
			if err != nil {
				return err
			}
			if err := a.IO.Confirm(fmt.Sprintf("Delete project %s with all its apps and deployments? This can't be undone.", args[0]), a.Yes); err != nil {
				return err
			}
			if _, err := client.Call[api.V1ProjectsDeleteProjectResponseBody](cmd.Context(), c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
				return cl.ProjectsDeleteProject(ctx, api.V1ProjectsDeleteProjectRequestBody{Project: args[0]})
			}); err != nil {
				return err
			}
			a.IO.Success("Deleted project %s", args[0])
			return nil
		},
	})
	return cmd
}

func newAppsCmd(a *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "apps",
		Aliases: []string{"app"},
		Short:   "List, inspect, create, and delete apps",
	}
	cmd.AddCommand(&cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the apps of a project",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, _, err := a.Client()
			if err != nil {
				return err
			}
			t, err := a.ResolveTarget(ctx, c, NeedProject)
			if err != nil {
				return err
			}
			apps, err := listApps(ctx, c, t.Project)
			if err != nil {
				return err
			}
			if a.IO.JSON {
				return a.IO.PrintJSON(apps)
			}
			if len(apps) == 0 {
				a.IO.Info("Project %s has no apps yet. Create one with %s.", t.Project, a.IO.Code("teral apps create"))
				return nil
			}
			var rows [][]string
			for _, app := range apps {
				source := "upload"
				if app.Git != nil && app.Git.Repository != "" {
					source = app.Git.Repository
				} else if app.Oci != nil {
					source = app.Oci.Image
				}
				rows = append(rows, []string{app.Slug, app.Name, source, firstNonEmpty(app.CurrentDeploymentId, "—")})
			}
			a.IO.Table([]string{"Slug", "Name", "Source", "Live deployment"}, rows)
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "info",
		Short: "Show an app and its environments",
		Args:  cobra.NoArgs,
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
			app, err := getApp(ctx, c, t.Project, t.App)
			if err != nil {
				return err
			}
			envs, err := listEnvironments(ctx, c, t.Project, t.App)
			if err != nil {
				return err
			}
			if a.IO.JSON {
				return a.IO.PrintJSON(map[string]any{"app": app, "environments": envs})
			}
			pairs := [][2]string{{"Name", app.Name}, {"Slug", app.Slug}, {"ID", app.Id}, {"Created", ui.Ago(app.CreatedAt)}}
			if app.Git != nil {
				pairs = append(pairs, [2]string{"Repository", firstNonEmpty(app.Git.Repository, "not connected")})
			}
			if app.Oci != nil {
				pairs = append(pairs, [2]string{"Image", app.Oci.Image})
			}
			a.IO.KV(pairs)
			a.IO.Println()
			var rows [][]string
			for _, e := range envs {
				root, dockerfile := "", "auto-detect"
				if e.Build != nil {
					root = e.Build.RootDirectory
					if e.Build.Dockerfile != nil && *e.Build.Dockerfile != "" {
						dockerfile = *e.Build.Dockerfile
					}
				}
				rows = append(rows, []string{e.Slug, string(e.Kind), firstNonEmpty(root, "."), dockerfile})
			}
			a.IO.Table([]string{"Environment", "Kind", "Root directory", "Dockerfile"}, rows)
			return nil
		},
	})
	var repo, image string
	create := &cobra.Command{
		Use:   "create <name>",
		Short: "Create an app in a project",
		Long: `Create an app. Without --repo or --image, the app is deployed from your
machine with "teral up".`,
		Example: "  teral apps create api\n  teral apps create api --repo acme/api\n  teral apps create web --image ghcr.io/acme/web:latest",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, _, err := a.Client()
			if err != nil {
				return err
			}
			t, err := a.ResolveTarget(ctx, c, NeedProject)
			if err != nil {
				return err
			}
			slug := slugify(args[0])
			if err := validateSlug(slug); err != nil {
				return &UserError{Message: fmt.Sprintf("Can't derive a slug from %q: %v.", args[0], err)}
			}
			body := api.V1AppsCreateAppRequestBody{Project: t.Project, Name: args[0], Slug: slug}
			switch {
			case repo != "" && image != "":
				return &UserError{Message: "Pass either --repo or --image, not both."}
			case image != "":
				body.Oci = &api.AppOCI{Image: image}
			case repo != "":
				body.Git = &api.AppGitCreateInput{Repository: &repo}
			default:
				body.Git = &api.AppGitCreateInput{}
			}
			res, err := client.Call[api.V1AppsCreateAppResponseBody](ctx, c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
				return cl.AppsCreateApp(ctx, body)
			})
			if err != nil {
				return err
			}
			if a.IO.JSON {
				return a.IO.PrintJSON(res.Data)
			}
			a.IO.Success("Created app %s in %s %s", args[0], t.Project, a.IO.ErrTheme.Muted.Render("("+slug+")"))
			a.IO.Hint("Link a directory to it with `teral link --project %s --app %s`.", t.Project, slug)
			return nil
		},
	}
	create.Flags().StringVar(&repo, "repo", "", "GitHub repository to deploy from, as owner/repo")
	create.Flags().StringVar(&image, "image", "", "Container image to deploy")
	cmd.AddCommand(create)
	cmd.AddCommand(&cobra.Command{
		Use:   "delete",
		Short: "Delete an app and its deployments",
		Args:  cobra.NoArgs,
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
			if err := a.IO.Confirm(fmt.Sprintf("Delete app %s/%s with all its deployments and domains? This can't be undone.", t.Project, t.App), a.Yes); err != nil {
				return err
			}
			if _, err := client.Call[api.V1AppsDeleteAppResponseBody](ctx, c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
				return cl.AppsDeleteApp(ctx, api.V1AppsDeleteAppRequestBody{Project: t.Project, App: t.App})
			}); err != nil {
				return err
			}
			a.IO.Success("Deleted app %s/%s", t.Project, t.App)
			return nil
		},
	})
	return cmd
}
