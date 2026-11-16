package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/pkg/browser"
	"github.com/spf13/cobra"

	"github.com/teraldev/cli/internal/api"
	"github.com/teraldev/cli/internal/client"
	"github.com/teraldev/cli/internal/config"
	"github.com/teraldev/cli/internal/ui"
	"github.com/teraldev/cli/internal/version"
)

func newLoginCmd(a *App) *cobra.Command {
	var noBrowser bool
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Sign in with your browser",
		Long: `Sign in to a Teral workspace.

teral login shows a one-time code and opens the dashboard, where you confirm
the code and approve access for the workspace you're in. Run it again to add
more workspaces; switch between them with "teral workspace switch".

Over SSH or in a container, pass --no-browser and open the link on any device.
In CI, don't log in: set TERAL_API_KEY instead.`,
		Example: "  teral login\n  teral login --no-browser",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.login(cmd.Context(), noBrowser)
		},
	}
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "Print the link instead of opening a browser")
	return cmd
}

func (a *App) login(ctx context.Context, noBrowser bool) error {
	if a.IO.CI() {
		return &UserError{
			Message: "teral login needs a person to approve it in the browser, which CI can't do.",
			Hint:    "Set TERAL_API_KEY to an API key instead (dashboard → Settings → API Keys).",
		}
	}
	if apiKeyFromEnv() != "" {
		a.IO.Warn("TERAL_API_KEY is set and takes precedence over the session you're about to create.")
	}

	c, err := a.Anonymous()
	if err != nil {
		return err
	}
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "unknown device"
	}
	start, err := client.Call[api.V1CliStartDeviceAuthorizationResponseBody](ctx, c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
		return cl.CliStartDeviceAuthorization(ctx, api.V1CliStartDeviceAuthorizationRequestBody{
			ClientName: version.Client(),
			DeviceName: truncate(hostname, 128),
			DeviceOs:   runtime.GOOS + "/" + runtime.GOARCH,
		})
	})
	if err != nil {
		return err
	}
	d := start.Data

	t := a.IO.ErrTheme
	fmt.Fprintf(a.IO.Err, "\n  Your one-time code:  %s\n\n", t.Bold.Render(d.UserCode))
	opened := false
	if !noBrowser && canOpenBrowser() {
		opened = browser.OpenURL(d.VerificationUriComplete) == nil
	}
	if opened {
		fmt.Fprintf(a.IO.Err, "  Opened %s\n", a.IO.Link(d.VerificationUriComplete))
		fmt.Fprintf(a.IO.Err, "  %s\n\n", t.Muted.Render("Check that the code matches, then approve. The browser didn't open? Use the link above."))
	} else {
		fmt.Fprintf(a.IO.Err, "  Open this link on any device and confirm the code:\n  %s\n\n", a.IO.Link(d.VerificationUriComplete))
	}

	spin := a.IO.Spin("Waiting for approval in the browser…")
	creds, err := a.pollLogin(ctx, c, d.DeviceCode, d.Interval, time.Duration(d.ExpiresIn)*time.Second)
	spin.Stop()
	if err != nil {
		return err
	}

	return a.saveSession(ctx, creds)
}

func (a *App) pollLogin(ctx context.Context, c *client.Client, deviceCode string, interval int, lifetime time.Duration) (*api.CliSessionCredentials, error) {
	deadline := time.Now().Add(lifetime)
	if interval <= 0 {
		interval = 5
	}
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(interval) * time.Second):
		}
		if time.Now().After(deadline) {
			return nil, &UserError{Message: "The login code expired before it was approved.", Hint: "Run `teral login` again."}
		}

		res, err := client.Call[api.V1CliPollDeviceAuthorizationResponseBody](ctx, c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
			return cl.CliPollDeviceAuthorization(ctx, api.V1CliPollDeviceAuthorizationRequestBody{DeviceCode: deviceCode})
		})
		if err != nil {
			// Keep polling through blips; the code stays valid.
			var apiErr *client.Error
			if errors.As(err, &apiErr) && apiErr.Status < 500 && apiErr.Status != http.StatusTooManyRequests {
				return nil, err
			}
			interval = min(interval+5, 30)
			continue
		}
		switch res.Data.Status {
		case api.V1CliPollDeviceAuthorizationResponseDataStatusApproved:
			if res.Data.Session == nil {
				return nil, errors.New("the API approved the login but returned no session")
			}
			return res.Data.Session, nil
		case api.V1CliPollDeviceAuthorizationResponseDataStatusDenied:
			return nil, &UserError{Message: "The login was denied in the browser."}
		case api.V1CliPollDeviceAuthorizationResponseDataStatusExpired:
			return nil, &UserError{Message: "The login code expired.", Hint: "Run `teral login` again."}
		case api.V1CliPollDeviceAuthorizationResponseDataStatusSlowDown, api.V1CliPollDeviceAuthorizationResponseDataStatusPending:
			if res.Data.Interval != nil && *res.Data.Interval > 0 {
				interval = *res.Data.Interval
			}
		}
	}
}

