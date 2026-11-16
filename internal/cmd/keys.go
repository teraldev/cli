package cmd

import (
	"context"
	"fmt"
	"net/http"

	"github.com/spf13/cobra"

	"github.com/teraldev/cli/internal/api"
	"github.com/teraldev/cli/internal/client"
	"github.com/teraldev/cli/internal/ui"
)

// keyPresets mirror the dashboard roles: "deploy" can do what a developer
// can, "read" what a viewer can.
var keyPresets = map[string][]string{
	"deploy": {
		"projects/*#read", "projects/*#write", "projects/*#delete",
		"projects/*/apps/*#read", "projects/*/apps/*#write", "projects/*/apps/*#delete",
		"projects/*/apps/*/environments/*#read", "projects/*/apps/*/environments/*#write", "projects/*/apps/*/environments/*#delete",
		"projects/*/apps/*/environments/*/variables/*#read", "projects/*/apps/*/environments/*/variables/*#write", "projects/*/apps/*/environments/*/variables/*#delete",
	},
	"read": {
		"projects/*#read", "projects/*/apps/*#read", "projects/*/apps/*/environments/*#read", "projects/*/apps/*/environments/*/variables/*#read",
	},
}

func newKeysCmd(a *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "keys",
		Aliases: []string{"key", "api-keys"},
		Short:   "Manage API keys for CI and automation",
		Long: `API keys authenticate the CLI and the API without a browser: set TERAL_API_KEY
in CI and every command uses it. Keys can only grant what you're allowed to do.`,
	}
	cmd.AddCommand(&cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List API keys",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := a.Client()
			if err != nil {
				return err
			}
			res, err := client.Call[api.V1ApiKeysListKeysResponseBody](cmd.Context(), c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
				return cl.ApiKeysListKeys(ctx, api.V1ApiKeysListKeysRequestBody{Limit: ptr(100)})
			})
			if err != nil {
				return err
			}
			if a.IO.JSON {
				return a.IO.PrintJSON(res.Data)
			}
			if len(res.Data) == 0 {
				a.IO.Info("No API keys. Create one for CI with %s.", a.IO.Code("teral keys create ci --preset deploy"))
				return nil
			}
			var rows [][]string
			for _, k := range res.Data {
				name, _ := k.Name.Get()
				expires := "never"
				if v, err := k.Expires.Get(); err == nil {
					expires = ui.Ago(v)
				}
				rows = append(rows, []string{k.KeyId, name, k.Start + "…" + k.End, ui.Ago(k.LastUsedAt), expires})
			}
			a.IO.Table([]string{"ID", "Name", "Key", "Last used", "Expires"}, rows)
			return nil
		},
	})

	var preset string
	var permissions []string
	create := &cobra.Command{
		Use:   "create <name>",
		Short: "Create an API key",
		Long: `Create an API key and print it once. Choose its permissions with a preset:
  deploy  deploy, manage apps, environments and variables (like a developer)
  read    read-only access (like a viewer)
or pass raw permissions with --permission (repeatable).`,
		Example: "  teral keys create github-actions --preset deploy",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, cred, err := a.Client()
			if err != nil {
				return err
			}
			perms := permissions
			if preset != "" {
				grants, ok := keyPresets[preset]
				if !ok {
					return &UserError{Message: fmt.Sprintf("Unknown preset %q.", preset), Hint: "Use deploy or read."}
				}
				ws, err := a.workspaceOf(ctx, c, cred)
				if err != nil {
					return err
				}
				for _, g := range grants {
					perms = append(perms, "teral:v1:"+ws.Id+":"+g)
				}
			}
			if len(perms) == 0 {
				return &UserError{Message: "Choose permissions for the key.", Hint: "Pass --preset deploy, --preset read, or --permission."}
			}
			name := args[0]
			res, err := client.Call[api.V1ApiKeysCreateKeyResponseBody](ctx, c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
				return cl.ApiKeysCreateKey(ctx, api.V1ApiKeysCreateKeyRequestBody{Name: &name, Permissions: perms})
			})
			if err != nil {
				return err
			}
			if a.IO.JSON {
				return a.IO.PrintJSON(res.Data)
			}
			a.IO.Success("Created API key %s %s", name, a.IO.ErrTheme.Muted.Render("("+res.Data.KeyId+")"))
			a.IO.Hint("Copy it now; it won't be shown again. Use it as TERAL_API_KEY:")
			a.IO.Println(res.Data.Key)
			return nil
		},
	}
	create.Flags().StringVar(&preset, "preset", "", "Permission preset: deploy or read")
	create.Flags().StringArrayVar(&permissions, "permission", nil, "Raw permission (repeatable)")
	cmd.AddCommand(create)

	cmd.AddCommand(&cobra.Command{
		Use:     "delete <key-id>",
		Aliases: []string{"rm", "revoke"},
		Short:   "Delete an API key",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := a.Client()
			if err != nil {
				return err
			}
			if err := a.IO.Confirm(fmt.Sprintf("Delete API key %s? Anything using it stops working.", args[0]), a.Yes); err != nil {
				return err
			}
			if _, err := client.Call[map[string]any](cmd.Context(), c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
				return cl.ApiKeysDeleteKey(ctx, api.V1ApiKeysDeleteKeyRequestBody{KeyId: args[0]})
			}); err != nil {
				return err
			}
			a.IO.Success("Deleted %s", args[0])
			return nil
		},
	})
	return cmd
}
