package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDBAPI implements the databases endpoints of the Teral API, following
// the OpenAPI contract, and records the bodies the CLI sent.
type fakeDBAPI struct {
	t   *testing.T
	srv *httptest.Server

	mu         sync.Mutex
	bodies     map[string][]map[string]any
	getCalls   int
	backupList int
	tunnelPort int
}

func newFakeDBAPI(t *testing.T) *fakeDBAPI {
	f := &fakeDBAPI{t: t, bodies: map[string][]map[string]any{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeDBAPI) last(op string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	b := f.bodies[op]
	if len(b) == 0 {
		f.t.Fatalf("no call to %s", op)
	}
	return b[len(b)-1]
}

func (f *fakeDBAPI) calls(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies[op])
}

func fakeDatabase(status string) map[string]any {
	return map[string]any{
		"id": "db_1", "projectId": "proj_1", "name": "main", "engine": "postgres", "version": "17", "size": "db-nano",
		"instances": 1, "status": status, "host": "pg-1.db.teral.dev", "port": 5432, "database": "main", "username": "main_owner",
		"region": "eu-central-1", "createdAt": 1, "backupHour": 3, "backupRetentionDays": 7, "pointInTimeRecovery": true,
		"links": []map[string]any{
			{"id": "dbl_1", "appId": "app_1", "environmentId": "env_1", "envKey": "DATABASE_URL", "createdAt": 1},
			{"id": "dbl_2", "appId": "app_1", "environmentId": "env_2", "envKey": "DATABASE_URL", "createdAt": 1},
		},
	}
}

func (f *fakeDBAPI) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+fakeToken {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	op := strings.TrimPrefix(r.URL.Path, "/v1/")
	f.bodies[op] = append(f.bodies[op], body)
	reply := func(data any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"meta": map[string]string{"requestId": "req_1"}, "data": data})
	}
	switch op {
	case "databases.listOptions":
		reply(map[string]any{
			"engines": []map[string]any{
				{"engine": "postgres", "versions": []string{"16", "17"}, "defaultVersion": "17", "available": true},
				{"engine": "mysql", "versions": []string{"8.4"}, "defaultVersion": "8.4", "available": false},
			},
			"sizes": []map[string]any{
				{"slug": "db-nano", "displayName": "Nano", "cpuMillicores": 250, "memoryMib": 512, "storageMib": 5120, "maxConnections": 50, "minPlan": "free", "allowed": true},
				{"slug": "db-large", "displayName": "Large", "cpuMillicores": 2000, "memoryMib": 8192, "storageMib": 102400, "maxConnections": 400, "minPlan": "pro", "allowed": false},
			},
			"plan": "free", "maxDatabases": 1, "databaseCount": 0, "maxInstances": 1,
		})
	case "databases.createDatabase":
		reply(fakeDatabase("provisioning"))
	case "databases.listDatabases":
		reply([]any{fakeDatabase("ready")})
	case "databases.getDatabase":
		f.getCalls++
		if f.getCalls == 1 && body["database"] == "db_1" {
			reply(fakeDatabase("provisioning"))
			return
		}
		if body["database"] != "db_1" && body["project"] != "acme" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"meta":{"requestId":"req_1"},"error":{"title":"Bad Request","detail":"A database name needs a project.","status":400}}`))
			return
		}
		reply(fakeDatabase("ready"))
	case "databases.updateDatabase", "databases.rotatePassword", "databases.updateBackupSchedule":
		d := fakeDatabase("updating")
		if h, ok := body["hour"]; ok {
			d["backupHour"] = h
		}
		reply(d)
	case "databases.deleteDatabase", "databases.unlinkDatabase":
		reply(map[string]any{})
	case "databases.getCredentials":
		reply(map[string]any{"engine": "postgres", "host": "pg-1.db.teral.dev", "port": 5432, "database": "main", "username": "main_owner",
			"password": "s3cret", "url": "postgresql://main_owner:s3cret@pg-1.db.teral.dev:5432/main?sslmode=require"})
	case "databases.linkDatabase":
		key := "DATABASE_URL"
		if k, ok := body["envKey"].(string); ok {
			key = k
		}
		reply(map[string]any{"id": "dbl_3", "appId": "app_1", "environmentId": "env_1", "envKey": key, "createdAt": 1})
	case "databases.listBackups":
		f.backupList++
		status := "running"
		if f.backupList > 1 {
			status = "completed"
		}
		reply([]map[string]any{
			{"id": "dbb_2", "kind": "manual", "status": status, "createdAt": 2, "startedAt": 2},
			{"id": "dbb_1", "kind": "scheduled", "status": "completed", "createdAt": 1, "startedAt": 1000, "stoppedAt": 61000},
		})
	case "databases.createBackup":
		reply(map[string]any{"id": "dbb_2", "kind": "manual", "status": "pending", "createdAt": 2})
	case "databases.createTunnel":
		reply(map[string]any{"token": fakeTunnelToken, "host": "example.com", "port": f.tunnelPort, "expiresAt": time.Now().Add(15 * time.Minute).UnixMilli()})
	case "databases.restoreDatabase":
		d := fakeDatabase("provisioning")
		d["id"], d["name"] = "db_2", body["name"]
		reply(d)
	case "apps.listApps":
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"meta": map[string]string{"requestId": "r"}, "pagination": map[string]any{"hasMore": false},
			"data": []map[string]any{{"id": "app_1", "slug": "api", "name": "API", "createdAt": 1}}})
	case "environments.listEnvironments":
		reply([]map[string]any{
			{"id": "env_1", "slug": "production", "kind": "production", "createdAt": 1},
			{"id": "env_2", "slug": "preview", "kind": "preview", "createdAt": 1},
		})
	case "projects.listProjects":
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"meta": map[string]string{"requestId": "r"}, "pagination": map[string]any{"hasMore": false},
			"data": []map[string]any{{"id": "proj_1", "slug": "acme", "name": "Acme", "createdAt": 1}}})
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func setupDBTest(t *testing.T) *fakeDBAPI {
	t.Helper()
	f := newFakeDBAPI(t)
	t.Setenv("TERAL_CONFIG_DIR", t.TempDir())
	t.Setenv("TERAL_API_URL", f.srv.URL)
	t.Setenv("TERAL_API_KEY", fakeToken)
	for _, name := range []string{"CI", "GITHUB_ACTIONS", "TERAL_PROJECT", "TERAL_APP", "TERAL_ENVIRONMENT", "TERAL_WORKSPACE", "TERAL_OUTPUT"} {
		t.Setenv(name, "")
	}
	t.Chdir(t.TempDir())
	old := dbPollInterval
	dbPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { dbPollInterval = old })
	return f
}

func TestDatabaseCommands(t *testing.T) {
	f := setupDBTest(t)

	r := run(t, "db", "sizes")
	if r.code != 0 {
		t.Fatalf("sizes: %+v", r)
	}
	for _, want := range []string{"db-nano", "0.25 vCPU", "512 MiB", "5 GiB", "pro plan", "0 of 1", "postgres 16, 17 (default 17)", "mysql (coming soon)"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("sizes output lacks %q:\n%s", want, r.stdout)
		}
	}

	// A name needs a project; without one and no terminal, the flag is named.
	if r := run(t, "db", "get", "main"); r.code != 1 || !strings.Contains(r.err, "--project") {
		t.Fatalf("get without a project: %+v", r)
	}

	// Create and wait until ready.
	r = run(t, "db", "create", "main", "-p", "acme", "--size", "db-nano", "--instances", "2", "--wait")
	if r.code != 0 || !strings.Contains(r.err, "Created main") || !strings.Contains(r.err, "pg-1.db.teral.dev:5432") {
		t.Fatalf("create: %+v", r)
	}
	body := f.last("databases.createDatabase")
	if body["project"] != "acme" || body["name"] != "main" || body["size"] != "db-nano" || body["instances"] != float64(2) {
		t.Errorf("createDatabase body: %v", body)
	}
	if _, ok := body["version"]; ok {
		t.Errorf("createDatabase sent an empty version: %v", body)
	}

	// List across projects shows the project's slug.
	r = run(t, "db", "list")
	if r.code != 0 || !strings.Contains(r.stdout, "acme") || !strings.Contains(r.stdout, "postgres 17") {
		t.Fatalf("list: %+v", r)
	}
	if _, ok := f.last("databases.listDatabases")["project"]; ok {
		t.Error("list without a project must not send one")
	}

	// Get shows details, links by app and environment slug, and backups.
	r = run(t, "db", "get", "main", "-p", "acme")
	if r.code != 0 {
		t.Fatalf("get: %+v", r)
	}
	for _, want := range []string{"db_1", "daily at 03:00 UTC, kept 7 days, point-in-time recovery", "dbl_2", "preview", "DATABASE_URL", "dbb_1", "1m 0s"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("get output lacks %q:\n%s", want, r.stdout)
		}
	}
	if r := run(t, "db", "get", "db_1", "-o", "json"); r.code != 0 || !strings.Contains(r.stdout, `"backups"`) {
		t.Fatalf("get json: %+v", r)
	}
	if _, ok := f.last("databases.getDatabase")["project"]; ok {
		t.Error("an id needs no project")
	}

	// Credentials print the URL and warn about the audit log.
	r = run(t, "db", "credentials", "db_1")
	if r.code != 0 || strings.TrimSpace(r.stdout) != "postgresql://main_owner:s3cret@pg-1.db.teral.dev:5432/main?sslmode=require" || !strings.Contains(r.err, "audit log") {
		t.Fatalf("credentials: %+v", r)
	}
	if r := run(t, "db", "credentials", "db_1", "-o", "json"); r.code != 0 || !strings.Contains(r.stdout, `"password": "s3cret"`) {
		t.Fatalf("credentials json: %+v", r)
	}

	// Resize.
	if r := run(t, "db", "resize", "db_1"); r.code != 1 || !strings.Contains(r.err, "--size") {
		t.Fatalf("resize without changes: %+v", r)
	}
	if r := run(t, "db", "resize", "db_1", "--size", "db-small"); r.code != 0 || !strings.Contains(r.err, "Resizing main") {
		t.Fatalf("resize: %+v", r)
	}
	if body := f.last("databases.updateDatabase"); body["size"] != "db-small" || body["instances"] != nil {
		t.Errorf("updateDatabase body: %v", body)
	}

	// Destructive commands need confirmation.
	if r := run(t, "db", "rotate", "db_1"); r.code != 1 || !strings.Contains(r.err, "--yes") {
		t.Fatalf("rotate without --yes: %+v", r)
	}
	if r := run(t, "db", "rotate", "db_1", "--yes"); r.code != 0 || f.calls("databases.rotatePassword") != 1 {
		t.Fatalf("rotate: %+v", r)
	}
	if r := run(t, "db", "delete", "db_1"); r.code != 1 || !strings.Contains(r.err, "--yes") || f.calls("databases.deleteDatabase") != 0 {
		t.Fatalf("delete without --yes: %+v", r)
	}
	if r := run(t, "db", "delete", "main", "-p", "acme", "--yes"); r.code != 0 || f.last("databases.deleteDatabase")["database"] != "db_1" {
		t.Fatalf("delete: %+v", r)
	}

	// Link with the global --app/--env and a custom variable.
	r = run(t, "db", "link", "main", "-p", "acme", "--app", "api", "--env-key", "PG_URL")
	if r.code != 0 || !strings.Contains(r.err, "PG_URL") {
		t.Fatalf("link: %+v", r)
	}
	if body := f.last("databases.linkDatabase"); body["app"] != "api" || body["environment"] != "production" || body["project"] != "acme" || body["envKey"] != "PG_URL" {
		t.Errorf("linkDatabase body: %v", body)
	}

	// Unlink by app: ambiguous across environments until --env picks one.
	if r := run(t, "db", "unlink", "db_1", "api", "--yes"); r.code != 1 || !strings.Contains(r.err, "2 environments") {
		t.Fatalf("ambiguous unlink: %+v", r)
	}
	if r := run(t, "db", "unlink", "db_1", "api", "--env", "preview", "--yes"); r.code != 0 {
		t.Fatalf("unlink by app: %+v", r)
	}
	if body := f.last("databases.unlinkDatabase"); body["linkId"] != "dbl_2" || body["removeVariable"] != true {
		t.Errorf("unlinkDatabase body: %v", body)
	}
	if r := run(t, "db", "unlink", "db_1", "dbl_1", "--keep-variable", "--yes"); r.code != 0 {
		t.Fatalf("unlink by id: %+v", r)
	}
	if body := f.last("databases.unlinkDatabase"); body["linkId"] != "dbl_1" || body["removeVariable"] != false {
		t.Errorf("unlinkDatabase body: %v", body)
	}

	// Backups.
	r = run(t, "db", "backup", "db_1", "--wait")
	if r.code != 0 || !strings.Contains(r.err, "Backed up db_1") {
		t.Fatalf("backup --wait: %+v", r)
	}
	if r := run(t, "db", "backups", "db_1"); r.code != 0 || !strings.Contains(r.stdout, "scheduled") {
		t.Fatalf("backups: %+v", r)
	}
	if r := run(t, "db", "backup-schedule", "db_1", "--hour", "24"); r.code != 1 {
		t.Fatalf("backup-schedule out of range: %+v", r)
	}
	if r := run(t, "db", "backup-schedule", "db_1", "--hour", "0"); r.code != 0 || !strings.Contains(r.err, "00:00 UTC") {
		t.Fatalf("backup-schedule: %+v", r)
	}

	// Restore.
	if r := run(t, "db", "restore", "db_1"); r.code != 1 || !strings.Contains(r.err, "--name") {
		t.Fatalf("restore without --name: %+v", r)
	}
	if r := run(t, "db", "restore", "db_1", "--name", "x", "--backup", "dbb_1", "--to", "1h"); r.code != 1 {
		t.Fatalf("restore with --backup and --to: %+v", r)
	}
	if r := run(t, "db", "restore", "db_1", "--name", "main-before", "--to", "2026-11-26T09:30:00Z"); r.code != 0 || !strings.Contains(r.err, "Restoring db_1 into main-before") {
		t.Fatalf("restore: %+v", r)
	}
	want := float64(time.Date(2026, 11, 26, 9, 30, 0, 0, time.UTC).UnixMilli())
	if body := f.last("databases.restoreDatabase"); body["name"] != "main-before" || body["targetTime"] != want || body["backupId"] != nil {
		t.Errorf("restoreDatabase body: %v", body)
	}
}

func TestDatabaseConnect(t *testing.T) {
	setupDBTest(t)

	t.Setenv("PATH", t.TempDir())
	if r := run(t, "db", "connect", "db_1"); r.code != 1 || !strings.Contains(r.err, "psql isn't installed") {
		t.Fatalf("connect without psql: %+v", r)
	}

	if runtime.GOOS == "windows" {
		t.Skip("the fake psql is a shell script")
	}
	bin := t.TempDir()
	out := filepath.Join(bin, "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + out + "\nexit 3\n"
	if err := os.WriteFile(filepath.Join(bin, "psql"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	if r := run(t, "db", "connect", "db_1", "--", "-c", "select 1"); r.code != 3 {
		t.Fatalf("connect should pass on psql's exit code: %+v", r)
	}
	got, _ := os.ReadFile(out)
	if string(got) != "postgresql://main_owner:s3cret@pg-1.db.teral.dev:5432/main?sslmode=require\n-c\nselect 1\n" {
		t.Errorf("psql arguments: %q", got)
	}
}
