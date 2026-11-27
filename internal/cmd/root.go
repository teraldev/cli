// Package cmd implements the teral commands.
package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/teraldev/cli/internal/client"
	"github.com/teraldev/cli/internal/config"
	"github.com/teraldev/cli/internal/ui"
	"github.com/teraldev/cli/internal/version"
)

// UserError is an expected failure with advice, printed without a stack of
// wrapped causes.
type UserError struct {
	Message string
	Hint    string
}

func (e *UserError) Error() string { return e.Message }

// exitError carries an exit code from a subprocess (`teral run`).
type exitError struct{ code int }

func (e *exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

// Execute runs the CLI and returns the process exit code.
func Execute(ctx context.Context) int {
	return Run(ctx, ui.System(), os.Args[1:])
}

// Run executes one command line on io and returns the exit code.
func Run(ctx context.Context, io *ui.IO, args []string) int {
	cfg, err := config.Load()
	if err != nil {
		io.Warn("%v", err)
		cfg = &config.Config{Workspaces: map[string]config.Account{}}
	}
	app := &App{IO: io, Config: cfg}
	root := newRoot(app)
	root.SetArgs(args)
	root.SetOut(io.Out)
	root.SetErr(io.Err)

	err = root.ExecuteContext(ctx)
	if err == nil {
		return 0
	}
	return app.printError(err)
}

func (a *App) printError(err error) int {
	var exit *exitError
	if errors.As(err, &exit) {
		return exit.code
	}
	if errors.Is(err, ui.ErrCanceled) || errors.Is(err, context.Canceled) {
		fmt.Fprintln(a.IO.Err, a.IO.ErrTheme.Muted.Render("Canceled."))
		return 130
	}

	t := a.IO.ErrTheme
	var userErr *UserError
	var apiErr *client.Error
	var nonInteractive *ui.NonInteractiveError
	switch {
	case errors.As(err, &userErr):
		fmt.Fprintf(a.IO.Err, "%s %s\n", t.Danger.Render("✗"), userErr.Message)
		if userErr.Hint != "" {
			fmt.Fprintf(a.IO.Err, "  %s\n", t.Muted.Render(userErr.Hint))
		}
	case errors.As(err, &apiErr):
		fmt.Fprintf(a.IO.Err, "%s %s\n", t.Danger.Render("✗"), apiErr.Error())
		if apiErr.Hint != "" {
			fmt.Fprintf(a.IO.Err, "  %s\n", t.Muted.Render(apiErr.Hint))
		}
		if apiErr.RequestID != "" && apiErr.Status >= 500 {
			fmt.Fprintf(a.IO.Err, "  %s\n", t.Muted.Render("Request id: "+apiErr.RequestID))
		}
	case errors.As(err, &nonInteractive):
		fmt.Fprintf(a.IO.Err, "%s %s\n", t.Danger.Render("✗"), nonInteractive.Error())
	default:
		fmt.Fprintf(a.IO.Err, "%s %s\n", t.Danger.Render("✗"), capitalize(err.Error()))
	}
	return 1
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func newRoot(a *App) *cobra.Command {
	root := &cobra.Command{
		Use:   "teral",
		Short: "Deploy and manage apps on Teral",
		Long: `Deploy and manage apps on Teral from your terminal.

Get started:
  teral login        Sign in with your browser
  teral init         Create a project and app for this directory
  teral up           Deploy this directory

In CI, set TERAL_API_KEY instead of logging in.`,
		Version:       version.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			switch a.Output {
			case "", "text":
			case "json":
				a.IO.JSON = true
			default:
				return &UserError{Message: fmt.Sprintf("Unknown output format %q.", a.Output), Hint: "Use --output text or --output json."}
			}
			if v := os.Getenv("TERAL_OUTPUT"); v == "json" && !cmd.Flags().Changed("output") {
				a.IO.JSON = true
			}
			return nil
		},
	}
	root.SetVersionTemplate(fmt.Sprintf("teral %s (%s, %s)\n", version.Version, version.Commit, version.Date))

	f := root.PersistentFlags()
	f.StringVarP(&a.Output, "output", "o", "text", "Output format: text or json")
	f.StringVarP(&a.Workspace, "workspace", "w", "", "Workspace slug (default: linked or active workspace)")
	f.StringVarP(&a.Project, "project", "p", "", "Project id or slug (default: linked project)")
	f.StringVarP(&a.AppSlug, "app", "a", "", "App id or slug (default: linked app)")
	f.StringVarP(&a.Environment, "env", "e", "", "Environment slug (default: linked environment, else production)")
	f.BoolVarP(&a.Yes, "yes", "y", false, "Skip confirmation prompts")
	f.BoolVarP(&a.IO.Quiet, "quiet", "q", false, "Print only essential output")

	root.AddGroup(
		&cobra.Group{ID: "start", Title: "Getting started:"},
		&cobra.Group{ID: "deploy", Title: "Deploy and operate:"},
		&cobra.Group{ID: "manage", Title: "Manage resources:"},
		&cobra.Group{ID: "account", Title: "Account:"},
	)

	add := func(group string, cmds ...*cobra.Command) {
		for _, c := range cmds {
			c.GroupID = group
			root.AddCommand(c)
		}
	}
	add("start", newLoginCmd(a), newInitCmd(a), newLinkCmd(a), newUnlinkCmd(a), newStatusCmd(a))
	add("deploy", newUpCmd(a), newDeployCmd(a), newDeploymentsCmd(a), newLogsCmd(a), newEnvCmd(a), newRunCmd(a), newOpenCmd(a))
	add("manage", newProjectsCmd(a), newAppsCmd(a), newDomainsCmd(a), newDatabasesCmd(a), newKeysCmd(a), newConfigCmd(a))
	add("account", newLogoutCmd(a), newWhoamiCmd(a), newWorkspaceCmd(a), newSessionsCmd(a))
	root.AddCommand(newVersionCmd(a), newDocsCmd(a), newUpgradeCmd(a))
	root.SetHelpCommandGroupID("account")
	root.SetCompletionCommandGroupID("account")
	return root
}

func newVersionCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the CLI version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if a.IO.JSON {
				return a.IO.PrintJSON(map[string]string{"version": version.Version, "commit": version.Commit, "date": version.Date})
			}
			a.IO.Printf("teral %s (%s, %s)\n", version.Version, version.Commit, version.Date)
			return nil
		},
	}
}

func newDocsCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:   "docs",
		Short: "Open the CLI documentation",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.openURL("https://docs.teral.dev/cli")
		},
	}
}
