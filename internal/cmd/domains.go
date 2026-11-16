package cmd

import (
	"context"
	"fmt"
	"net/http"

	"github.com/spf13/cobra"

	"github.com/teraldev/cli/internal/api"
	"github.com/teraldev/cli/internal/client"
)

func newDomainsCmd(a *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "domains",
		Aliases: []string{"domain"},
		Short:   "Add and verify custom domains",
	}
	cmd.AddCommand(&cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List an app's custom domains",
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
			res, err := client.Call[api.V1DomainsListDomainsResponseBody](ctx, c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
				return cl.DomainsListDomains(ctx, api.V1DomainsListDomainsRequestBody{Project: &t.Project, App: &t.App, Environment: nonEmpty(t.Environment), Limit: ptr(100)})
			})
			if err != nil {
				return err
			}
			if a.IO.JSON {
				return a.IO.PrintJSON(res.Data)
			}
			if len(res.Data) == 0 {
				a.IO.Info("No custom domains. Add one with %s.", a.IO.Code("teral domains add example.com"))
				return nil
			}
			var rows [][]string
			for _, d := range res.Data {
				rows = append(rows, []string{d.Domain, domainStatus(a, d)})
			}
			a.IO.Table([]string{"Domain", "Status"}, rows)
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "add <domain>",
		Short: "Attach a custom domain and show the DNS records to create",
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
			res, err := client.Call[api.V1DomainsCreateDomainResponseBody](ctx, c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
				return cl.DomainsCreateDomain(ctx, api.V1DomainsCreateDomainRequestBody{Project: t.Project, App: t.App, Environment: t.Environment, Domain: args[0]})
			})
			if err != nil {
				return err
			}
			if a.IO.JSON {
				return a.IO.PrintJSON(res.Data)
			}
			a.IO.Success("Added %s to %s", args[0], t)
			a.printDNS(args[0], res.Data.DnsRecords)
			if res.Data.DomainConnect != nil {
				a.IO.Hint("Your DNS provider supports automatic setup; finish it in the dashboard under the app's domains.")
			}
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "verify <domain>",
		Short: "Check a domain's DNS records now",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := a.Client()
			if err != nil {
				return err
			}
			if _, err := client.Call[api.V1DomainsVerifyDomainResponseBody](cmd.Context(), c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
				return cl.DomainsVerifyDomain(ctx, api.V1DomainsVerifyDomainRequestBody{Domain: args[0]})
			}); err != nil {
				return err
			}
			res, err := client.Call[api.V1DomainsGetDomainResponseBody](cmd.Context(), c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
				return cl.DomainsGetDomain(ctx, api.V1DomainsGetDomainRequestBody{Domain: args[0]})
			})
			if err != nil {
				return err
			}
			if a.IO.JSON {
				return a.IO.PrintJSON(res.Data)
			}
			a.IO.KV([][2]string{{"Domain", res.Data.Domain}, {"Status", domainStatus(a, res.Data)}})
			if res.Data.Status != api.DomainStatusVerified {
				a.printDNS(res.Data.Domain, res.Data.DnsRecords)
			}
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:     "remove <domain>",
		Aliases: []string{"rm", "delete"},
		Short:   "Detach a custom domain",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := a.Client()
			if err != nil {
				return err
			}
			if err := a.IO.Confirm(fmt.Sprintf("Remove %s? It stops serving your app.", args[0]), a.Yes); err != nil {
				return err
			}
			if _, err := client.Call[map[string]any](cmd.Context(), c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
				return cl.DomainsDeleteDomain(ctx, api.V1DomainsDeleteDomainRequestBody{Domain: args[0]})
			}); err != nil {
				return err
			}
			a.IO.Success("Removed %s", args[0])
			return nil
		},
	})
	return cmd
}

func domainStatus(a *App, d api.Domain) string {
	th := a.IO.Theme
	switch d.Status {
	case api.DomainStatusVerified:
		return th.Success.Render("● verified")
	case api.DomainStatusFailed:
		msg := "● failed"
		if d.VerificationError != nil {
			msg += ": " + *d.VerificationError
		}
		return th.Danger.Render(msg)
	default:
		return th.Warning.Render("◐ " + string(d.Status))
	}
}

func (a *App) printDNS(domain string, records []api.DnsRecord) {
	if len(records) == 0 {
		return
	}
	a.IO.Println()
	a.IO.Println("Create these records at your DNS provider:")
	var rows [][]string
	for _, r := range records {
		state := a.IO.Theme.Muted.Render("pending")
		if r.Verified {
			state = a.IO.Theme.Success.Render("ok")
		}
		rows = append(rows, []string{string(r.Type), r.Name, r.Value, state})
	}
	a.IO.Table([]string{"Type", "Name", "Value", ""}, rows)
	a.IO.Hint("Check again with `teral domains verify %s`. DNS changes can take a few minutes.", domain)
}
