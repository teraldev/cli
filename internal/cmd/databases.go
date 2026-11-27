package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/teraldev/cli/internal/api"
	"github.com/teraldev/cli/internal/client"
	"github.com/teraldev/cli/internal/ui"
)

// dbPollInterval is how often --wait checks on a database or backup.
var dbPollInterval = 2 * time.Second

func newDatabasesCmd(a *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "db",
		Aliases: []string{"databases", "database"},
		Short:   "Create and manage databases",
		Long: `Create managed Postgres databases, link them to apps, and manage their
backups. A database belongs to a project and is addressed by its name in the
project (from --project or the directory's link) or by its id (db_…).`,
	}
	cmd.AddCommand(
		newDBListCmd(a), newDBCreateCmd(a), newDBGetCmd(a), newDBDeleteCmd(a), newDBResizeCmd(a), newDBSizesCmd(a),
		newDBCredentialsCmd(a), newDBRotateCmd(a), newDBConnectCmd(a),
		newDBLinkCmd(a), newDBUnlinkCmd(a),
		newDBBackupsCmd(a), newDBBackupCmd(a), newDBBackupScheduleCmd(a), newDBRestoreCmd(a),
	)
	return cmd
}

func newDBListCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List databases",
		Long:    "List the project's databases, or every project's when no project is selected.",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, _, err := a.Client()
			if err != nil {
				return err
			}
			var project string
			if l := a.Link(); l != nil {
				project = l.Project
			}
			project = firstNonEmpty(a.Project, os.Getenv("TERAL_PROJECT"), project)
			res, err := client.Call[api.V1DatabasesListDatabasesResponseBody](ctx, c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
				return cl.DatabasesListDatabases(ctx, api.V1DatabasesListDatabasesRequestBody{Project: nonEmpty(project)})
			})
			if err != nil {
				return err
			}
			if a.IO.JSON {
				return a.IO.PrintJSON(res.Data)
			}
			if len(res.Data) == 0 {
				a.IO.Info("No databases yet. Create one with %s.", a.IO.Code("teral db create main"))
				return nil
			}
			headers := []string{"Name", "Engine", "Size", "Instances", "Status", "Created"}
			projects := map[string]string{}
			if project == "" {
				headers = append([]string{"Project"}, headers...)
				if list, err := listProjects(ctx, c); err == nil {
					for _, p := range list {
						projects[p.Id] = p.Slug
					}
				}
			}
			var rows [][]string
			for _, d := range res.Data {
				row := []string{d.Name, dbEngine(d), d.Size, strconv.Itoa(int(d.Instances)), a.IO.Theme.Status(string(d.Status)), ui.Ago(d.CreatedAt)}
				if project == "" {
					row = append([]string{firstNonEmpty(projects[d.ProjectId], d.ProjectId)}, row...)
				}
				rows = append(rows, row)
			}
			a.IO.Table(headers, rows)
			return nil
		},
	}
}

func newDBCreateCmd(a *App) *cobra.Command {
	var size, version string
	var instances int32
	var wait bool
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a Postgres database",
		Long: `Create a database in the project. It is provisioned in the background; --wait
waits until it accepts connections. See the sizes your plan allows with
"teral db sizes".`,
		Example: "  teral db create main\n  teral db create main --size db-small --instances 2 --wait",
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
			body := api.V1DatabasesCreateDatabaseRequestBody{Project: t.Project, Name: args[0], Size: nonEmpty(size), Version: nonEmpty(version)}
			if instances > 0 {
				body.Instances = &instances
			}
			res, err := client.Call[api.V1DatabasesCreateDatabaseResponseBody](ctx, c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
				return cl.DatabasesCreateDatabase(ctx, body)
			})
			if err != nil {
				return err
			}
			db := res.Data
			if wait {
				a.IO.Info("Database %s %s", a.IO.ErrTheme.Bold.Render(db.Name), a.IO.ErrTheme.Muted.Render("("+db.Id+")"))
				final, err := a.waitDatabase(ctx, c, db.Id, "Created")
				if err != nil {
					return err
				}
				db = *final
			}
			if a.IO.JSON {
				return a.IO.PrintJSON(db)
			}
			if !wait {
				a.IO.Success("Creating database %s in %s %s", db.Name, t.Project, a.IO.ErrTheme.Muted.Render("("+db.Id+")"))
				a.IO.Hint("Check on it with `teral db get %s`, or pass --wait next time.", db.Name)
			}
			a.IO.Hint("Link it to an app with `teral db link %s --app <app>`.", db.Name)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&size, "size", "", "Size slug from teral db sizes (default: the smallest)")
	f.StringVar(&version, "version", "", "Postgres major version (default: the newest)")
	f.Int32Var(&instances, "instances", 0, "Instances: one primary plus replicas (default 1)")
	f.BoolVar(&wait, "wait", false, "Wait until the database is ready")
	return cmd
}

func newDBGetCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:     "get <db>",
		Aliases: []string{"info", "show"},
		Short:   "Show a database, its links and recent backups",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, _, err := a.Client()
			if err != nil {
				return err
			}
			project, err := a.dbProject(ctx, c, args[0])
			if err != nil {
				return err
			}
			db, err := getDatabase(ctx, c, project, args[0])
			if err != nil {
				return err
			}
			backups, _ := listBackups(ctx, c, db.Id)
			if a.IO.JSON {
				return a.IO.PrintJSON(map[string]any{"database": db, "backups": backups})
			}
			a.printDatabase(*db)
			if len(db.Links) > 0 {
				names := a.linkNames(ctx, c, db.ProjectId, db.Links)
				a.IO.Println()
				var rows [][]string
				for _, l := range db.Links {
					n := names[l.Id]
					rows = append(rows, []string{l.Id, n[0], n[1], l.EnvKey, ui.Ago(l.CreatedAt)})
				}
				a.IO.Table([]string{"Link", "App", "Environment", "Variable", "Linked"}, rows)
			}
			if len(backups) > 0 {
				a.IO.Println()
				a.backupTable(backups[:min(len(backups), 5)])
				if len(backups) > 5 {
					a.IO.Hint("%d more; list them with `teral db backups %s`.", len(backups)-5, args[0])
				}
			}
			return nil
		},
	}
}

func newDBDeleteCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:     "delete <db>",
		Aliases: []string{"rm", "remove"},
		Short:   "Delete a database and its backups",
		Long: `Delete a database with all its data and backups. Linked apps keep their
variable but can no longer connect. You confirm by typing the database's name;
--yes skips that.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, _, err := a.Client()
			if err != nil {
				return err
			}
			project, err := a.dbProject(ctx, c, args[0])
			if err != nil {
				return err
			}
			db, err := getDatabase(ctx, c, project, args[0])
			if err != nil {
				return err
			}
			if !a.Yes {
				if !a.IO.Interactive() {
					return &ui.NonInteractiveError{What: "confirmation", Flag: "--yes"}
				}
				a.IO.Warn("This deletes %s with all its data and backups. It can't be undone.", a.IO.ErrTheme.Bold.Render(db.Name))
				typed, err := a.IO.Input(fmt.Sprintf("Type %s to confirm", db.Name), "--yes", db.Name, nil)
				if err != nil {
					return err
				}
				if strings.TrimSpace(typed) != db.Name {
					return &UserError{Message: "The name didn't match; nothing was deleted."}
				}
			}
			if _, err := client.Call[api.V1DatabasesDeleteDatabaseResponseBody](ctx, c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
				return cl.DatabasesDeleteDatabase(ctx, api.V1DatabasesDeleteDatabaseRequestBody{Database: db.Id})
			}); err != nil {
				return err
			}
			a.IO.Success("Deleting database %s", db.Name)
			return nil
		},
	}
}

func newDBResizeCmd(a *App) *cobra.Command {
	var size string
	var instances int32
	var wait bool
	cmd := &cobra.Command{
		Use:   "resize <db>",
		Short: "Change a database's size or instance count",
		Long: `Change a database's size, its instance count (one primary plus replicas), or
