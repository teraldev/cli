package cmd

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/teraldev/cli/internal/tunnel"
	"github.com/teraldev/cli/internal/ui"
)

const fakeTunnelToken = "tunnel-token-for-tests"

// startFakeDBProxy runs an httptest TLS server that answers the tunnel
// handshake like dbproxy and then echoes in place of the database, and
// points tunnels at it (as example.com, the name in its certificate).
func startFakeDBProxy(t *testing.T, f *fakeDBAPI) {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	srv.TLS = &tls.Config{NextProtos: []string{tunnel.ALPN}}
	srv.Config.TLSNextProto = map[string]func(*http.Server, *tls.Conn, http.Handler){
		tunnel.ALPN: func(_ *http.Server, c *tls.Conn, _ http.Handler) {
			defer c.Close()
			line := make([]byte, 0, 64)
			b := make([]byte, 1)
			for {
				if _, err := io.ReadFull(c, b); err != nil {
					return
				}
				if b[0] == '\n' {
					break
				}
				line = append(line, b[0])
			}
			if string(line) != "TERAL-TUNNEL "+fakeTunnelToken || c.ConnectionState().ServerName != "example.com" {
				_, _ = io.WriteString(c, "ERR invalid token\n")
				return
			}
			_, _ = io.WriteString(c, "OK\n")
			_, _ = io.Copy(c, c)
		},
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	addr := srv.Listener.Addr().(*net.TCPAddr)
	f.mu.Lock()
	f.tunnelPort = addr.Port
	f.mu.Unlock()
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	old := tunnelOptions
	tunnelOptions = tunnel.Options{
		RootCAs: pool,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr.String())
		},
	}
	t.Cleanup(func() { tunnelOptions = old })
}

// syncBuffer is a bytes.Buffer safe to read while the command writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestDatabaseTunnel(t *testing.T) {
	f := setupDBTest(t)
	startFakeDBProxy(t, f)

	var out, errOut syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exited := make(chan int, 1)
	go func() {
		exited <- Run(ctx, ui.New(strings.NewReader(""), &out, &errOut), []string{"db", "tunnel", "db_1", "--port", "0"})
	}()

	urlRe := regexp.MustCompile(`postgresql://main_owner@(127\.0\.0\.1:\d+)/main\?sslmode=disable`)
	var addr string
	deadline := time.Now().Add(5 * time.Second)
	for addr == "" {
		if m := urlRe.FindStringSubmatch(out.String()); m != nil {
			addr = m[1]
			break
		}
		select {
		case code := <-exited:
			t.Fatalf("tunnel exited with %d: %s", code, errOut.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("no connection URL printed; stdout %q, stderr %q", out.String(), errOut.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.Contains(errOut.String(), "teral db credentials main") {
		t.Errorf("no hint about the password: %q", errOut.String())
	}

	for i := range 2 {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		msg := []byte("SELECT " + string(rune('1'+i)))
		if _, err := conn.Write(msg); err != nil {
			t.Fatal(err)
		}
		got := make([]byte, len(msg))
		if _, err := io.ReadFull(conn, got); err != nil || !bytes.Equal(got, msg) {
			t.Fatalf("echo %q, %v", got, err)
		}
		_ = conn.Close()
	}
	if body := f.last("databases.createTunnel"); body["database"] != "db_1" {
		t.Errorf("createTunnel body: %v", body)
	}
	if n := f.calls("databases.createTunnel"); n != 1 {
		t.Errorf("createTunnel called %d times; the token is still fresh", n)
	}

	cancel()
	select {
	case code := <-exited:
		if code != 0 {
			t.Fatalf("exit code %d: %s", code, errOut.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("tunnel did not close")
	}
	if _, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		t.Fatal("still listening after close")
	}
}

func TestListenTunnelPortTaken(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	if _, err := listenTunnel("127.0.0.1", port, true); err == nil || !strings.Contains(err.Error(), "Couldn't listen") {
		t.Fatalf("err = %v", err)
	}
}

func TestDatabaseConnectTunnel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake psql is a shell script")
	}
	f := setupDBTest(t)
	startFakeDBProxy(t, f)

	bin := t.TempDir()
	out := filepath.Join(bin, "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" \"PGPASSWORD=$PGPASSWORD\" > " + out + "\n"
	if err := os.WriteFile(filepath.Join(bin, "psql"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("PGPASSWORD", "")

	r := run(t, "db", "connect", "db_1", "--tunnel", "--", "-c", "select 1")
	if r.code != 0 || !strings.Contains(r.err, "through a tunnel") {
		t.Fatalf("connect --tunnel: %+v", r)
	}
	got, _ := os.ReadFile(out)
	re := regexp.MustCompile(`^postgresql://main_owner@127\.0\.0\.1:\d+/main\?sslmode=disable\n-c\nselect 1\nPGPASSWORD=s3cret\n$`)
	if !re.Match(got) {
		t.Errorf("psql arguments: %q", got)
	}
	if body := f.last("databases.createTunnel"); body["database"] != "db_1" {
		t.Errorf("createTunnel body: %v", body)
	}
}
