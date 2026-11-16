// Package link remembers which Teral app a directory deploys to, like
// `railway link` or `vercel link`. The link lives in .teral/link.json at the
// linked directory and applies to every directory below it. It is local
// state: the CLI adds .teral/ to .gitignore when it creates it.
package link

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	dirName  = ".teral"
	fileName = "link.json"
)

// Link points a directory at a project, app, and environment.
type Link struct {
	Workspace   string `json:"workspace"`
	Project     string `json:"project"`
	App         string `json:"app"`
	Environment string `json:"environment,omitempty"`

	// Dir is the directory holding .teral/, filled in by Find.
	Dir string `json:"-"`
}

// Find walks up from dir to the nearest link. It returns nil, nil when no
// directory up to the filesystem root is linked.
func Find(dir string) (*Link, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	for {
		path := filepath.Join(dir, dirName, fileName)
		raw, err := os.ReadFile(path)
		if err == nil {
			var l Link
			if err := json.Unmarshal(raw, &l); err != nil {
				return nil, fmt.Errorf("%s is not valid JSON: %w", path, err)
			}
			l.Dir = dir
			return &l, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, nil
		}
		dir = parent
	}
}

// Write links dir. It returns true when it added .teral/ to an existing
// .gitignore or created one in a git checkout.
func Write(dir string, l Link) (gitignored bool, err error) {
	if err := os.MkdirAll(filepath.Join(dir, dirName), 0o755); err != nil {
		return false, err
	}
	raw, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return false, err
	}
	if err := os.WriteFile(filepath.Join(dir, dirName, fileName), append(raw, '\n'), 0o644); err != nil {
		return false, err
	}
	readme := "This directory is linked to a Teral app by `teral link`.\nIt is local to your machine; don't commit it.\n"
	_ = os.WriteFile(filepath.Join(dir, dirName, "README.txt"), []byte(readme), 0o644)
	return ensureGitignored(dir)
}

// Remove unlinks dir.
func Remove(dir string) error {
	err := os.RemoveAll(filepath.Join(dir, dirName))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// ensureGitignored adds ".teral/" to the .gitignore of dir when dir is in a
// git checkout and the entry is missing.
func ensureGitignored(dir string) (bool, error) {
	if !inGitCheckout(dir) {
		return false, nil
	}
	path := filepath.Join(dir, ".gitignore")
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		switch strings.TrimSpace(line) {
		case ".teral", ".teral/", "/.teral", "/.teral/":
			return false, nil
		}
	}
	content := string(raw)
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	content += "\n# Teral CLI link (local)\n.teral/\n"
	return true, os.WriteFile(path, []byte(content), 0o644)
}

func inGitCheckout(dir string) bool {
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}
