package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/teraldev/cli/internal/api"
	"github.com/teraldev/cli/internal/client"
)

var envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func listVariables(ctx context.Context, c *client.Client, t Target) ([]api.EnvironmentVariable, error) {
	var out []api.EnvironmentVariable
	var cursor *string
	for {
		res, err := client.Call[api.V1EnvironmentsListEnvironmentVariablesResponseBody](ctx, c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
			return cl.EnvironmentsListEnvironmentVariables(ctx, api.V1EnvironmentsListEnvironmentVariablesRequestBody{
				Project: t.Project, App: t.App, Environment: t.Environment, Cursor: cursor, Limit: ptr(100),
			})
		})
		if err != nil {
			return nil, err
		}
		out = append(out, res.Data...)
		if !res.Pagination.HasMore || res.Pagination.Cursor == nil {
			sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
			return out, nil
		}
		cursor = res.Pagination.Cursor
	}
}

func newEnvCmd(a *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "env",
		Aliases: []string{"variables", "vars"},
		Short:   "Manage environment variables",
		Long: `Read and change an environment's variables. Changes apply to the next
deployment; redeploy with "teral deploy --from <id>" or "teral up" to use them.

Variables are "recoverable" (readable again later) by default. Mark secrets
with --secret to make them write-only: they reach your app but can never be
read back, not even by "teral env pull".`,
	}

	var reveal bool
	list := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List variables (values hidden unless --reveal)",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, _, err := a.Client()
			if err != nil {
				return err
			}
			t, err := a.ResolveTarget(ctx, c, NeedEnvironment)
			if err != nil {
				return err
			}
			vars, err := listVariables(ctx, c, t)
			if err != nil {
				return err
			}
			if a.IO.JSON {
				if !reveal {
					for i := range vars {
						vars[i].Value = ""
					}
				}
				return a.IO.PrintJSON(vars)
			}
			if len(vars) == 0 {
				a.IO.Info("%s has no variables. Add one with %s.", t, a.IO.Code("teral env set KEY=value"))
				return nil
			}
			th := a.IO.Theme
			var rows [][]string
			for _, v := range vars {
				value := th.Muted.Render("••••••••")
				if v.Kind == api.Writeonly {
					value = th.Muted.Render("write-only")
				} else if reveal {
					value = v.Value
				}
				rows = append(rows, []string{v.Key, value, v.Description})
			}
			a.IO.Table([]string{"Key", "Value", "Description"}, rows)
			return nil
		},
	}
	list.Flags().BoolVar(&reveal, "reveal", false, "Show values")
	cmd.AddCommand(list)

	cmd.AddCommand(&cobra.Command{
		Use:   "get <KEY>",
		Short: "Print one variable's value",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, _, err := a.Client()
			if err != nil {
				return err
			}
			t, err := a.ResolveTarget(ctx, c, NeedEnvironment)
			if err != nil {
				return err
			}
			vars, err := listVariables(ctx, c, t)
			if err != nil {
				return err
			}
			for _, v := range vars {
				if v.Key != args[0] {
					continue
				}
				if v.Kind == api.Writeonly {
					return &UserError{Message: fmt.Sprintf("%s is write-only; its value can't be read.", v.Key)}
				}
				if a.IO.JSON {
					return a.IO.PrintJSON(v)
				}
				a.IO.Println(v.Value)
				return nil
			}
			return &UserError{Message: fmt.Sprintf("%s is not set in %s.", args[0], t)}
		},
	})

	var secret bool
	var description, fromFile string
	set := &cobra.Command{
		Use:   "set KEY=value...",
		Short: "Set variables",
		Long: `Set one or more variables. A value of "-" is read from stdin, which keeps it out
of your shell history. --from-file imports a .env file.`,
		Example: `  teral env set LOG_LEVEL=debug
  teral env set DATABASE_URL=- --secret < db-url.txt
  teral env set --from-file .env.production`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			vars, err := parseAssignments(args, fromFile, a.IO.In)
			if err != nil {
				return err
			}
			if len(vars) == 0 {
				return &UserError{Message: "Nothing to set.", Hint: "Pass KEY=value pairs or --from-file."}
			}
			c, _, err := a.Client()
			if err != nil {
				return err
			}
			t, err := a.ResolveTarget(ctx, c, NeedEnvironment)
			if err != nil {
				return err
			}
			kind := api.Recoverable
			if secret {
				kind = api.Writeonly
			}
			inputs := make([]api.EnvironmentVariableInput, 0, len(vars))
			for _, v := range vars {
				in := api.EnvironmentVariableInput{Key: v[0], Value: v[1], Kind: &kind}
				if description != "" {
					in.Description = &description
				}
				inputs = append(inputs, in)
			}
			if _, err := client.Call[api.V1EnvironmentsSetEnvironmentVariablesResponseBody](ctx, c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
				return cl.EnvironmentsSetEnvironmentVariables(ctx, api.V1EnvironmentsSetEnvironmentVariablesRequestBody{
					Project: t.Project, App: t.App, Environment: t.Environment, Variables: inputs,
				})
			}); err != nil {
				return err
			}
			keys := make([]string, len(vars))
			for i, v := range vars {
				keys[i] = v[0]
			}
			a.IO.Success("Set %s in %s", strings.Join(keys, ", "), t)
			a.IO.Hint("Takes effect on the next deployment.")
			return nil
		},
	}
	set.Flags().BoolVar(&secret, "secret", false, "Store as write-only: never readable again")
	set.Flags().StringVar(&description, "description", "", "Description shown in the dashboard")
	set.Flags().StringVar(&fromFile, "from-file", "", "Import variables from a .env file")
	cmd.AddCommand(set)

	cmd.AddCommand(&cobra.Command{
		Use:     "unset <KEY>...",
		Aliases: []string{"rm", "remove"},
		Short:   "Remove variables",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, _, err := a.Client()
			if err != nil {
				return err
			}
			t, err := a.ResolveTarget(ctx, c, NeedEnvironment)
			if err != nil {
				return err
			}
			if err := a.IO.Confirm(fmt.Sprintf("Remove %s from %s?", strings.Join(args, ", "), t), a.Yes); err != nil {
				return err
			}
			if _, err := client.Call[api.V1EnvironmentsRemoveEnvironmentVariablesResponseBody](ctx, c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
				return cl.EnvironmentsRemoveEnvironmentVariables(ctx, api.V1EnvironmentsRemoveEnvironmentVariablesRequestBody{
					Project: t.Project, App: t.App, Environment: t.Environment, Variables: args,
				})
			}); err != nil {
				return err
			}
			a.IO.Success("Removed %s from %s", strings.Join(args, ", "), t)
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "pull [file]",
		Short: "Write variables to a .env file",
		Long: `Write the environment's readable variables to a file (default .env) for local
development. Write-only variables are listed as comments.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			file := ".env"
			if len(args) == 1 {
				file = args[0]
			}
			c, _, err := a.Client()
			if err != nil {
				return err
			}
			t, err := a.ResolveTarget(ctx, c, NeedEnvironment)
			if err != nil {
				return err
			}
			vars, err := listVariables(ctx, c, t)
			if err != nil {
				return err
			}
			out := os.Stdout
			if file != "-" {
				if _, err := os.Stat(file); err == nil {
					if err := a.IO.Confirm(fmt.Sprintf("Overwrite %s?", file), a.Yes); err != nil {
						return err
					}
				}
				f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
				if err != nil {
					return err
				}
				defer func() { _ = f.Close() }()
				out = f
			}
			w := bufio.NewWriter(out)
			fmt.Fprintf(w, "# Pulled from Teral: %s\n", t)
			pulled, skipped := 0, 0
			for _, v := range vars {
				if v.Kind == api.Writeonly {
					fmt.Fprintf(w, "# %s is write-only and can't be pulled\n", v.Key)
					skipped++
					continue
				}
				fmt.Fprintf(w, "%s=%s\n", v.Key, quoteEnv(v.Value))
				pulled++
			}
			if err := w.Flush(); err != nil {
				return err
			}
			if file != "-" {
				a.IO.Success("Wrote %d variables to %s", pulled, file)
				if skipped > 0 {
					a.IO.Hint("%d write-only variables were skipped.", skipped)
				}
				if !gitIgnores(file) {
					a.IO.Warn("%s is not ignored by git. Add it to .gitignore so secrets aren't committed.", file)
				}
			}
			return nil
		},
	})
	return cmd
}

// parseAssignments reads KEY=value arguments and an optional .env file.
func parseAssignments(args []string, fromFile string, stdin io.Reader) ([][2]string, error) {
	var out [][2]string
	if fromFile != "" {
		f, err := os.Open(fromFile)
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		parsed, err := parseDotenv(f)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", fromFile, err)
		}
		out = append(out, parsed...)
	}
	for _, arg := range args {
		key, value, ok := strings.Cut(arg, "=")
		if !ok {
			return nil, &UserError{Message: fmt.Sprintf("%q is not KEY=value.", arg)}
		}
		if value == "-" {
			raw, err := io.ReadAll(stdin)
			if err != nil {
				return nil, err
			}
			value = strings.TrimRight(string(raw), "\r\n")
		}
		out = append(out, [2]string{key, value})
	}
	for _, kv := range out {
		if !envKeyPattern.MatchString(kv[0]) {
			return nil, &UserError{Message: fmt.Sprintf("%q is not a valid variable name.", kv[0]), Hint: "Use letters, digits, and underscores, not starting with a digit."}
		}
	}
	return out, nil
}

// parseDotenv reads KEY=value lines, with optional "export", quotes, and
// comments.
func parseDotenv(r io.Reader) ([][2]string, error) {
	var out [][2]string
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		text = strings.TrimPrefix(text, "export ")
		key, value, ok := strings.Cut(text, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: expected KEY=value", line)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		switch {
		case len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"':
			if unquoted, err := strconv.Unquote(value); err == nil {
				value = unquoted
			} else {
				value = value[1 : len(value)-1]
			}
		case len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'':
			value = value[1 : len(value)-1]
		default:
			if i := strings.Index(value, " #"); i >= 0 {
				value = strings.TrimSpace(value[:i])
			}
		}
		out = append(out, [2]string{key, value})
	}
	return out, scanner.Err()
}

// quoteEnv quotes a value for a .env file when needed.
func quoteEnv(v string) string {
	if v == "" {
		return `""`
	}
	if strings.ContainsAny(v, " \t\n\r\"'#$\\=") {
		return strconv.Quote(v)
	}
	return v
}

func gitIgnores(file string) bool {
	cmd := exec.Command("git", "check-ignore", "-q", file)
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 128 {
		return true // not a git checkout: nothing to warn about
	}
	return err == nil
}

func newRunCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:   "run -- <command> [args...]",
		Short: "Run a local command with the app's variables",
		Long: `Run a command on your machine with the environment's variables set, for example
a dev server or a database migration. Teral's values override variables of
the same name in your shell. Write-only variables can't be read and are
not set.`,
		Example: "  teral run -- npm run dev\n  teral run -e preview -- ./migrate up",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, _, err := a.Client()
			if err != nil {
				return err
			}
			t, err := a.ResolveTarget(ctx, c, NeedEnvironment)
			if err != nil {
				return err
			}
			vars, err := listVariables(ctx, c, t)
			if err != nil {
				return err
			}
			env := os.Environ()
			var skipped []string
			for _, v := range vars {
				if v.Kind == api.Writeonly {
					skipped = append(skipped, v.Key)
					continue
				}
				env = append(env, v.Key+"="+v.Value)
			}
			if len(skipped) > 0 {
				a.IO.Warn("Write-only variables are not available locally: %s", strings.Join(skipped, ", "))
			}

			child := exec.Command(args[0], args[1:]...)
			child.Env = env
			child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
			// The child gets Ctrl+C from the terminal itself; don't die first.
			signal.Ignore(os.Interrupt)
			if err := child.Run(); err != nil {
				var exit *exec.ExitError
				if errors.As(err, &exit) {
					return &exitError{code: exit.ExitCode()}
				}
				return &UserError{Message: fmt.Sprintf("Couldn't run %s: %v", args[0], err)}
			}
			return nil
		},
	}
}