both. Storage can't shrink, so a new size needs at least the current storage.
With replicas, the resize switches over to an updated replica.`,
		Example: "  teral db resize main --size db-medium\n  teral db resize main --instances 2 --wait",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if size == "" && instances <= 0 {
				return &UserError{Message: "Nothing to change.", Hint: "Pass --size, --instances, or both."}
			}
			ctx := cmd.Context()
			c, _, err := a.Client()
			if err != nil {
				return err
			}
			project, err := a.dbProject(ctx, c, args[0])
			if err != nil {
				return err
			}
			body := api.V1DatabasesUpdateDatabaseRequestBody{Project: project, Database: args[0], Size: nonEmpty(size)}
			if instances > 0 {
				body.Instances = &instances
			}
			res, err := client.Call[api.V1DatabasesUpdateDatabaseResponseBody](ctx, c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
				return cl.DatabasesUpdateDatabase(ctx, body)
			})
			if err != nil {
				return err
			}
			db := res.Data
			if wait {
				final, err := a.waitDatabase(ctx, c, db.Id, "Resized")
				if err != nil {
					return err
				}
				db = *final
			}
			if a.IO.JSON {
				return a.IO.PrintJSON(db)
			}
			if !wait {
				a.IO.Success("Resizing %s to %s with %d %s", db.Name, db.Size, db.Instances, plural(int(db.Instances), "instance", "instances"))
				a.IO.Hint("Check on it with `teral db get %s`.", db.Name)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&size, "size", "", "New size slug from teral db sizes")
	f.Int32Var(&instances, "instances", 0, "New instance count: one primary plus replicas")
	f.BoolVar(&wait, "wait", false, "Wait until the change is applied")
	return cmd
}

func newDBSizesCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:     "sizes",
		Aliases: []string{"options"},
		Short:   "List database sizes and your plan's limits",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := a.Client()
			if err != nil {
				return err
			}
			res, err := client.Call[api.V1DatabasesListOptionsResponseBody](cmd.Context(), c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
				return cl.DatabasesListOptions(ctx, api.V1DatabasesListOptionsRequestBody{})
			})
			if err != nil {
				return err
			}
			o := res.Data
			if a.IO.JSON {
				return a.IO.PrintJSON(o)
			}
			th := a.IO.Theme
			var rows [][]string
			for _, s := range o.Sizes {
				allowed := th.Success.Render("✓")
				if !s.Allowed {
					allowed = th.Muted.Render(string(s.MinPlan) + " plan")
				}
				rows = append(rows, []string{s.Slug, s.DisplayName, vcpu(s.CpuMillicores), mib(int64(s.MemoryMib)), mib(int64(s.StorageMib)), strconv.Itoa(int(s.MaxConnections)), allowed})
			}
			a.IO.Table([]string{"Size", "Name", "CPU", "Memory", "Storage", "Connections", "Available"}, rows)
			a.IO.Println()
			var engines []string
			for _, e := range o.Engines {
				if !e.Available {
					engines = append(engines, string(e.Engine)+" (coming soon)")
					continue
				}
				engines = append(engines, fmt.Sprintf("%s %s (default %s)", e.Engine, strings.Join(e.Versions, ", "), e.DefaultVersion))
			}
			a.IO.KV([][2]string{
				{"Plan", o.Plan},
				{"Databases", fmt.Sprintf("%d of %d", o.DatabaseCount, o.MaxDatabases)},
				{"Instances", fmt.Sprintf("up to %d per database", o.MaxInstances)},
				{"Engines", strings.Join(engines, "; ")},
			})
			return nil
		},
	}
}

func newDBCredentialsCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:     "credentials <db>",
		Aliases: []string{"creds", "url"},
		Short:   "Print a database's connection URL",
		Long: `Print the connection URL, including the password. With --output json, every
field (host, port, user, password, ...) is printed. Reading credentials is
recorded in the workspace's audit log.`,
		Example: "  teral db credentials main\n  teral db credentials main -o json",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, _, err := a.Client()
			if err != nil {
				return err
			}
			creds, err := a.dbCredentials(ctx, c, args[0])
			if err != nil {
				return err
			}
			a.IO.Warn("Reading credentials is recorded in the audit log. Treat the URL as a secret.")
			if a.IO.JSON {
				return a.IO.PrintJSON(creds)
			}
			a.IO.Println(creds.Url)
			return nil
		},
	}
}

func newDBRotateCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:   "rotate <db>",
		Short: "Rotate a database's password",
		Long: `Set a new password. Linked apps get the new connection URL in their variable
and use it from their next deployment; other clients must update theirs.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, _, err := a.Client()
			if err != nil {
				return err
			}
			project, err := a.dbProject(ctx, c, args[0])
			if err != nil {
				return err
			}
			if err := a.IO.Confirm(fmt.Sprintf("Rotate the password of %s? Clients using the old one stop connecting, and linked apps need a redeploy.", args[0]), a.Yes); err != nil {
				return err
			}
			res, err := client.Call[api.V1DatabasesRotatePasswordResponseBody](ctx, c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
				return cl.DatabasesRotatePassword(ctx, api.V1DatabasesRotatePasswordRequestBody{Project: project, Database: args[0]})
			})
			if err != nil {
				return err
			}
			if a.IO.JSON {
				return a.IO.PrintJSON(res.Data)
			}
			a.IO.Success("Rotating the password of %s", res.Data.Name)
			a.IO.Hint("Redeploy linked apps to use it. Print the new URL with `teral db credentials %s`.", res.Data.Name)
			return nil
		},
	}
}

func newDBConnectCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:   "connect <db> [-- psql-args...]",
		Short: "Open a psql shell on a database",
		Long: `Fetch the database's credentials and start psql with its connection URL.
