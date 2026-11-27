package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/teraldev/cli/internal/api"
	"github.com/teraldev/cli/internal/client"
	"github.com/teraldev/cli/internal/tunnel"
)

// tunnelOptions lets tests point tunnels at a fake database proxy.
var tunnelOptions tunnel.Options

// defaultTunnelPort is tried first for `teral db tunnel` without --port.
const defaultTunnelPort = 5432

func newDBTunnelCmd(a *App) *cobra.Command {
	var port int
	var bind string
	cmd := &cobra.Command{
		Use:   "tunnel <db>",
		Short: "Open a local tunnel to a database",
		Long: `Listen on a local port and forward every connection to the database through
Teral's database proxy, authenticated with your Teral sign-in. Clients connect
to the local address with sslmode=disable (the tunnel itself is encrypted)
and log in with the database's user and password; the password comes from
"teral db credentials". The port is 5432 when it is free, else a random one;
--port picks one (0 for a random port). Press Ctrl+C to close the tunnel.

Opening a tunnel needs the permission to read the database's credentials and
is recorded in the audit log.`,
		Example: "  teral db tunnel main\n  teral db tunnel main --port 6543\n  teral db tunnel db_1234abcd --port 0",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()
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
			if db.Engine != api.DatabaseEnginePostgres {
				return &UserError{Message: fmt.Sprintf("Tunnels support Postgres; %s is %s.", db.Name, db.Engine)}
			}
			tun, err := a.newTunnel(ctx, c, nil, db.Id)
			if err != nil {
				return err
			}
			ln, err := listenTunnel(bind, port, cmd.Flags().Changed("port"))
			if err != nil {
				return err
			}
			addr := ln.Addr().String()
			connURL := tunnelURL(db.Username, "", addr, db.Database)
			if a.IO.JSON {
				if err := a.IO.PrintJSON(map[string]any{
					"database": db.Name, "address": addr, "url": connURL, "username": db.Username,
				}); err != nil {
					_ = ln.Close()
					return err
				}
			} else {
				a.IO.Success("Tunnel to %s open on %s %s", db.Name, addr, a.IO.ErrTheme.Muted.Render("(recorded in the audit log)"))
				a.IO.Println(connURL)
				a.IO.Hint("The password comes from `teral db credentials %s`. Press Ctrl+C to close the tunnel.", db.Name)
			}
			if err := tun.Serve(ctx, ln); err != nil {
				return fmt.Errorf("tunnel: %w", err)
			}
			a.IO.Info("Tunnel closed")
			return nil
		},
	}
	cmd.Flags().IntVar(&port, "port", 0, "Local port (default 5432 if free, else random; 0 for random)")
	cmd.Flags().StringVar(&bind, "bind", "127.0.0.1", "Local address to listen on")
	return cmd
}

// newTunnel fetches the first tunnel token for a database and returns a
// tunnel that renews it as needed.
func (a *App) newTunnel(ctx context.Context, c *client.Client, project *string, ref string) (*tunnel.Tunnel, error) {
	fetch := func(ctx context.Context) (tunnel.Token, error) {
		res, err := client.Call[api.V1DatabasesCreateTunnelResponseBody](ctx, c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
			return cl.DatabasesCreateTunnel(ctx, api.V1DatabasesCreateTunnelRequestBody{Project: project, Database: ref})
		})
		if err != nil {
			return tunnel.Token{}, err
		}
		d := res.Data
		return tunnel.Token{Token: d.Token, Host: d.Host, Port: int(d.Port), ExpiresAt: time.UnixMilli(d.ExpiresAt)}, nil
	}
	opts := tunnelOptions
	opts.OnError = func(err error) { a.IO.Warn("Tunnel connection failed: %v", err) }
	return tunnel.New(ctx, fetch, opts)
}

// listenTunnel listens on bind:port. Without an explicit port it tries 5432
// and falls back to a random port when that is taken.
func listenTunnel(bind string, port int, explicit bool) (net.Listener, error) {
	if !explicit {
		if ln, err := net.Listen("tcp", net.JoinHostPort(bind, strconv.Itoa(defaultTunnelPort))); err == nil {
			return ln, nil
		}
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(bind, strconv.Itoa(port)))
	if err != nil {
		var opErr *net.OpError
		if errors.As(err, &opErr) {
			return nil, &UserError{Message: fmt.Sprintf("Couldn't listen on %s: %v", net.JoinHostPort(bind, strconv.Itoa(port)), opErr.Err), Hint: "Pick another port with --port, or --port 0 for a random one."}
		}
		return nil, err
	}
	return ln, nil
}

// tunnelURL is the connection URL for a local tunnel endpoint. The tunnel is
// encrypted, so the local hop needs no TLS.
func tunnelURL(user, password, addr, database string) string {
	u := url.URL{Scheme: "postgresql", Host: addr, Path: "/" + database, RawQuery: "sslmode=disable"}
	if password != "" {
		u.User = url.UserPassword(user, password)
	} else {
		u.User = url.User(user)
	}
	return u.String()
}
