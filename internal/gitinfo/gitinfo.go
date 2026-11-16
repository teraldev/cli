// Package gitinfo reads git metadata of a local checkout for `teral up`, so a
// deployment from a laptop still shows the branch and commit it came from.
package gitinfo

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"time"
)

// Info describes the checkout a directory belongs to.
type Info struct {
	Root    string
	Branch  string
	SHA     string
	Message string
	// Dirty is true when tracked or untracked files differ from HEAD.
	Dirty bool
}

// Read returns git metadata for dir, or ok=false when dir is not in a git
// checkout or git is not installed.
func Read(dir string) (Info, bool) {
	root, err := git(dir, "rev-parse", "--show-toplevel")
	if err != nil || root == "" {
		return Info{}, false
	}
	info := Info{Root: root}
	info.SHA, _ = git(dir, "rev-parse", "HEAD")
	info.Branch, _ = git(dir, "symbolic-ref", "--quiet", "--short", "HEAD")
	info.Message, _ = git(dir, "log", "-1", "--format=%s")
	if status, err := git(dir, "status", "--porcelain", "--untracked-files=normal"); err == nil {
		info.Dirty = status != ""
	}
	return info, true
}

// ListFiles returns the files git would consider part of the project below
// dir: tracked and untracked files minus ignored ones, relative to dir.
func ListFiles(dir string) ([]string, error) {
	out, err := gitRaw(dir, "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--", ".")
	if err != nil {
		return nil, err
	}
	var files []string
	seen := map[string]bool{}
	for _, f := range bytes.Split(out, []byte{0}) {
		name := string(f)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		files = append(files, name)
	}
	return files, nil
}

func git(dir string, args ...string) (string, error) {
	out, err := gitRaw(dir, args...)
	return strings.TrimSpace(string(out)), err
}

func gitRaw(dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	return cmd.Output()
}