psql must be installed; arguments after -- are passed to it. The database must
be reachable from this machine; a tunnel for databases without public access
is coming later. Reading the credentials is recorded in the audit log.`,
		Example: "  teral db connect main\n  teral db connect main -- -c 'select now()'",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, _, err := a.Client()
			if err != nil {
				return err
			}
			creds, err := a.dbCredentials(ctx, c, args[0])
			if err != nil {
				return err
			}
			if creds.Engine != api.DatabaseEnginePostgres {
				return &UserError{Message: fmt.Sprintf("teral db connect supports Postgres; %s is %s.", args[0], creds.Engine), Hint: "Connect with the URL from `teral db credentials`."}
			}
			psql, err := exec.LookPath("psql")
			if err != nil {
				return &UserError{
					Message: "psql isn't installed or not on your PATH.",
					Hint:    "Install the Postgres client: `brew install libpq` (macOS), `apt install postgresql-client` (Debian, Ubuntu), or see https://www.postgresql.org/download/.",
				}
			}
			a.IO.Info("Connecting to %s as %s %s", a.IO.ErrTheme.Bold.Render(creds.Database), creds.Username, a.IO.ErrTheme.Muted.Render("(recorded in the audit log)"))
			child := exec.Command(psql, append([]string{creds.Url}, args[1:]...)...)
			child.Stdin, child.Stdout, child.Stderr = a.IO.In, a.IO.Out, a.IO.Err
			// psql handles Ctrl+C itself (cancels the query); don't die first.
			signal.Ignore(os.Interrupt)
			defer signal.Reset(os.Interrupt)
			if err := child.Run(); err != nil {
				var exit *exec.ExitError
				if errors.As(err, &exit) {
					return &exitError{code: exit.ExitCode()}
				}
				return &UserError{Message: fmt.Sprintf("Couldn't run psql: %v", err)}
			}
			return nil
		},
	}
}

func newDBLinkCmd(a *App) *cobra.Command {
	var envKey string
	cmd := &cobra.Command{
		Use:   "link <db>",
		Short: "Put a database's connection URL into an app's variables",
		Long: `Link a database to an app environment: Teral stores the connection URL in a
variable (DATABASE_URL by default) and rewrites it when the password rotates.
The app picks it up on its next deployment. The app is --app or the linked
app; the environment is --env, else production.`,
		Example: "  teral db link main --app api\n  teral db link main --app api --env preview --env-key PG_URL",
		Args:    cobra.ExactArgs(1),
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
			res, err := client.Call[api.V1DatabasesLinkDatabaseResponseBody](ctx, c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
				return cl.DatabasesLinkDatabase(ctx, api.V1DatabasesLinkDatabaseRequestBody{
					Project: &t.Project, Database: args[0], App: t.App, Environment: t.Environment, EnvKey: nonEmpty(envKey),
				})
			})
			if err != nil {
				return err
			}
			if a.IO.JSON {
				return a.IO.PrintJSON(res.Data)
			}
			a.IO.Success("Linked %s to %s as %s %s", args[0], t, res.Data.EnvKey, a.IO.ErrTheme.Muted.Render("("+res.Data.Id+")"))
			a.IO.Hint("Takes effect on the next deployment.")
			return nil
		},
	}
	cmd.Flags().StringVar(&envKey, "env-key", "", "Variable that holds the URL (default DATABASE_URL)")
	return cmd
}

func newDBUnlinkCmd(a *App) *cobra.Command {
	var keep bool
	cmd := &cobra.Command{
		Use:   "unlink <db> <link-id|app>",
		Short: "Remove a database link and its variable",
		Long: `Remove a link by its id (dbl_…, see "teral db get") or by app. When the app is
