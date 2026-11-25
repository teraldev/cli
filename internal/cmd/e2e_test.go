package cmd

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/teraldev/cli/internal/ui"
)

// fakeAPI implements the slice of the Teral API the CLI uses, following the
// OpenAPI contract, and records what the CLI sent.
type fakeAPI struct {
	t   *testing.T
	srv *httptest.Server

	mu          sync.Mutex
	polls       int
	statusCalls int
	uploaded    map[string]string
	deployBody  map[string]any
	vars        map[string]string
	revoked     bool
}

const fakeToken = "teral_cli_testtoken"

func newFakeAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{t: t, vars: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if !strings.HasPrefix(r.Header.Get("X-Teral-Client"), "teral-cli/") && r.Method == http.MethodPost {
		f.t.Errorf("%s: missing X-Teral-Client header", r.URL.Path)
	}
	public := r.URL.Path == "/v1/cli.startDeviceAuthorization" || r.URL.Path == "/v1/cli.pollDeviceAuthorization" || strings.HasPrefix(r.URL.Path, "/upload/")
	if !public && r.Header.Get("Authorization") != "Bearer "+fakeToken {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"meta":{"requestId":"req_1"},"error":{"title":"Unauthorized","detail":"This CLI session was revoked. Run ` + "`teral login`" + ` to sign in again.","status":401}}`))
		return
	}

	var body map[string]any
	if r.Method == http.MethodPost {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	reply := func(data any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"meta": map[string]string{"requestId": "req_1"}, "data": data})
	}
	deployment := func(status string) map[string]any {
		return map[string]any{
			"id": "d_1", "status": status, "isCurrent": status == "ready", "environment": "production", "app": "api", "project": "acme",
			"availableActions": []string{}, "regions": []string{"eu-central-1"}, "runtime": map[string]any{}, "createdAt": 1,
			"domains": []string{"api-acme.teral.app"}, "upload": map[string]any{"uploadId": "upl_1", "dirty": true},
		}
	}

	switch r.URL.Path {
	case "/v1/cli.startDeviceAuthorization":
		if body["clientName"] == nil || body["deviceName"] == nil || body["deviceOs"] == nil {
			f.t.Errorf("start: incomplete body %v", body)
		}
		reply(map[string]any{
			"deviceCode": "tdc_secret", "userCode": "WDJB-MJHT",
			"verificationUri": "https://app.example/cli", "verificationUriComplete": "https://app.example/cli?code=WDJB-MJHT",
			"expiresIn": 600, "interval": 1,
		})
	case "/v1/cli.pollDeviceAuthorization":
		f.polls++
		if body["deviceCode"] != "tdc_secret" {
			f.t.Errorf("poll: device code %v", body["deviceCode"])
		}
		if f.polls < 2 {
			reply(map[string]any{"status": "pending", "interval": 1})
			return
		}
		reply(map[string]any{"status": "approved", "session": map[string]any{
			"token": fakeToken, "sessionId": "clis_1", "expiresAt": 4102444800000,
			"workspace": map[string]any{"id": "ws_1", "slug": "acme", "name": "Acme"},
			"user":      map[string]any{"id": "user_1", "name": "Fynn", "email": "fynn@example.com"},
		}})
	case "/v1/auth.whoami":
		reply(map[string]any{
			"workspace": map[string]any{"id": "ws_1", "slug": "acme", "name": "Acme"},
			"principal": map[string]any{"type": "cli_session", "id": "user_1", "name": "Fynn", "email": "fynn@example.com", "sessionId": "clis_1"},
			"roles":     []string{"admin"}, "permissions": []string{"teral:v1:ws_1:**#*"},
		})
	case "/v1/environments.getEnvironment":
		reply(map[string]any{"id": "env_1", "slug": "production", "kind": "production", "description": "", "deleteProtection": false, "createdAt": 1,
			"build": map[string]any{"autoDeploy": true, "rootDirectory": "", "watchPaths": []string{}}})
	case "/v1/deployments.createUpload":
		if body["sizeBytes"] == nil {
			f.t.Error("createUpload: no size")
		}
		reply(map[string]any{"uploadId": "upl_1", "uploadUrl": f.srv.URL + "/upload/upl_1", "method": "PUT",
			"headers": map[string]string{"Content-Type": "application/gzip"}, "expiresAt": 4102444800000, "maxSizeBytes": 524288000})
	case "/upload/upl_1":
		if r.Method != http.MethodPut || r.Header.Get("Content-Type") != "application/gzip" || r.ContentLength <= 0 {
			f.t.Errorf("upload: %s %s length %d", r.Method, r.Header.Get("Content-Type"), r.ContentLength)
		}
		f.uploaded = untar(f.t, r.Body)
		w.WriteHeader(http.StatusOK)
	case "/v1/deployments.createDeployment":
		f.deployBody = body
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"meta": map[string]string{"requestId": "req_1"}, "data": map[string]string{"deploymentId": "d_1"}})
	case "/v1/deployments.getDeployment":
		f.statusCalls++
		switch {
		case f.statusCalls <= 1:
			reply(deployment("building"))
		case f.statusCalls == 2:
			reply(deployment("deploying"))
		default:
			reply(deployment("ready"))
		}
	case "/v1/logs.getBuildLogs":
		reply(map[string]any{"status": "building", "nextSeq": 2, "hasMore": false,
			"steps": []map[string]any{{"id": "s1", "name": "[1/2] FROM node:22", "startedAt": 1, "cached": true}},
			"lines": []map[string]any{{"seq": 1, "time": 1, "stepId": "s1", "message": "resolve done", "stderr": false}}})
	case "/v1/environments.setEnvironmentVariables":
		for _, v := range body["variables"].([]any) {
			m := v.(map[string]any)
			f.vars[m["key"].(string)] = m["value"].(string)
		}
		reply(map[string]any{})
	case "/v1/environments.listEnvironmentVariables":
		var data []map[string]any
		for k, v := range f.vars {
			data = append(data, map[string]any{"key": k, "kind": "recoverable", "value": v, "createdAt": 1})
		}
		data = append(data, map[string]any{"key": "STRIPE_KEY", "kind": "writeonly", "createdAt": 1})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"meta": map[string]string{"requestId": "r"}, "data": data, "pagination": map[string]any{"hasMore": false}})
	case "/v1/cli.revokeSession":
		f.revoked = true
		reply(map[string]any{"sessionId": "clis_1"})
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func untar(t *testing.T, r io.Reader) map[string]string {
	gz, err := gzip.NewReader(r)
	if err != nil {
		t.Errorf("upload is not gzip: %v", err)
		return nil
	}
	tr := tar.NewReader(gz)
	out := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Errorf("upload is not a tar: %v", err)
			return out
		}
		b, _ := io.ReadAll(tr)
		out[h.Name] = string(b)
	}
}

type result struct {
	code        int
	stdout, err string
}

func run(t *testing.T, args ...string) result {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Run(context.Background(), ui.New(strings.NewReader(""), &out, &errOut), args)
	return result{code, out.String(), errOut.String()}
}

func TestEndToEnd(t *testing.T) {
	api := newFakeAPI(t)
	t.Setenv("TERAL_CONFIG_DIR", t.TempDir())
	t.Setenv("TERAL_API_URL", api.srv.URL)
	t.Setenv("TERAL_NO_KEYRING", "1")
	for _, name := range []string{"CI", "GITHUB_ACTIONS", "TERAL_API_KEY", "TERAL_TOKEN", "TERAL_PROJECT", "TERAL_APP", "TERAL_ENVIRONMENT", "TERAL_WORKSPACE"} {
		t.Setenv(name, "")
	}
	project := t.TempDir()
	t.Chdir(project)
	for name, content := range map[string]string{
		"Dockerfile": "FROM node:22", "server.js": "console.log(1)", ".gitignore": "secret.env\n", "secret.env": "X=1",
		"teral.toml": "[deploy]\nport = 3000\n",
	} {
		if err := os.WriteFile(filepath.Join(project, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Not signed in yet.
	if r := run(t, "whoami"); r.code != 1 || !strings.Contains(r.err, "not signed in") {
		t.Fatalf("whoami before login: %+v", r)
	}

	// Login through the device flow.
	r := run(t, "login", "--no-browser")
	if r.code != 0 || !strings.Contains(r.err, "WDJB-MJHT") || !strings.Contains(r.err, "Logged in to Acme as fynn@example.com") {
		t.Fatalf("login: %+v", r)
	}
	creds, err := os.ReadFile(filepath.Join(os.Getenv("TERAL_CONFIG_DIR"), "credentials.toml"))
	if err != nil || !strings.Contains(string(creds), fakeToken) {
		t.Fatalf("token not stored in the credentials file: %v", err)
	}
	// Windows has no Unix permissions; the file lives in the user's profile.
	if info, _ := os.Stat(filepath.Join(os.Getenv("TERAL_CONFIG_DIR"), "credentials.toml")); runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("credentials file mode %o", info.Mode().Perm())
	}

	if r := run(t, "whoami", "-o", "json"); r.code != 0 || !strings.Contains(r.stdout, `"cli_session"`) {
		t.Fatalf("whoami: %+v", r)
	}

	// Link non-interactively.
	if r := run(t, "link", "--project", "acme", "--app", "api", "--env", "production"); r.code != 0 {
		t.Fatalf("link: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(project, ".teral", "link.json")); err != nil {
		t.Fatal("link file missing")
	}

	// Deploy the directory and follow it.
	r = run(t, "up")
	if r.code != 0 {
		t.Fatalf("up: %+v", r)
	}
	for _, want := range []string{"Uploaded", "[1/2] FROM node:22", "resolve done", "Deployed in", "https://api-acme.teral.app"} {
		if !strings.Contains(r.err, want) {
			t.Errorf("up output lacks %q:\n%s", want, r.err)
		}
	}
	for _, name := range []string{"Dockerfile", "server.js", "teral.toml", ".gitignore"} {
		if _, ok := api.uploaded[name]; !ok {
			t.Errorf("archive lacks %s: %v", name, keys(api.uploaded))
		}
	}
	for _, name := range []string{"secret.env", ".teral/link.json"} {
		if _, ok := api.uploaded[name]; ok {
			t.Errorf("archive must not contain %s", name)
		}
	}
	upload, _ := api.deployBody["upload"].(map[string]any)
	if upload["uploadId"] != "upl_1" || upload["configFile"] != "[deploy]\nport = 3000\n" {
		t.Errorf("createDeployment upload source: %v", upload)
	}
	if api.deployBody["project"] != "acme" || api.deployBody["app"] != "api" || api.deployBody["environment"] != "production" {
		t.Errorf("createDeployment target: %v", api.deployBody)
	}

	// Variables round trip.
	if r := run(t, "env", "set", "LOG_LEVEL=debug", "GREETING=hello world"); r.code != 0 {
		t.Fatalf("env set: %+v", r)
	}
	if r := run(t, "env", "pull", "local.env"); r.code != 0 {
		t.Fatalf("env pull: %+v", r)
	}
	pulled, _ := os.ReadFile(filepath.Join(project, "local.env"))
	for _, want := range []string{"LOG_LEVEL=debug", `GREETING="hello world"`, "# STRIPE_KEY is write-only"} {
		if !strings.Contains(string(pulled), want) {
			t.Errorf("pulled file lacks %q:\n%s", want, pulled)
		}
	}
	if r := run(t, "env", "get", "LOG_LEVEL"); r.code != 0 || strings.TrimSpace(r.stdout) != "debug" {
		t.Errorf("env get: %+v", r)
	}

	// Logout revokes the session and forgets the token.
	if r := run(t, "logout"); r.code != 0 || !api.revoked {
		t.Fatalf("logout: %+v revoked=%v", r, api.revoked)
	}
	if r := run(t, "whoami"); r.code != 1 || !strings.Contains(r.err, "not signed in") {
		t.Fatalf("whoami after logout: %+v", r)
	}
}

func TestAPIKeyFromEnvironment(t *testing.T) {
	api := newFakeAPI(t)
	t.Setenv("TERAL_CONFIG_DIR", t.TempDir())
	t.Setenv("TERAL_API_URL", api.srv.URL)
	t.Setenv("TERAL_API_KEY", fakeToken)
	t.Chdir(t.TempDir())
	if r := run(t, "whoami"); r.code != 0 || !strings.Contains(r.stdout, "Acme") {
		t.Fatalf("whoami with TERAL_API_KEY: %+v", r)
	}
	t.Setenv("TERAL_API_KEY", "teral_wrong")
	r := run(t, "whoami")
	if r.code != 1 || !strings.Contains(r.err, "TERAL_API_KEY") {
		t.Fatalf("bad key should point at TERAL_API_KEY: %+v", r)
	}
}

func TestNonInteractiveMissingTarget(t *testing.T) {
	api := newFakeAPI(t)
	t.Setenv("TERAL_CONFIG_DIR", t.TempDir())
	t.Setenv("TERAL_API_URL", api.srv.URL)
	t.Setenv("TERAL_API_KEY", fakeToken)
	for _, name := range []string{"TERAL_PROJECT", "TERAL_APP"} {
		t.Setenv(name, "")
	}
	t.Chdir(t.TempDir())
	r := run(t, "up")
	if r.code != 1 || !strings.Contains(r.err, "--project") {
		t.Fatalf("up without a target should name the flag: %+v", r)
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
