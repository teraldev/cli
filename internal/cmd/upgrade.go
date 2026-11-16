package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/teraldev/cli/internal/version"
)

const releasesAPI = "https://api.github.com/repos/teraldev/cli/releases/latest"

func newUpgradeCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:   "upgrade",
		Short: "Check for a newer version and show how to install it",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			latest, err := latestRelease(cmd.Context())
			if err != nil {
				return fmt.Errorf("check for updates: %w", err)
			}
			current := strings.TrimPrefix(version.Version, "v")
			if a.IO.JSON {
				return a.IO.PrintJSON(map[string]any{"current": current, "latest": latest, "upToDate": latest == current})
			}
			if latest == current {
				a.IO.Success("teral %s is the latest version", current)
				return nil
			}
			a.IO.Info("teral %s is available (you have %s).", latest, current)
			a.IO.Hint("Update with: %s", upgradeCommand())
			return nil
		},
	}
}

func latestRelease(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releasesAPI, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", version.UserAgent())
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub answered HTTP %d", res.StatusCode)
	}
	var release struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(res.Body).Decode(&release); err != nil {
		return "", err
	}
	return strings.TrimPrefix(release.TagName, "v"), nil
}

// upgradeCommand guesses how this binary was installed.
func upgradeCommand() string {
	exe, _ := os.Executable()
	exe, _ = filepath.EvalSymlinks(exe)
	switch {
	case strings.Contains(exe, "/Cellar/") || strings.Contains(exe, "/homebrew/"):
		return "brew upgrade teral"
	case strings.Contains(exe, "node_modules"):
		return "npm install -g @teral/cli"
	case runtime.GOOS == "windows":
		return "scoop update teral"
	default:
		return "curl -fsSL https://teral.dev/install.sh | sh"
	}
}