linked in several environments, pick one with --env. The variable is removed
too unless --keep-variable is passed; a kept variable holds the last URL and
is no longer updated.`,
		Example: "  teral db unlink main api\n  teral db unlink main dbl_1234abcd --keep-variable",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, _, err := a.Client()
			if err != nil {
				return err
			}
			project, err := a.dbProject(ctx, c, args[0])
			if err != nil {
				return err
			}
			db, err := getDatabase(ctx, c, project, args[0])
			if err != nil {
				return err
			}
			l, err := a.findLink(ctx, c, db, args[1])
			if err != nil {
				return err
			}
			names := a.linkNames(ctx, c, db.ProjectId, []api.DatabaseLink{l})[l.Id]
			where := fmt.Sprintf("%s (%s)", names[0], names[1])
			question := fmt.Sprintf("Unlink %s from %s and remove its %s variable?", db.Name, where, l.EnvKey)
			if keep {
				question = fmt.Sprintf("Unlink %s from %s? %s keeps its last value.", db.Name, where, l.EnvKey)
			}
			if err := a.IO.Confirm(question, a.Yes); err != nil {
				return err
			}
			if _, err := client.Call[api.V1DatabasesUnlinkDatabaseResponseBody](ctx, c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
				return cl.DatabasesUnlinkDatabase(ctx, api.V1DatabasesUnlinkDatabaseRequestBody{Database: db.Id, LinkId: l.Id, RemoveVariable: ptr(!keep)})
			}); err != nil {
				return err
			}
			a.IO.Success("Unlinked %s from %s", db.Name, where)
			if !keep {
				a.IO.Hint("Removed %s; takes effect on the next deployment.", l.EnvKey)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&keep, "keep-variable", false, "Keep the variable with its last value")
	return cmd
}

func newDBBackupsCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:   "backups <db>",
		Short: "List a database's backups",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, _, err := a.Client()
			if err != nil {
				return err
			}
			project, err := a.dbProject(ctx, c, args[0])
			if err != nil {
				return err
			}
			res, err := client.Call[api.V1DatabasesListBackupsResponseBody](ctx, c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
				return cl.DatabasesListBackups(ctx, api.V1DatabasesListBackupsRequestBody{Project: project, Database: args[0]})
			})
			if err != nil {
				return err
			}
			if a.IO.JSON {
				return a.IO.PrintJSON(res.Data)
			}
			if len(res.Data) == 0 {
				a.IO.Info("%s has no backups yet. Take one with %s.", args[0], a.IO.Code("teral db backup "+args[0]))
				return nil
			}
			a.backupTable(res.Data)
			return nil
		},
	}
}

func newDBBackupCmd(a *App) *cobra.Command {
	var wait bool
	cmd := &cobra.Command{
		Use:   "backup <db>",
		Short: "Take a backup now",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, _, err := a.Client()
			if err != nil {
				return err
			}
			project, err := a.dbProject(ctx, c, args[0])
			if err != nil {
				return err
			}
			res, err := client.Call[api.V1DatabasesCreateBackupResponseBody](ctx, c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
				return cl.DatabasesCreateBackup(ctx, api.V1DatabasesCreateBackupRequestBody{Project: project, Database: args[0]})
			})
			if err != nil {
				return err
			}
			b := res.Data
			if wait {
				final, err := a.waitBackup(ctx, c, args[0], project, b.Id)
				if err != nil {
					return err
				}
				b = *final
			}
			if a.IO.JSON {
				return a.IO.PrintJSON(b)
			}
			if !wait {
				a.IO.Success("Started backup %s of %s", b.Id, args[0])
				a.IO.Hint("Check on it with `teral db backups %s`.", args[0])
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&wait, "wait", false, "Wait until the backup completes")
	return cmd
}

func newDBBackupScheduleCmd(a *App) *cobra.Command {
	var hour int
	cmd := &cobra.Command{
		Use:     "backup-schedule <db> --hour N",
		Short:   "Set the UTC hour of the daily backup",
		Example: "  teral db backup-schedule main --hour 3",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !cmd.Flags().Changed("hour") || hour < 0 || hour > 23 {
				return &UserError{Message: "Pass the UTC hour of the daily backup.", Hint: "Use --hour 0 to --hour 23."}
			}
			ctx := cmd.Context()
			c, _, err := a.Client()
			if err != nil {
				return err
			}
			project, err := a.dbProject(ctx, c, args[0])
			if err != nil {
				return err
			}
			res, err := client.Call[api.V1DatabasesUpdateBackupScheduleResponseBody](ctx, c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
				return cl.DatabasesUpdateBackupSchedule(ctx, api.V1DatabasesUpdateBackupScheduleRequestBody{Project: project, Database: args[0], Hour: int32(hour)})
			})
			if err != nil {
				return err
			}
			if a.IO.JSON {
				return a.IO.PrintJSON(res.Data)
			}
			a.IO.Success("%s is backed up daily at %02d:00 UTC", res.Data.Name, res.Data.BackupHour)
			return nil
		},
	}
	cmd.Flags().IntVar(&hour, "hour", 0, "UTC hour, 0 to 23")
	return cmd
}

func newDBRestoreCmd(a *App) *cobra.Command {
	var name, backup, to, size string
	var instances int32
	var wait bool
	cmd := &cobra.Command{
		Use:   "restore <db> --name <new>",
		Short: "Restore a database into a new database",
		Long: `Create a new database from a backup or a point in time of an existing one. The
