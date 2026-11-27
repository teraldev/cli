// Package tunnel connects local clients to a Teral database through the
// database proxy on its server, authenticated with a short-lived token from
// the API (databases.createTunnel) instead of only the database password.
//
// The wire protocol, per connection:
//
//  1. TLS to the database's host and port, verified against the system roots,
//     with the host as the server name and the ALPN protocol "teral-tunnel".
//  2. The client sends "TERAL-TUNNEL <token>\n".
//  3. The proxy answers "OK\n" or "ERR <reason>\n" (and closes).
//  4. After OK the connection carries the Postgres protocol in the clear
//     inside the TLS connection, so local clients use sslmode=disable.
package tunnel

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ALPN is the TLS application protocol the database proxy serves tunnels on.
const ALPN = "teral-tunnel"

const (
	// refreshBefore is how long before its expiry a token is replaced, when
	// a new connection needs one.
	refreshBefore = time.Minute
	// maxAnswer caps the proxy's answer line.
	maxAnswer = 1024
)

// Token is what the API hands out for a database.
type Token struct {
	Token     string
	Host      string
	Port      int
	ExpiresAt time.Time
}

// Addr is the proxy's host:port.
func (t Token) Addr() string { return net.JoinHostPort(t.Host, strconv.Itoa(t.Port)) }

// FetchFunc asks the API for a new token.
type FetchFunc func(ctx context.Context) (Token, error)

// Options tune a [Tunnel]. The zero value is right outside of tests.
type Options struct {
	// RootCAs verifies the proxy's certificate. Nil uses the system roots.
	RootCAs *x509.CertPool
	// Dial opens the TCP connection to the proxy. Nil uses a net.Dialer.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// Now is the clock for token expiry. Nil uses time.Now.
	Now func() time.Time
	// HandshakeTimeout bounds dialing, TLS and the token exchange. Zero is 15s.
	HandshakeTimeout time.Duration
	// OnError is told about connections that failed. Nil drops them.
	OnError func(error)
}

// RefusedError is the proxy's ERR answer.
type RefusedError struct{ Reason string }

func (e *RefusedError) Error() string { return "the database proxy refused the tunnel: " + e.Reason }

// Tunnel opens tunnel connections to one database.
type Tunnel struct {
	fetch FetchFunc
	opts  Options

	mu  sync.Mutex
	tok Token
}

// New fetches the first token, so errors such as missing permissions show
// before anything listens.
func New(ctx context.Context, fetch FetchFunc, opts Options) (*Tunnel, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Dial == nil {
		d := &net.Dialer{KeepAlive: 30 * time.Second}
		opts.Dial = d.DialContext
	}
	if opts.HandshakeTimeout <= 0 {
		opts.HandshakeTimeout = 15 * time.Second
	}
	if opts.OnError == nil {
		opts.OnError = func(error) {}
	}
	t := &Tunnel{fetch: fetch, opts: opts}
	tok, err := fetch(ctx)
	if err != nil {
		return nil, err
	}
	t.tok = tok
	return t, nil
}

// Token returns the current token.
func (t *Tunnel) Token() Token {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.tok
}

// token returns a token with more than refreshBefore left, fetching a new
// one when needed or when force is set.
func (t *Tunnel) token(ctx context.Context, force bool) (Token, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !force && t.tok.ExpiresAt.Sub(t.opts.Now()) > refreshBefore {
		return t.tok, nil
	}
	tok, err := t.fetch(ctx)
	if err != nil {
		return Token{}, fmt.Errorf("renewing the tunnel token: %w", err)
	}
	t.tok = tok
	return tok, nil
}

// Dial opens one tunnel connection, ready for the Postgres protocol. A token
// the proxy calls expired is renewed and the dial retried once.
func (t *Tunnel) Dial(ctx context.Context) (net.Conn, error) {
	tok, err := t.token(ctx, false)
	if err != nil {
		return nil, err
	}
	conn, err := t.dial(ctx, tok)
	var refused *RefusedError
	if errors.As(err, &refused) && refused.Reason == "token expired" {
		if tok, err = t.token(ctx, true); err != nil {
			return nil, err
		}
		conn, err = t.dial(ctx, tok)
	}
	return conn, err
}

func (t *Tunnel) dial(ctx context.Context, tok Token) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, t.opts.HandshakeTimeout)
	defer cancel()

	raw, err := t.opts.Dial(ctx, "tcp", tok.Addr())
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", tok.Addr(), err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = raw.SetDeadline(dl)
	}
	conn := tls.Client(raw, &tls.Config{
		ServerName: tok.Host,
		RootCAs:    t.opts.RootCAs,
		NextProtos: []string{ALPN},
		MinVersion: tls.VersionTLS12,
	})
	if err := conn.HandshakeContext(ctx); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("TLS handshake with %s: %w", tok.Host, err)
	}
	if conn.ConnectionState().NegotiatedProtocol != ALPN {
		_ = conn.Close()
		return nil, fmt.Errorf("%s does not support tunnels yet", tok.Host)
	}
	if _, err := io.WriteString(conn, "TERAL-TUNNEL "+tok.Token+"\n"); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("sending the tunnel token: %w", err)
	}
	answer, err := readLine(conn)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("reading the proxy's answer: %w", err)
	}
	switch {
	case answer == "OK":
	case strings.HasPrefix(answer, "ERR "):
		_ = conn.Close()
		return nil, &RefusedError{Reason: strings.TrimPrefix(answer, "ERR ")}
	default:
		_ = conn.Close()
		return nil, fmt.Errorf("unexpected answer from the database proxy: %q", answer)
	}
	_ = raw.SetDeadline(time.Time{})
	return conn, nil
}

// readLine reads one '\n' terminated line byte by byte, so nothing after it
// is consumed.
func readLine(r io.Reader) (string, error) {
	var b [1]byte
	var line []byte
	for len(line) < maxAnswer {
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return "", err
		}
		if b[0] == '\n' {
			return strings.TrimSuffix(string(line), "\r"), nil
		}
		line = append(line, b[0])
	}
	return "", errors.New("answer line too long")
}

// Serve accepts local connections on ln and tunnels each one until ctx is
// done, then closes ln and every open connection.
func (t *Tunnel) Serve(ctx context.Context, ln net.Listener) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		local, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			t.handle(ctx, local)
		}()
	}
}

func (t *Tunnel) handle(ctx context.Context, local net.Conn) {
	defer local.Close()
	remote, err := t.Dial(ctx)
	if err != nil {
		t.opts.OnError(err)
		return
	}
	defer remote.Close()

	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(remote, local); done <- struct{}{} }()
	go func() { _, _ = io.Copy(local, remote); done <- struct{}{} }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}
