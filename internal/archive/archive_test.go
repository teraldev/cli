package archive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
)

func write(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func entries(t *testing.T, buf *bytes.Buffer) map[string]*tar.Header {
	t.Helper()
	gz, err := gzip.NewReader(buf)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	out := map[string]*tar.Header{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out[h.Name] = h
	}
}

func names(m map[string]*tar.Header) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestWriteWithoutGit(t *testing.T) {
	root := t.TempDir()
	write(t, root, map[string]string{
		"Dockerfile":              "FROM scratch",
		"main.go":                 "package main",
		".gitignore":              "dist/\n*.log\n",
		"dist/out.js":             "x",
		"debug.log":               "x",
		"sub/.gitignore":          "secret.txt\n",
		"sub/secret.txt":          "x",
		"sub/keep.txt":            "x",
		".teralignore":            "fixtures/\n",
		"fixtures/big.bin":        "x",
		".git/config":             "x",
		".teral/link.json":        "{}",
		"node_modules/a/index.js": "x",
		".dockerignore":           "node_modules\n",
	})
	if err := os.Chmod(filepath.Join(root, "main.go"), 0o755); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	stats, err := Write(&buf, root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	got := entries(t, &buf)
	want := []string{".dockerignore", ".gitignore", ".teralignore", "Dockerfile", "main.go", "sub/.gitignore", "sub/keep.txt"}
	if g := names(got); !equal(g, want) {
		t.Fatalf("files = %v, want %v", g, want)
	}
	if stats.Files != len(want) {
		t.Fatalf("stats.Files = %d", stats.Files)
	}
	if got["main.go"].Mode != 0o755 || got["Dockerfile"].Mode != 0o644 {
		t.Fatalf("modes: main.go %o, Dockerfile %o", got["main.go"].Mode, got["Dockerfile"].Mode)
	}
	if got["main.go"].Uid != 0 || got["main.go"].Uname != "" {
		t.Fatal("owner should be stripped")
	}
}

func TestWriteWithGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	write(t, root, map[string]string{
		".gitignore":    "build/\n",
		"tracked.txt":   "x",
		"untracked.txt": "x",
		"build/app":     "x",
	})
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("add", ".gitignore", "tracked.txt")
	run("commit", "-q", "-m", "init")
	// A tracked file deleted locally must not break packing.
	write(t, root, map[string]string{"gone.txt": "x"})
	run("add", "gone.txt")
	if err := os.Remove(filepath.Join(root, "gone.txt")); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if _, err := Write(&buf, root, Options{InGit: true}); err != nil {
		t.Fatal(err)
	}
	want := []string{".gitignore", "tracked.txt", "untracked.txt"}
	if g := names(entries(t, &buf)); !equal(g, want) {
		t.Fatalf("files = %v, want %v", g, want)
	}
}

func TestTooLarge(t *testing.T) {
	root := t.TempDir()
	write(t, root, map[string]string{"a.txt": "0123456789"})
	var buf bytes.Buffer
	if _, err := Write(&buf, root, Options{MaxBytes: 5}); err != ErrTooLarge {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