source is left untouched. Without --backup or --to, the latest state is
restored. --to needs point-in-time recovery on your plan.`,
		Example: `  teral db restore main --name main-restored
  teral db restore main --name main-before --to 2026-11-26T09:30:00Z
  teral db restore main --name main-1h --to 1h
  teral db restore main --name main-b --backup dbb_1234abcd`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return &UserError{Message: "Name the new database.", Hint: "Pass --name."}
			}
			if backup != "" && to != "" {
				return &UserError{Message: "Pass either --backup or --to, not both."}
			}
			body := api.V1DatabasesRestoreDatabaseRequestBody{Database: args[0], Name: name, BackupId: nonEmpty(backup), Size: nonEmpty(size)}
			if to != "" {
				ms, err := parseTime(to, time.Now())
				if err != nil {
					return err
				}
				body.TargetTime = &ms
			}
			if instances > 0 {
				body.Instances = &instances
			}
			ctx := cmd.Context()
			c, _, err := a.Client()
			if err != nil {
				return err
			}
			if body.Project, err = a.dbProject(ctx, c, args[0]); err != nil {
				return err
			}
			res, err := client.Call[api.V1DatabasesRestoreDatabaseResponseBody](ctx, c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
				return cl.DatabasesRestoreDatabase(ctx, body)
			})
			if err != nil {
				return err
			}
			db := res.Data
			if wait {
				final, err := a.waitDatabase(ctx, c, db.Id, "Restored")
				if err != nil {
					return err
				}
				db = *final
			}
			if a.IO.JSON {
				return a.IO.PrintJSON(db)
			}
			if !wait {
				a.IO.Success("Restoring %s into %s %s", args[0], db.Name, a.IO.ErrTheme.Muted.Render("("+db.Id+")"))
				a.IO.Hint("Check on it with `teral db get %s`.", db.Name)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&name, "name", "", "Name of the new database (required)")
	f.StringVar(&backup, "backup", "", "Restore to the end of this backup (dbb_…, from teral db backups)")
	f.StringVar(&to, "to", "", "Restore to this time: RFC 3339, or a duration ago like 2h")
	f.StringVar(&size, "size", "", "Size of the new database (default: the source's)")
	f.Int32Var(&instances, "instances", 0, "Instances of the new database (default: the source's)")
	f.BoolVar(&wait, "wait", false, "Wait until the new database is ready")
	return cmd
}

// ---- helpers ----

// dbProject returns the project to address a database with. An id (db_…)
// needs none; a name needs the project from flags, the environment, the
// directory's link, or a prompt.
func (a *App) dbProject(ctx context.Context, c *client.Client, ref string) (*string, error) {
	if strings.HasPrefix(ref, "db_") {
		return nonEmpty(a.Project), nil
	}
	t, err := a.ResolveTarget(ctx, c, NeedProject)
	if err != nil {
		return nil, err
	}
	return &t.Project, nil
}

func getDatabase(ctx context.Context, c *client.Client, project *string, ref string) (*api.Database, error) {
	res, err := client.Call[api.V1DatabasesGetDatabaseResponseBody](ctx, c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
		return cl.DatabasesGetDatabase(ctx, api.V1DatabasesGetDatabaseRequestBody{Project: project, Database: ref})
	})
	if err != nil {
		return nil, err
	}
	return &res.Data, nil
}

func listBackups(ctx context.Context, c *client.Client, id string) ([]api.DatabaseBackup, error) {
	res, err := client.Call[api.V1DatabasesListBackupsResponseBody](ctx, c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
		return cl.DatabasesListBackups(ctx, api.V1DatabasesListBackupsRequestBody{Database: id})
	})
	if err != nil {
		return nil, err
	}
	return res.Data, nil
}

func (a *App) dbCredentials(ctx context.Context, c *client.Client, ref string) (*api.V1DatabasesGetCredentialsResponseData, error) {
	project, err := a.dbProject(ctx, c, ref)
	if err != nil {
		return nil, err
	}
	res, err := client.Call[api.V1DatabasesGetCredentialsResponseBody](ctx, c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
		return cl.DatabasesGetCredentials(ctx, api.V1DatabasesGetCredentialsRequestBody{Project: project, Database: ref})
	})
	if err != nil {
		return nil, err
	}
	return &res.Data, nil
}

// waitDatabase polls a database until it is ready or failed. Interrupting
// stops watching, never the operation.
func (a *App) waitDatabase(ctx context.Context, c *client.Client, id, done string) (*api.Database, error) {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	started := time.Now()
	spin := a.IO.Spin("Provisioning")
	defer spin.Stop()
	stopped := &UserError{Message: "Stopped watching. The database operation continues.", Hint: fmt.Sprintf("Check on it with `teral db get %s`.", id)}
	for {
		db, err := getDatabase(ctx, c, nil, id)
		if err != nil {
			if ctx.Err() != nil {
				return nil, stopped
			}
			return nil, err
		}
		switch db.Status {
		case api.DatabaseStatusReady:
			spin.Stop()
			a.IO.Success("%s %s in %s", done, db.Name, ui.Duration(time.Since(started)))
			if db.Host != nil {
				a.IO.Hint("%s:%d, database %s, user %s", *db.Host, db.Port, db.Database, db.Username)
			}
			return db, nil
		case api.DatabaseStatusFailed:
			spin.Stop()
			msg := fmt.Sprintf("Database %s failed.", db.Name)
			if db.StatusMessage != nil && *db.StatusMessage != "" {
				msg = fmt.Sprintf("Database %s failed: %s", db.Name, *db.StatusMessage)
			}
			return db, &UserError{Message: msg, Hint: "Contact support if it keeps failing."}
		case api.DatabaseStatusDeleting:
			spin.Stop()
			return db, &UserError{Message: fmt.Sprintf("Database %s is being deleted.", db.Name)}
		}
		label := "Provisioning"
		if db.Status == api.DatabaseStatusUpdating {
			label = "Applying changes"
		}
		if db.StatusMessage != nil && *db.StatusMessage != "" {
			label += ": " + *db.StatusMessage
		}
		spin.Update(fmt.Sprintf("%s %s", label, a.IO.ErrTheme.Muted.Render(ui.Duration(time.Since(started)))))
		select {
		case <-ctx.Done():
			spin.Stop()
			return nil, stopped
		case <-time.After(dbPollInterval):
		}
	}
}

// waitBackup polls a database's backups until the given one settles.
func (a *App) waitBackup(ctx context.Context, c *client.Client, ref string, project *string, id string) (*api.DatabaseBackup, error) {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	started := time.Now()
	spin := a.IO.Spin("Backing up")
	defer spin.Stop()
	stopped := &UserError{Message: "Stopped watching. The backup continues.", Hint: fmt.Sprintf("Check on it with `teral db backups %s`.", ref)}
	for {
		res, err := client.Call[api.V1DatabasesListBackupsResponseBody](ctx, c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
			return cl.DatabasesListBackups(ctx, api.V1DatabasesListBackupsRequestBody{Project: project, Database: ref})
		})
		if err != nil {
			if ctx.Err() != nil {
				return nil, stopped
			}
			return nil, err
		}
		for i := range res.Data {
			b := &res.Data[i]
			if b.Id != id {
				continue
			}
			switch b.Status {
			case api.DatabaseBackupStatusCompleted:
				spin.Stop()
				a.IO.Success("Backed up %s in %s %s", ref, ui.Duration(time.Since(started)), a.IO.ErrTheme.Muted.Render("("+b.Id+")"))
				return b, nil
			case api.DatabaseBackupStatusFailed:
				spin.Stop()
				msg := "The backup failed."
				if b.Error != nil && *b.Error != "" {
					msg = "The backup failed: " + *b.Error
				}
				return b, &UserError{Message: msg}
			}
			spin.Update(fmt.Sprintf("Backup %s %s", b.Status, a.IO.ErrTheme.Muted.Render(ui.Duration(time.Since(started)))))
		}
		select {
		case <-ctx.Done():
			spin.Stop()
			return nil, stopped
		case <-time.After(dbPollInterval):
		}
	}
}

// findLink picks a database link by its id, or by app (and --env when the
// app is linked in several environments).
func (a *App) findLink(ctx context.Context, c *client.Client, db *api.Database, ref string) (api.DatabaseLink, error) {
	for _, l := range db.Links {
		if l.Id == ref {
			return l, nil
		}
	}
	if strings.HasPrefix(ref, "dbl_") {
		return api.DatabaseLink{}, &UserError{Message: fmt.Sprintf("%s has no link %s.", db.Name, ref), Hint: fmt.Sprintf("See its links with `teral db get %s`.", db.Name)}
	}
	apps, err := listApps(ctx, c, db.ProjectId)
	if err != nil {
		return api.DatabaseLink{}, err
	}
	var app *api.App
	for i := range apps {
		if apps[i].Slug == ref || apps[i].Id == ref {
			app = &apps[i]
		}
	}
	if app == nil {
		return api.DatabaseLink{}, &UserError{Message: fmt.Sprintf("%q is neither a link of %s nor an app in its project.", ref, db.Name), Hint: fmt.Sprintf("See its links with `teral db get %s`.", db.Name)}
	}
	envRef := firstNonEmpty(a.Environment, os.Getenv("TERAL_ENVIRONMENT"))
	envID := ""
	if envRef != "" {
		envs, err := listEnvironments(ctx, c, db.ProjectId, app.Slug)
		if err != nil {
			return api.DatabaseLink{}, err
		}
		for _, e := range envs {
			if e.Slug == envRef || e.Id == envRef {
				envID = e.Id
			}
		}
		if envID == "" {
			return api.DatabaseLink{}, &UserError{Message: fmt.Sprintf("App %s has no environment %s.", app.Slug, envRef)}
		}
	}
	var matches []api.DatabaseLink
	for _, l := range db.Links {
		if l.AppId == app.Id && (envID == "" || l.EnvironmentId == envID) {
			matches = append(matches, l)
		}
	}
	switch len(matches) {
	case 0:
		return api.DatabaseLink{}, &UserError{Message: fmt.Sprintf("%s isn't linked to %s.", db.Name, app.Slug)}
	case 1:
		return matches[0], nil
	default:
		ids := make([]string, len(matches))
		for i, l := range matches {
			ids[i] = l.Id
		}
		return api.DatabaseLink{}, &UserError{
			Message: fmt.Sprintf("%s is linked to %s in %d environments.", db.Name, app.Slug, len(matches)),
			Hint:    "Pick one with --env, or pass a link id: " + strings.Join(ids, ", "),
		}
	}
}

// linkNames maps link ids to app and environment slugs, falling back to ids
// when they can't be looked up.
func (a *App) linkNames(ctx context.Context, c *client.Client, project string, links []api.DatabaseLink) map[string][2]string {
	out := map[string][2]string{}
	for _, l := range links {
		out[l.Id] = [2]string{l.AppId, l.EnvironmentId}
	}
	apps, err := listApps(ctx, c, project)
	if err != nil {
		return out
	}
	for _, app := range apps {
		var envs []api.Environment
		for _, l := range links {
			if l.AppId != app.Id {
				continue
			}
			if envs == nil {
				envs, _ = listEnvironments(ctx, c, project, app.Slug)
			}
			env := l.EnvironmentId
			for _, e := range envs {
				if e.Id == l.EnvironmentId {
					env = e.Slug
				}
			}
			out[l.Id] = [2]string{app.Slug, env}
		}
	}
	return out
}

func (a *App) printDatabase(d api.Database) {
	th := a.IO.Theme
	status := th.Status(string(d.Status))
	if d.StatusMessage != nil && *d.StatusMessage != "" {
		status += th.Muted.Render("  " + *d.StatusMessage)
	}
	pairs := [][2]string{
		{"Name", d.Name},
		{"ID", d.Id},
		{"Status", status},
		{"Engine", dbEngine(d)},
		{"Size", fmt.Sprintf("%s, %d %s", d.Size, d.Instances, plural(int(d.Instances), "instance", "instances"))},
	}
	if d.Host != nil {
		pairs = append(pairs, [2]string{"Host", fmt.Sprintf("%s:%d", *d.Host, d.Port)})
	}
	pairs = append(pairs, [2]string{"Database", d.Database}, [2]string{"User", d.Username})
	if d.Region != nil {
		pairs = append(pairs, [2]string{"Region", *d.Region})
	}
	if d.StorageUsedMib != nil {
		pairs = append(pairs, [2]string{"Storage used", mib(*d.StorageUsedMib)})
	}
	pairs = append(pairs, [2]string{"Created", ui.Ago(d.CreatedAt)})
	backups := fmt.Sprintf("daily at %02d:00 UTC, kept %d days", d.BackupHour, d.BackupRetentionDays)
	if d.PointInTimeRecovery {
		backups += ", point-in-time recovery"
	}
	pairs = append(pairs, [2]string{"Backups", backups})
	if d.LatestBackupAt != nil {
		pairs = append(pairs, [2]string{"Last backup", ui.Ago(*d.LatestBackupAt)})
	}
	if d.EarliestRestoreTime != nil {
		pairs = append(pairs, [2]string{"Restorable from", time.UnixMilli(*d.EarliestRestoreTime).Format(time.RFC3339)})
	}
	a.IO.KV(pairs)
}

func (a *App) backupTable(backups []api.DatabaseBackup) {
	th := a.IO.Theme
	var rows [][]string
	for _, b := range backups {
		status := th.Accent.Render("◐ " + string(b.Status))
		switch b.Status {
		case api.DatabaseBackupStatusCompleted:
			status = th.Success.Render("● completed")
		case api.DatabaseBackupStatusFailed:
			status = th.Danger.Render("● failed")
			if b.Error != nil && *b.Error != "" {
				status += th.Muted.Render(" " + *b.Error)
			}
		}
		took := "—"
		if b.StartedAt != nil && b.StoppedAt != nil {
			took = ui.Duration(time.UnixMilli(*b.StoppedAt).Sub(time.UnixMilli(*b.StartedAt)))
		}
		rows = append(rows, []string{b.Id, string(b.Kind), status, ui.Ago(b.CreatedAt), took})
	}
	a.IO.Table([]string{"Backup", "Kind", "Status", "Created", "Took"}, rows)
}

func dbEngine(d api.Database) string {
	return strings.TrimSpace(string(d.Engine) + " " + d.Version)
}

// mib renders a size in MiB, e.g. "512 MiB" or "1.5 GiB".
func mib(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d MiB", n)
	}
	gib := float64(n) / 1024
	if gib == float64(int64(gib)) {
		return fmt.Sprintf("%d GiB", int64(gib))
	}
	return fmt.Sprintf("%.1f GiB", gib)
}

// vcpu renders millicores, e.g. "0.25 vCPU".
func vcpu(millicores int32) string {
	return strconv.FormatFloat(float64(millicores)/1000, 'f', -1, 64) + " vCPU"
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
