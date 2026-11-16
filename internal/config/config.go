// Package config stores the CLI's settings and credentials.
//
// Settings live in config.toml in the config directory: $TERAL_CONFIG_DIR, or
// $XDG_CONFIG_HOME/teral, or ~/.config/teral. Session tokens are kept in the
// operating system's keychain; where none is available (headless Linux, CI
// containers) they fall back to credentials.toml next to config.toml,
// readable only by the user.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

const (
	// DefaultAPIURL is the Teral API.
	DefaultAPIURL = "https://api.teral.dev"
	// DefaultDashboardURL is the Teral dashboard.
	DefaultDashboardURL = "https://app.teral.dev"
)

// Account is a workspace the user is signed in to with `teral login`.
type Account struct {
	WorkspaceID   string `toml:"workspace_id"`
	WorkspaceName string `toml:"workspace_name"`
	SessionID     string `toml:"session_id"`
	UserID        string `toml:"user_id"`
	UserName      string `toml:"user_name"`
	Email         string `toml:"email,omitempty"`
	ExpiresAt     int64  `toml:"expires_at"`
	// Storage is "keyring" or "file": where the token is kept.
	Storage string `toml:"storage"`
}

// Config is the content of config.toml.
type Config struct {
	// APIURL overrides the API endpoint, for self-hosted or staging setups.
	APIURL string `toml:"api_url,omitempty"`
	// DashboardURL overrides the dashboard, used for links.
	DashboardURL string `toml:"dashboard_url,omitempty"`
	// ActiveWorkspace is the slug of the workspace commands use by default.
	ActiveWorkspace string `toml:"active_workspace,omitempty"`
	// Workspaces holds one account per signed-in workspace, keyed by slug.
	Workspaces map[string]Account `toml:"workspaces,omitempty"`
	// UpdateCheck disables the daily new-version notice when false.
	UpdateCheck *bool `toml:"update_check,omitempty"`

	dir string
}

// Dir returns the configuration directory.
func Dir() (string, error) {
	if dir := os.Getenv("TERAL_CONFIG_DIR"); dir != "" {
		return dir, nil
	}
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "teral"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home directory: %w", err)
	}
	return filepath.Join(home, ".config", "teral"), nil
}

// Load reads config.toml, returning an empty config when it does not exist.
func Load() (*Config, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	cfg := &Config{dir: dir, Workspaces: map[string]Account{}}
	path := filepath.Join(dir, "config.toml")
	if _, err := toml.DecodeFile(path, cfg); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if cfg.Workspaces == nil {
		cfg.Workspaces = map[string]Account{}
	}
	return cfg, nil
}

// Path returns the location of config.toml.
func (c *Config) Path() string { return filepath.Join(c.dir, "config.toml") }

// Save writes config.toml atomically with user-only permissions.
func (c *Config) Save() error {
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("# Teral CLI settings. Tokens are not stored here.\n")
	if err := toml.NewEncoder(&b).Encode(c); err != nil {
		return err
	}
	return writeFileAtomic(c.Path(), []byte(b.String()), 0o600)
}

// APIEndpoint returns the API base URL: $TERAL_API_URL, then api_url, then
// the default.
func (c *Config) APIEndpoint() string {
	if v := os.Getenv("TERAL_API_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	if c.APIURL != "" {
		return strings.TrimRight(c.APIURL, "/")
	}
	return DefaultAPIURL
}

// DashboardEndpoint returns the dashboard base URL.
func (c *Config) DashboardEndpoint() string {
	if v := os.Getenv("TERAL_DASHBOARD_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	if c.DashboardURL != "" {
		return strings.TrimRight(c.DashboardURL, "/")
	}
	return DefaultDashboardURL
}

// WorkspaceSlugs returns the signed-in workspaces, sorted.
func (c *Config) WorkspaceSlugs() []string {
	slugs := make([]string, 0, len(c.Workspaces))
	for slug := range c.Workspaces {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)
	return slugs
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
