package tunnel

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// FakeProxy is an httptest TLS server that speaks the dbproxy side of the
// tunnel protocol on ALPN teral-tunnel and then echoes, standing in for the
// database. Its certificate is for example.com.
type fakeProxy struct {
	srv *httptest.Server

	mu      sync.Mutex
	tokens  []string
	sni     []string
	refuse  map[string]string // token -> reason
	allowed map[string]bool
}

func newFakeProxy(t *testing.T) *fakeProxy {
	t.Helper()
	p := &fakeProxy{refuse: map[string]string{}, allowed: map[string]bool{}}
	p.srv = httptest.NewUnstartedServer(http.NotFoundHandler())
	p.srv.TLS = &tls.Config{NextProtos: []string{ALPN}}
	p.srv.Config.TLSNextProto = map[string]func(*http.Server, *tls.Conn, http.Handler){
		ALPN: func(_ *http.Server, c *tls.Conn, _ http.Handler) { p.serve(c) },
	}
	p.srv.StartTLS()
	t.Cleanup(p.srv.Close)
	return p
}

func (p *fakeProxy) serve(c *tls.Conn) {
	defer c.Close()
	line, err := readLine(c)
	if err != nil {
		return
	}
	token := strings.TrimPrefix(line, "TERAL-TUNNEL ")
	p.mu.Lock()
	p.tokens = append(p.tokens, token)
	p.sni = append(p.sni, c.ConnectionState().ServerName)
	reason, refused := p.refuse[token]
	ok := p.allowed[token]
	p.mu.Unlock()
	switch {
	case refused:
		_, _ = io.WriteString(c, "ERR "+reason+"\n")
		return
	case !ok:
		_, _ = io.WriteString(c, "ERR invalid token\n")
		return
	}
	_, _ = io.WriteString(c, "OK\n")
	_, _ = io.Copy(c, c)
}

func (p *fakeProxy) seen() ([]string, []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.tokens...), append([]string(nil), p.sni...)
}

func (p *fakeProxy) pool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(p.srv.Certificate())
	return pool
}

// options dial example.com:5432 to the fake proxy and trust its certificate.
func (p *fakeProxy) options(now func() time.Time, onError func(error)) Options {
	addr := p.srv.Listener.Addr().String()
	return Options{
		RootCAs: p.pool(),
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
		Now:     now,
		OnError: onError,
	}
}

// fetcher hands out tok-1, tok-2, ... each valid for ttl from clock().
type fetcher struct {
	mu    sync.Mutex
	n     int
	ttl   time.Duration
	clock func() time.Time
}

func (f *fetcher) fetch(context.Context) (Token, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	return Token{Token: "tok-" + string(rune('0'+f.n)), Host: "example.com", Port: 5432, ExpiresAt: f.clock().Add(f.ttl)}, nil
}

func (f *fetcher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func echo(t *testing.T, conn net.Conn, msg string) {
	t.Helper()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(conn, msg); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != msg {
		t.Fatalf("echo %q, want %q", got, msg)
	}
}

func TestServeAndRefresh(t *testing.T) {
	p := newFakeProxy(t)
	p.allowed["tok-1"], p.allowed["tok-2"] = true, true
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	f := &fetcher{ttl: 15 * time.Minute, clock: clock.now}

	errs := make(chan error, 10)
	tun, err := New(context.Background(), f.fetch, p.options(clock.now, func(err error) { errs <- err }))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- tun.Serve(ctx, ln) }()

	c1, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()
	echo(t, c1, "SELECT 1")

	// With less than a minute left, the next connection gets a new token;
	// the open one keeps going.
	clock.add(14*time.Minute + 30*time.Second)
	c2, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	echo(t, c2, "SELECT 2")
	echo(t, c1, "still here")

	tokens, sni := p.seen()
	if strings.Join(tokens, ",") != "tok-1,tok-2" || f.count() != 2 {
		t.Fatalf("tokens %v, fetches %d", tokens, f.count())
	}
	if sni[0] != "example.com" {
		t.Fatalf("SNI %q", sni[0])
	}

	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after cancel")
	}
	if _, err := c1.Read(make([]byte, 1)); err == nil {
		t.Fatal("connection still open after the tunnel closed")
	}
	select {
	case err := <-errs:
		t.Fatalf("unexpected connection error: %v", err)
	default:
	}
}

func TestExpiredTokenIsRenewed(t *testing.T) {
	p := newFakeProxy(t)
	p.refuse["tok-1"] = "token expired"
	p.allowed["tok-2"] = true
	f := &fetcher{ttl: 15 * time.Minute, clock: time.Now}
	tun, err := New(context.Background(), f.fetch, p.options(nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := tun.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	echo(t, conn, "hello")
	if f.count() != 2 {
		t.Fatalf("fetches %d", f.count())
	}
}

func TestRefused(t *testing.T) {
	p := newFakeProxy(t)
	f := &fetcher{ttl: 15 * time.Minute, clock: time.Now}
	errs := make(chan error, 1)
	tun, err := New(context.Background(), f.fetch, p.options(nil, func(err error) { errs <- err }))
	if err != nil {
		t.Fatal(err)
	}
	_, err = tun.Dial(context.Background())
	var refused *RefusedError
	if !errors.As(err, &refused) || refused.Reason != "invalid token" {
		t.Fatalf("err = %v", err)
	}

	// Through Serve, the local connection is closed and the error reported.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = tun.Serve(ctx, ln) }()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := bufio.NewReader(c).ReadByte(); err == nil {
		t.Fatal("refused connection stayed open")
	}
	select {
	case err := <-errs:
		if !errors.As(err, &refused) {
			t.Fatalf("reported %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no error reported")
	}
}

func TestUntrustedCertificate(t *testing.T) {
	p := newFakeProxy(t)
	p.allowed["tok-1"] = true
	f := &fetcher{ttl: 15 * time.Minute, clock: time.Now}
	opts := p.options(nil, nil)
	opts.RootCAs = x509.NewCertPool() // not the proxy's CA
	tun, err := New(context.Background(), f.fetch, opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tun.Dial(context.Background()); err == nil || !strings.Contains(err.Error(), "TLS handshake") {
		t.Fatalf("err = %v", err)
	}
	if tokens, _ := p.seen(); len(tokens) != 0 {
		t.Fatal("token sent to an untrusted server")
	}
}

func TestNoTunnelSupport(t *testing.T) {
	// A proxy that only speaks postgresql refuses the ALPN protocol.
	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	srv.TLS = &tls.Config{NextProtos: []string{"postgresql"}}
	srv.StartTLS()
	defer srv.Close()
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	addr := srv.Listener.Addr().String()
	f := &fetcher{ttl: 15 * time.Minute, clock: time.Now}
	tun, err := New(context.Background(), f.fetch, Options{
		RootCAs: pool,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tun.Dial(context.Background()); err == nil {
		t.Fatal("dial succeeded without tunnel support")
	}
}