func (a *App) saveSession(ctx context.Context, creds *api.CliSessionCredentials) error {
	apiURL := a.Config.APIEndpoint()
	slug := creds.Workspace.Slug

	// Replacing a session for the same workspace: end the old one.
	if old, ok := a.Config.Workspaces[slug]; ok && old.SessionID != creds.SessionId {
		if token, err := a.Config.Token(apiURL, old.WorkspaceID, old.Storage); err == nil {
			if c, err := client.New(apiURL, token, "session"); err == nil {
				_ = revokeSession(ctx, c, "")
			}
		}
		_ = a.Config.DeleteToken(apiURL, old.WorkspaceID)
	}

	storage, err := a.Config.StoreToken(apiURL, creds.Workspace.Id, creds.Token)
	if err != nil {
		return fmt.Errorf("save the session token: %w", err)
	}
	a.Config.Workspaces[slug] = config.Account{
		WorkspaceID:   creds.Workspace.Id,
		WorkspaceName: creds.Workspace.Name,
		SessionID:     creds.SessionId,
		UserID:        creds.User.Id,
		UserName:      creds.User.Name,
		Email:         creds.User.Email,
		ExpiresAt:     creds.ExpiresAt,
		Storage:       storage,
	}
	a.Config.ActiveWorkspace = slug
	if err := a.Config.Save(); err != nil {
		return err
	}

	if a.IO.JSON {
		return a.IO.PrintJSON(map[string]any{
			"workspace": creds.Workspace,
			"user":      creds.User,
			"sessionId": creds.SessionId,
			"expiresAt": creds.ExpiresAt,
		})
	}
	who := creds.User.Name
	if creds.User.Email != "" {
		who = creds.User.Email
	}
	a.IO.Success("Logged in to %s as %s", a.IO.ErrTheme.Bold.Render(creds.Workspace.Name), who)
	if storage == "file" {
		a.IO.Hint("No system keychain available; the token is stored in %s (readable only by you).", strings.TrimSuffix(a.Config.Path(), "config.toml")+"credentials.toml")
	}
	if len(a.Config.Workspaces) > 1 {
		a.IO.Hint("Switched the active workspace to %s. Switch back with `teral workspace switch`.", slug)
	}
	if a.Link() == nil {
		a.IO.Hint("Next: run `teral link` in your project, or `teral init` to create one.")
	}
	return nil
}

// canOpenBrowser is false in SSH sessions and headless Linux, where a
// browser would open somewhere the user can't see.
func canOpenBrowser() bool {
	if os.Getenv("SSH_CONNECTION") != "" || os.Getenv("SSH_TTY") != "" {
		return false
	}
	if runtime.GOOS == "linux" && os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		return false
	}
	return true
}

func (a *App) openURL(url string) error {
	if canOpenBrowser() && browser.OpenURL(url) == nil {
		a.IO.Success("Opened %s", a.IO.Link(url))
		return nil
	}
	a.IO.Println(url)
	return nil
}

