// Package archive packs a build context for `teral up`: a gzip-compressed
// tar of a directory, without what the project ignores.
//
// Inside a git checkout the file list comes from git itself (tracked and
// untracked files minus .gitignore), so it matches what a git deployment
// would build. Outside git, .gitignore files are honored by pattern. Either
// way .teralignore (gitignore syntax) excludes more, .dockerignore applies
// with Docker's own matching rules (as `docker build` would), and .git and
// .teral are never uploaded.
package archive

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/moby/patternmatcher"
	"github.com/moby/patternmatcher/ignorefile"
	ignore "github.com/sabhiram/go-gitignore"

	"github.com/teraldev/cli/internal/gitinfo"
)

// Stats summarizes an archive.
type Stats struct {
	Files int
	// Bytes is the uncompressed size of the files.
	Bytes int64
}

// Options tunes [Write].
type Options struct {
	// InGit lists files through git. Set when dir is inside a checkout.
	InGit bool
	// MaxBytes aborts when the uncompressed content exceeds it (0 = no limit).
	MaxBytes int64
	// Progress is called after each file.
	Progress func(Stats)
}

// ErrTooLarge means the directory exceeds Options.MaxBytes.
var ErrTooLarge = errors.New("directory is too large to upload")

// Files lists the files Write would pack, relative to dir and sorted.
func Files(dir string, opts Options) ([]string, error) {
	var files []string
	var err error
	if opts.InGit {
		files, err = gitinfo.ListFiles(dir)
		if err != nil {
			return nil, fmt.Errorf("list files with git: %w", err)
		}
	} else {
		files, err = walk(dir)
		if err != nil {
			return nil, err
		}
	}

	extra, err := loadIgnore(filepath.Join(dir, ".teralignore"))
	if err != nil {
		return nil, err
	}
	docker, err := loadDockerignore(dir)
	if err != nil {
		return nil, err
	}

	kept := files[:0]
	for _, f := range files {
		f = filepath.ToSlash(f)
		if alwaysExcluded(f) || (extra != nil && extra.MatchesPath(f)) {
			continue
		}
		if docker != nil && !keepForDocker(f) {
			if excluded, err := docker.MatchesOrParentMatches(f); err == nil && excluded {
				continue
			}
		}
		// git lists tracked files that were deleted locally; skip them.
		info, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(f)))
		if err != nil || info.IsDir() {
			continue
		}
		kept = append(kept, f)
	}
	sort.Strings(kept)
	return kept, nil
}

// Write packs dir into w as tar.gz.
func Write(w io.Writer, dir string, opts Options) (Stats, error) {
	files, err := Files(dir, opts)
	if err != nil {
		return Stats{}, err
	}

	gz, err := gzip.NewWriterLevel(w, gzip.DefaultCompression)
	if err != nil {
		return Stats{}, err
	}
	tw := tar.NewWriter(gz)

	var stats Stats
	for _, name := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		info, err := os.Lstat(path)
		if err != nil {
			return stats, err
		}

		var linkTarget string
		if info.Mode()&fs.ModeSymlink != 0 {
			linkTarget, err = os.Readlink(path)
			if err != nil {
				return stats, err
			}
		} else if !info.Mode().IsRegular() {
			continue // sockets, devices, pipes
		}

		hdr, err := tar.FileInfoHeader(info, linkTarget)
		if err != nil {
			return stats, err
		}
		hdr.Name = name
		// Reproducible metadata: owner and group are meaningless in a build.
		hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
		hdr.Mode = int64(normalizeMode(info.Mode()))
		if err := tw.WriteHeader(hdr); err != nil {
			return stats, err
		}

		if info.Mode().IsRegular() {
			stats.Bytes += info.Size()
			if opts.MaxBytes > 0 && stats.Bytes > opts.MaxBytes {
				return stats, ErrTooLarge
			}
			f, err := os.Open(path)
			if err != nil {
				return stats, err
			}
			_, err = io.Copy(tw, f)
			_ = f.Close()
			if err != nil {
				return stats, fmt.Errorf("pack %s: %w", name, err)
			}
		}
		stats.Files++
		if opts.Progress != nil {
			opts.Progress(stats)
		}
	}

	if err := tw.Close(); err != nil {
		return stats, err
	}
	return stats, gz.Close()
}

// normalizeMode keeps only the executable bit, which matters for scripts in
// the build; other permission differences between machines would only make
// builds less reproducible.
func normalizeMode(m fs.FileMode) fs.FileMode {
	if m&fs.ModeSymlink != 0 {
		return 0o777
	}
	if m&0o111 != 0 {
		return 0o755
	}
	return 0o644
}

func alwaysExcluded(name string) bool {
	for _, dir := range []string{".git", ".teral"} {
		if name == dir || strings.HasPrefix(name, dir+"/") || strings.Contains(name, "/"+dir+"/") {
			return true
		}
	}
	return false
}

// walk lists files below dir outside git, honoring every .gitignore found
// on the way down.
func walk(root string) ([]string, error) {
	type scoped struct {
		base string
		m    *ignore.GitIgnore
	}
	var matchers []scoped
	var files []string

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			rel = ""
		}

		for _, m := range matchers {
			if !strings.HasPrefix(rel, m.base) {
				continue
			}
			sub := strings.TrimPrefix(rel, m.base)
			if sub != "" && (m.m.MatchesPath(sub) || (d.IsDir() && m.m.MatchesPath(sub+"/"))) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}

		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == ".teral" {
				return filepath.SkipDir
			}
			gi, err := loadIgnore(filepath.Join(path, ".gitignore"))
			if err != nil {
				return err
			}
			if gi != nil {
				base := rel
				if base != "" {
					base += "/"
				}
				matchers = append(matchers, scoped{base: base, m: gi})
			}
			return nil
		}
		files = append(files, rel)
		return nil
	})
	return files, err
}

func loadIgnore(path string) (*ignore.GitIgnore, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return ignore.CompileIgnoreLines(strings.Split(string(raw), "\n")...), nil
}

// keepForDocker lists files Docker always sends even when .dockerignore
// matches them, because the build needs them.
func keepForDocker(name string) bool {
	return name == "Dockerfile" || name == ".dockerignore" || name == "teral.toml"
}

func loadDockerignore(dir string) (*patternmatcher.PatternMatcher, error) {
	f, err := os.Open(filepath.Join(dir, ".dockerignore"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	patterns, err := ignorefile.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("read .dockerignore: %w", err)
	}
	if len(patterns) == 0 {
		return nil, nil
	}
	return patternmatcher.New(patterns)
}
