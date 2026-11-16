package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/teraldev/cli/internal/api"
	"github.com/teraldev/cli/internal/client"
	"github.com/teraldev/cli/internal/config"
	"github.com/teraldev/cli/internal/link"
	"github.com/teraldev/cli/internal/ui"
)

// App is the state shared by every command of one invocation.
type App struct {
	IO     *ui.IO
	Config *config.Config

	// Global flags.
	Workspace   string
	Project     string
	AppSlug     string
	Environment string
	Yes         bool
	Output      string

	link       *link.Link
	linkLoaded bool
}

// Link returns the link of the current directory, or nil.
func (a *App) Link() *link.Link {
	if !a.linkLoaded {
		a.linkLoaded = true
		cwd, err := os.Getwd()
		if err == nil {
			l, err := link.Find(cwd)
			if err != nil {
				a.IO.Warn("Ignoring the project link: %v", err)
			}
			a.link = l
		}
	}
	return a.link
}

// Credential describes how a client authenticates.
type Credential struct {
	// Source is "api_key" or "session".
	Source string
	// Workspace is the slug of the session's workspace; empty for API keys
	// until resolved through whoami.
	Workspace string
	Account   *config.Account
	Token     string
}

// apiKeyFromEnv returns the CI credential, if set.
func apiKeyFromEnv() string {
	for _, name := range []string{"TERAL_API_KEY", "TERAL_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v
		}
	}
	return ""
}

// Credential picks the credential for this invocation: TERAL_API_KEY wins,
// then the session of the selected workspace.
func (a *App) Credential() (*Credential, error) {
	if key := apiKeyFromEnv(); key != "" {
		return &Credential{Source: "api_key", Token: key}, nil
	}

	slug, err := a.workspaceSlug()
	if err != nil {
		return nil, err
	}
	account := a.Config.Workspaces[slug]
	token, err := a.Config.Token(a.Config.APIEndpoint(), account.WorkspaceID, account.Storage)
	if err != nil {
		if errors.Is(err, config.ErrNoToken) {
			return nil, &UserError{
				Message: fmt.Sprintf("Your session for %s is missing from this machine.", slug),
				Hint:    "Run `teral login` to sign in again.",
			}
		}
		return nil, err
	}
	return &Credential{Source: "session", Workspace: slug, Account: &account, Token: token}, nil
}

// workspaceSlug selects a signed-in workspace: --workspace, then
// $TERAL_WORKSPACE, then the directory's link, then the active one.
func (a *App) workspaceSlug() (string, error) {
	if len(a.Config.Workspaces) == 0 {
		hint := "Run `teral login` to sign in."
		if a.IO.CI() {
			hint = "In CI, set TERAL_API_KEY to an API key from the dashboard (Settings → API Keys)."
		}
		return "", &UserError{Message: "You're not signed in.", Hint: hint}
	}

	candidates := []struct{ slug, from string }{
		{a.Workspace, "--workspace"},
		{os.Getenv("TERAL_WORKSPACE"), "TERAL_WORKSPACE"},
	}
	if l := a.Link(); l != nil {
		candidates = append(candidates, struct{ slug, from string }{l.Workspace, "the project link"})
	}
	candidates = append(candidates, struct{ slug, from string }{a.Config.ActiveWorkspace, "the active workspace"})

	for _, c := range candidates {
		if c.slug == "" {
			continue
		}
		if _, ok := a.Config.Workspaces[c.slug]; ok {
			return c.slug, nil
		}
		return "", &UserError{
			Message: fmt.Sprintf("You're not signed in to workspace %q (from %s).", c.slug, c.from),
			Hint:    fmt.Sprintf("Run `teral login` and approve the login for %s, or pick another workspace with `teral workspace switch`.", c.slug),
		}
	}
	slugs := a.Config.WorkspaceSlugs()
	if len(slugs) == 1 {
		return slugs[0], nil
	}
	return "", &UserError{Message: "Several workspaces are signed in and none is active.", Hint: "Run `teral workspace switch`."}
}

// Client returns an authenticated API client.
func (a *App) Client() (*client.Client, *Credential, error) {
	cred, err := a.Credential()
	if err != nil {
		return nil, nil, err
	}
	c, err := client.New(a.Config.APIEndpoint(), cred.Token, cred.Source)
	if err != nil {
		return nil, nil, err
	}
	return c, cred, nil
}

// Anonymous returns a client without credentials, for logging in.
func (a *App) Anonymous() (*client.Client, error) {
	return client.New(a.Config.APIEndpoint(), "", "")
}

// Target is the project, app, and environment a command acts on.
type Target struct {
	Project     string
	App         string
	Environment string
}

func (t Target) String() string {
	s := t.Project
	if t.App != "" {
		s += "/" + t.App
	}
	if t.Environment != "" {
		s += " (" + t.Environment + ")"
	}
	return s
}

// Need selects how much of a target a command requires.
type Need int