func newLogoutCmd(a *App) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "logout",
		Short: "Sign out and revoke this machine's session",
		Long: `Sign out of the active workspace (or --workspace), revoking the CLI session
so its token stops working everywhere. --all signs out of every workspace.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if len(a.Config.Workspaces) == 0 {
				a.IO.Info("You're not signed in.")
				return nil
			}
			var slugs []string
			if all {
				slugs = a.Config.WorkspaceSlugs()
			} else {
				slug, err := a.workspaceSlug()
				if err != nil {
					return err
				}
				slugs = []string{slug}
			}
			apiURL := a.Config.APIEndpoint()
			for _, slug := range slugs {
				account := a.Config.Workspaces[slug]
				if token, err := a.Config.Token(apiURL, account.WorkspaceID, account.Storage); err == nil {
					if c, err := client.New(apiURL, token, "session"); err == nil {
						if err := revokeSession(cmd.Context(), c, ""); err != nil && !client.IsStatus(err, http.StatusUnauthorized) {
							a.IO.Warn("Couldn't revoke the session for %s on the server: %v", slug, err)
						}
					}
				}
				if err := a.Config.DeleteToken(apiURL, account.WorkspaceID); err != nil {
					a.IO.Warn("Couldn't remove the stored token for %s: %v", slug, err)
				}
				delete(a.Config.Workspaces, slug)
				a.IO.Success("Logged out of %s", account.WorkspaceName)
			}
			if _, ok := a.Config.Workspaces[a.Config.ActiveWorkspace]; !ok {
				a.Config.ActiveWorkspace = ""
				if remaining := a.Config.WorkspaceSlugs(); len(remaining) > 0 {
					a.Config.ActiveWorkspace = remaining[0]
					a.IO.Hint("Active workspace is now %s.", remaining[0])
				}
			}
			return a.Config.Save()
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "Sign out of every workspace")
	return cmd
}

func revokeSession(ctx context.Context, c *client.Client, sessionID string) error {
	var id *string
	if sessionID != "" {
		id = &sessionID
	}
	_, err := client.Call[api.V1CliRevokeSessionResponseBody](ctx, c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
		return cl.CliRevokeSession(ctx, api.V1CliRevokeSessionRequestBody{SessionId: id})
	})
	return err
}

func newWhoamiCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show who you're signed in as",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, cred, err := a.Client()
			if err != nil {
				return err
			}
			res, err := client.Call[api.V1AuthWhoamiResponseBody](cmd.Context(), c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
				return cl.AuthWhoami(ctx, api.AuthWhoamiJSONRequestBody{})
			})
			if err != nil {
				return err
			}
			if a.IO.JSON {
				return a.IO.PrintJSON(res.Data)
			}
			d := res.Data
			t := a.IO.Theme
			var who, via string
			switch d.Principal.Type {
			case api.V1AuthWhoamiResponseDataPrincipalTypeApiKey:
				who = fmt.Sprintf("API key %s", d.Principal.Name)
				via = "TERAL_API_KEY"
			default:
				who = d.Principal.Name
				if d.Principal.Email != "" {
					who += " " + t.Muted.Render("<"+d.Principal.Email+">")
				}
				via = "teral login"
				if cred.Account != nil && cred.Account.Storage == "file" {
					via += " (token in credentials file)"
				}
			}
			roles := ""
			if d.Roles != nil {
				roles = strings.Join(*d.Roles, ", ")
			}
			expires := ""
			if d.Principal.ExpiresAt != nil {
				expires = ui.Ago(*d.Principal.ExpiresAt) + t.Muted.Render(" (extends with use)")
				if d.Principal.Type == api.V1AuthWhoamiResponseDataPrincipalTypeApiKey {
					expires = ui.Ago(*d.Principal.ExpiresAt)
				}
			}
			a.IO.KV([][2]string{
				{"User", who},
				{"Workspace", fmt.Sprintf("%s %s", d.Workspace.Name, t.Muted.Render("("+d.Workspace.Slug+")"))},
				{"Role", roles},
				{"Signed in via", via},
				{"Expires", expires},
				{"API", a.Config.APIEndpoint()},
			})
			return nil
		},
	}
}

func newWorkspaceCmd(a *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "workspace",
		Aliases: []string{"workspaces", "ws"},
		Short:   "List and switch signed-in workspaces",
	}
	cmd.AddCommand(&cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the workspaces you're signed in to",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			slugs := a.Config.WorkspaceSlugs()
			if a.IO.JSON {
				type row struct {
					Slug   string `json:"slug"`
					ID     string `json:"id"`
					Name   string `json:"name"`
					User   string `json:"user"`
					Active bool   `json:"active"`
				}
				rows := []row{}
				for _, s := range slugs {
					acc := a.Config.Workspaces[s]
					rows = append(rows, row{s, acc.WorkspaceID, acc.WorkspaceName, firstNonEmpty(acc.Email, acc.UserName), s == a.Config.ActiveWorkspace})
				}
				return a.IO.PrintJSON(rows)
			}
			if len(slugs) == 0 {
				a.IO.Info("You're not signed in. Run %s.", a.IO.Code("teral login"))
				return nil
			}
			var rows [][]string
			for _, s := range slugs {
				acc := a.Config.Workspaces[s]
				marker := "  "
				if s == a.Config.ActiveWorkspace {
					marker = a.IO.Theme.Accent.Render("● ")
				}
				rows = append(rows, []string{marker + s, acc.WorkspaceName, firstNonEmpty(acc.Email, acc.UserName)})
			}
			a.IO.Table([]string{"  Slug", "Name", "Signed in as"}, rows)
			a.IO.Hint("Add a workspace with `teral login` while it's selected in the dashboard.")
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "switch [slug]",
		Short: "Change the active workspace",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			slugs := a.Config.WorkspaceSlugs()
			if len(slugs) == 0 {
				return &UserError{Message: "You're not signed in.", Hint: "Run `teral login`."}
			}
			var slug string
			if len(args) == 1 {
				slug = args[0]
			} else {
				options := make([]ui.Option, len(slugs))
				for i, s := range slugs {
					options[i] = ui.Option{Label: fmt.Sprintf("%s  %s", a.Config.Workspaces[s].WorkspaceName, a.IO.Theme.Muted.Render(s)), Value: s}
				}
				var err error
				slug, err = a.IO.Select("Workspace", "a workspace slug argument", options)
				if err != nil {
					return err
				}
			}
			acc, ok := a.Config.Workspaces[slug]
			if !ok {
				return &UserError{Message: fmt.Sprintf("You're not signed in to %q.", slug), Hint: "Run `teral login` and approve it for that workspace."}
			}
			a.Config.ActiveWorkspace = slug
			if err := a.Config.Save(); err != nil {
				return err
			}
			a.IO.Success("Active workspace: %s", acc.WorkspaceName)
			return nil
		},
	})
	return cmd
}

func newSessionsCmd(a *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "sessions",
		Aliases: []string{"session"},
		Short:   "List and revoke CLI sessions",
	}
	var allUsers bool
	list := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List your CLI sessions in this workspace",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := a.Client()
			if err != nil {
				return err
			}
			var sessions []api.CliSession
			var cursor *string
			for {
				res, err := client.Call[api.V1CliListSessionsResponseBody](cmd.Context(), c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
					return cl.CliListSessions(ctx, api.V1CliListSessionsRequestBody{AllUsers: &allUsers, Cursor: cursor})
				})
				if err != nil {
					return err
				}
				sessions = append(sessions, res.Data...)
				if !res.Pagination.HasMore || res.Pagination.Cursor == nil {
					break
				}
				cursor = res.Pagination.Cursor
			}
			if a.IO.JSON {
				return a.IO.PrintJSON(sessions)
			}
			if len(sessions) == 0 {
				a.IO.Info("No active CLI sessions.")
				return nil
			}
			var rows [][]string
			for _, s := range sessions {
				device := s.DeviceName
				if s.Current {
					device += a.IO.Theme.Accent.Render(" (this device)")
				}
				row := []string{s.Id, device, s.DeviceOs, ui.Ago(s.LastUsedAt), ui.Ago(s.ExpiresAt)}
				if allUsers {
					row = append(row, firstNonEmpty(s.UserEmail, s.UserName))
				}
				rows = append(rows, row)
			}
			headers := []string{"ID", "Device", "OS", "Last used", "Expires"}
			if allUsers {
				headers = append(headers, "User")
			}
			a.IO.Table(headers, rows)
			return nil
		},
	}
	list.Flags().BoolVar(&allUsers, "all-users", false, "Include other members' sessions (admins only)")
	cmd.AddCommand(list)
	cmd.AddCommand(&cobra.Command{
		Use:   "revoke <session-id>",
		Short: "Revoke a CLI session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := a.Client()
			if err != nil {
				return err
			}
			if err := a.IO.Confirm(fmt.Sprintf("Revoke session %s? The CLI using it will be signed out.", args[0]), a.Yes); err != nil {
				return err
			}
			if err := revokeSession(cmd.Context(), c, args[0]); err != nil {
				return err
			}
			a.IO.Success("Revoked %s", args[0])
			return nil
		},
	})
	return cmd
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