const (
	NeedProject Need = iota
	NeedApp
	NeedEnvironment
)

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// ResolveTarget fills project, app, and environment from flags, then
// $TERAL_PROJECT/$TERAL_APP/$TERAL_ENVIRONMENT, then the directory's link,
// then prompts. Without a terminal, a missing app fails with the flag to
// pass, while a missing environment defaults to production.
func (a *App) ResolveTarget(ctx context.Context, c *client.Client, need Need) (Target, error) {
	var l link.Link
	if found := a.Link(); found != nil {
		l = *found
	}
	t := Target{
		Project:     firstNonEmpty(a.Project, os.Getenv("TERAL_PROJECT"), l.Project),
		App:         firstNonEmpty(a.AppSlug, os.Getenv("TERAL_APP"), l.App),
		Environment: firstNonEmpty(a.Environment, os.Getenv("TERAL_ENVIRONMENT"), l.Environment),
	}

	// A flag naming a different project invalidates the linked app.
	if a.Project != "" && l.Project != "" && a.Project != l.Project && a.AppSlug == "" {
		t.App = firstNonEmpty(os.Getenv("TERAL_APP"))
	}

	if t.Project == "" {
		p, err := a.pickProject(ctx, c)
		if err != nil {
			return t, err
		}
		t.Project = p
	}
	if need >= NeedApp && t.App == "" {
		app, err := a.pickApp(ctx, c, t.Project)
		if err != nil {
			return t, err
		}
		t.App = app
	}
	if need >= NeedEnvironment && t.Environment == "" {
		env, err := a.pickEnvironment(ctx, c, t)
		if err != nil {
			return t, err
		}
		t.Environment = env
	}
	return t, nil
}

func (a *App) pickProject(ctx context.Context, c *client.Client) (string, error) {
	if !a.IO.Interactive() {
		return "", &UserError{Message: "No project selected.", Hint: "Pass --project, set TERAL_PROJECT, or run `teral link` in this directory."}
	}
	projects, err := listProjects(ctx, c)
	if err != nil {
		return "", err
	}
	if len(projects) == 0 {
		return "", &UserError{Message: "This workspace has no projects yet.", Hint: "Create one with `teral init`."}
	}
	options := make([]ui.Option, len(projects))
	for i, p := range projects {
		options[i] = ui.Option{Label: fmt.Sprintf("%s  %s", p.Name, a.IO.Theme.Muted.Render(p.Slug)), Value: p.Slug}
	}
	return a.IO.Select("Project", "--project", options)
}

func (a *App) pickApp(ctx context.Context, c *client.Client, project string) (string, error) {
	if !a.IO.Interactive() {
		return "", &UserError{Message: "No app selected.", Hint: "Pass --app, set TERAL_APP, or run `teral link` in this directory."}
	}
	apps, err := listApps(ctx, c, project)
	if err != nil {
		return "", err
	}
	if len(apps) == 0 {
		return "", &UserError{Message: fmt.Sprintf("Project %s has no apps yet.", project), Hint: "Create one with `teral apps create` or `teral init`."}
	}
	options := make([]ui.Option, len(apps))
	for i, app := range apps {
		options[i] = ui.Option{Label: fmt.Sprintf("%s  %s", app.Name, a.IO.Theme.Muted.Render(app.Slug)), Value: app.Slug}
	}
	return a.IO.Select("App", "--app", options)
}

func (a *App) pickEnvironment(ctx context.Context, c *client.Client, t Target) (string, error) {
	envs, err := listEnvironments(ctx, c, t.Project, t.App)
	if err != nil {
		return "", err
	}
	if len(envs) == 0 {
		return "", &UserError{Message: fmt.Sprintf("App %s has no environments.", t.App)}
	}
	if !a.IO.Interactive() || len(envs) == 1 {
		for _, e := range envs {
			if e.Slug == "production" {
				return e.Slug, nil
			}
		}
		if len(envs) == 1 {
			return envs[0].Slug, nil
		}
		return "", &UserError{Message: "No environment selected.", Hint: "Pass --env or set TERAL_ENVIRONMENT."}
	}
	options := make([]ui.Option, len(envs))
	for i, e := range envs {
		options[i] = ui.Option{Label: fmt.Sprintf("%s  %s", e.Slug, a.IO.Theme.Muted.Render(string(e.Kind))), Value: e.Slug}
	}
	// Production first: it is the usual target.
	sort.SliceStable(options, func(i, j int) bool { return options[i].Value == "production" && options[j].Value != "production" })
	return a.IO.Select("Environment", "--env", options)
}

// ---- small API helpers shared by commands ----

func listProjects(ctx context.Context, c *client.Client) ([]api.Project, error) {
	var out []api.Project
	var cursor *string
	for {
		res, err := client.Call[api.V1ProjectsListProjectsResponseBody](ctx, c, func(ctx context.Context, a *api.Client) (*http.Response, error) {
			return a.ProjectsListProjects(ctx, api.V1ProjectsListProjectsRequestBody{Cursor: cursor, Limit: ptr(100)})
		})
		if err != nil {
			return nil, err
		}
		out = append(out, res.Data...)
		if !res.Pagination.HasMore || res.Pagination.Cursor == nil {
			return out, nil
		}
		cursor = res.Pagination.Cursor
	}
}

func listApps(ctx context.Context, c *client.Client, project string) ([]api.App, error) {
	var out []api.App
	var cursor *string
	for {
		res, err := client.Call[api.V1AppsListAppsResponseBody](ctx, c, func(ctx context.Context, a *api.Client) (*http.Response, error) {
			return a.AppsListApps(ctx, api.V1AppsListAppsRequestBody{Project: project, Cursor: cursor, Limit: ptr(100)})
		})
		if err != nil {
			return nil, err
		}
		out = append(out, res.Data...)
		if !res.Pagination.HasMore || res.Pagination.Cursor == nil {
			return out, nil
		}
		cursor = res.Pagination.Cursor
	}
}

func listEnvironments(ctx context.Context, c *client.Client, project, app string) ([]api.Environment, error) {
	res, err := client.Call[api.V1EnvironmentsListEnvironmentsResponseBody](ctx, c, func(ctx context.Context, a *api.Client) (*http.Response, error) {
		return a.EnvironmentsListEnvironments(ctx, api.V1EnvironmentsListEnvironmentsRequestBody{Project: project, App: app})
	})
	if err != nil {
		return nil, err
	}
	return res.Data, nil
}

func ptr[T any](v T) *T { return &v }
