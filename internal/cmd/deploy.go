package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/teraldev/cli/internal/api"
	"github.com/teraldev/cli/internal/archive"
	"github.com/teraldev/cli/internal/client"
	"github.com/teraldev/cli/internal/gitinfo"
	"github.com/teraldev/cli/internal/ui"
)

// defaultMaxUpload mirrors the API's default; the API's answer is authoritative.
const defaultMaxUpload = 500 << 20

func newUpCmd(a *App) *cobra.Command {
	var detach bool
	cmd := &cobra.Command{
		Use:   "up [directory]",
		Short: "Deploy a local directory",
		Long: `Upload a directory and deploy it, streaming the build until the deployment is
live. Like a git deployment, but from your working tree: uncommitted changes
are included.

Which files are uploaded: in a git checkout, what git tracks plus untracked
files that aren't ignored; otherwise everything not matched by a .gitignore.
A .teralignore file (gitignore syntax) excludes more. .git is never uploaded.

The directory is the build context. Without an argument it is the app's root
directory (from its settings) inside the current git checkout, else the
current directory. A teral.toml in it applies to the deployment.`,
		Example: "  teral up\n  teral up apps/api --env preview\n  teral up --detach",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, _, err := a.Client()
			if err != nil {
				return err
			}
			t, err := a.ResolveTarget(ctx, c, NeedEnvironment)
			if err != nil {
				return err
			}
			env, err := getEnvironment(ctx, c, t)
			if err != nil {
				return err
			}

			dir, err := a.buildContext(args, env)
			if err != nil {
				return err
			}
			a.IO.Step("Deploying %s to %s", displayPath(dir), a.IO.ErrTheme.Bold.Render(t.String()))

			git, inGit := gitinfo.Read(dir)
			archivePath, stats, err := a.pack(dir, inGit)
			if err != nil {
				return err
			}
			defer func() { _ = os.Remove(archivePath) }()

			uploadID, err := a.upload(ctx, c, t, archivePath, stats)
			if err != nil {
				return err
			}

			source := &api.DeploymentSourceUpload{UploadId: uploadID}
			if raw, err := os.ReadFile(filepath.Join(dir, "teral.toml")); err == nil {
				s := string(raw)
				source.ConfigFile = &s
				a.IO.Info("Using teral.toml")
			}
			if inGit {
				source.Git = &struct {
					Branch        *string `json:"branch,omitempty"`
					CommitMessage *string `json:"commitMessage,omitempty"`
					CommitSha     *string `json:"commitSha,omitempty"`
					Dirty         *bool   `json:"dirty,omitempty"`
				}{
					Branch:        nonEmpty(git.Branch),
					CommitSha:     nonEmpty(git.SHA),
					CommitMessage: nonEmpty(truncate(git.Message, 1024)),
					Dirty:         &git.Dirty,
				}
			}

			id, err := createDeployment(ctx, c, api.V1DeploymentsCreateDeploymentRequestBody{
				Project: t.Project, App: t.App, Environment: t.Environment, Upload: source,
			})
			if err != nil {
				return err
			}
			return a.afterCreate(ctx, c, id, detach)
		},
	}
	cmd.Flags().BoolVarP(&detach, "detach", "d", false, "Don't wait for the deployment to finish")
	return cmd
}

// buildContext picks the directory `teral up` uploads.
func (a *App) buildContext(args []string, env api.Environment) (string, error) {
	if len(args) == 1 {
		dir, err := filepath.Abs(args[0])
		if err != nil {
			return "", err
		}
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			return "", &UserError{Message: fmt.Sprintf("%s is not a directory.", args[0])}
		}
		return dir, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	root := ""
	if env.Build != nil {
		root = strings.Trim(env.Build.RootDirectory, "/")
	}
	if root == "" || root == "." {
		if l := a.Link(); l != nil {
			return l.Dir, nil
		}
		return cwd, nil
	}
	info, ok := gitinfo.Read(cwd)
	if !ok {
		return cwd, nil
	}
	dir := filepath.Join(info.Root, filepath.FromSlash(root))
	if _, err := os.Stat(dir); err != nil {
		return "", &UserError{
			Message: fmt.Sprintf("The app's root directory %q doesn't exist in this checkout.", root),
			Hint:    "Pass the directory to deploy: `teral up <directory>`.",
		}
	}
	return dir, nil
}

func (a *App) pack(dir string, inGit bool) (string, archive.Stats, error) {
	f, err := os.CreateTemp("", "teral-up-*.tar.gz")
	if err != nil {
		return "", archive.Stats{}, err
	}
	spin := a.IO.Spin("Packing files…")
	stats, err := archive.Write(f, dir, archive.Options{
		InGit:    inGit,
		MaxBytes: 4 * defaultMaxUpload,
		Progress: func(s archive.Stats) {
			if s.Files%200 == 0 {
				spin.Update(fmt.Sprintf("Packing files… %d (%s)", s.Files, ui.Bytes(s.Bytes)))
			}
		},
	})
	spin.Stop()
	closeErr := f.Close()
	if err != nil {
		_ = os.Remove(f.Name())
		if errors.Is(err, archive.ErrTooLarge) {
			return "", stats, &UserError{
				Message: "This directory is too large to upload.",
				Hint:    "Exclude build output and dependencies with a .gitignore or .teralignore file.",
			}
		}
		return "", stats, err
	}
	if closeErr != nil {
		return "", stats, closeErr
	}
	if stats.Files == 0 {
		_ = os.Remove(f.Name())
		return "", stats, &UserError{Message: "There are no files to upload in " + displayPath(dir) + "."}
	}
	return f.Name(), stats, nil
}

func (a *App) upload(ctx context.Context, c *client.Client, t Target, path string, stats archive.Stats) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	size := info.Size()
	if size > defaultMaxUpload {
		return "", &UserError{
			Message: fmt.Sprintf("The archive is %s; uploads are limited to %s.", ui.Bytes(size), ui.Bytes(defaultMaxUpload)),
			Hint:    "Exclude build output and dependencies with a .gitignore or .teralignore file.",
		}
	}

	res, err := client.Call[api.V1DeploymentsCreateUploadResponseBody](ctx, c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
		return cl.DeploymentsCreateUpload(ctx, api.V1DeploymentsCreateUploadRequestBody{
			Project: t.Project, App: t.App, Environment: t.Environment, SizeBytes: size,
		})
	})
	if err != nil {
		return "", err
	}
	up := res.Data
	if up.MaxSizeBytes > 0 && size > up.MaxSizeBytes {
		return "", &UserError{Message: fmt.Sprintf("The archive is %s; this installation accepts at most %s.", ui.Bytes(size), ui.Bytes(up.MaxSizeBytes))}
	}

	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	label := fmt.Sprintf("Uploading %d files (%s)", stats.Files, ui.Bytes(size))
	spin := a.IO.Spin(label + "…")
	body := &progressReader{r: f, total: size, onProgress: func(done int64) {
		spin.Update(fmt.Sprintf("%s… %d%%", label, done*100/max(size, 1)))
	}}
	req, err := http.NewRequestWithContext(ctx, string(up.Method), up.UploadUrl, body)
	if err != nil {
		spin.Stop()
		return "", err
	}
	req.ContentLength = size
	for k, v := range up.Headers {
		req.Header.Set(k, v)
	}
	httpRes, err := (&http.Client{Timeout: 30 * time.Minute}).Do(req)
	spin.Stop()
	if err != nil {
		return "", fmt.Errorf("upload the source archive: %w", err)
	}
	defer func() { _ = httpRes.Body.Close() }()
	if httpRes.StatusCode < 200 || httpRes.StatusCode > 299 {
		detail, _ := io.ReadAll(io.LimitReader(httpRes.Body, 2048))
		return "", fmt.Errorf("upload the source archive: storage answered HTTP %d: %s", httpRes.StatusCode, strings.TrimSpace(string(detail)))
	}
	a.IO.Success("Uploaded %d files (%s)", stats.Files, ui.Bytes(size))
	return up.UploadId, nil
}

type progressReader struct {
	r          io.Reader
	total      int64
	done       int64
	lastReport time.Time
	onProgress func(int64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.done += int64(n)
	if time.Since(p.lastReport) > 100*time.Millisecond || err == io.EOF {
		p.lastReport = time.Now()
		p.onProgress(p.done)
	}
	return n, err
}

func newDeployCmd(a *App) *cobra.Command {
	var image, branch, commit, from string
	var detach bool
	cmd := &cobra.Command{
		Use:   "deploy",
		Short: "Deploy from git or a container image",
		Long: `Create a deployment from the app's GitHub repository, a container image, or an
existing deployment, and wait until it is live.

Without flags, the app's declared source is deployed: the head of its default
branch for git apps, its configured image for image apps. To deploy local
files instead, use "teral up".`,
		Example: `  teral deploy
  teral deploy --branch feature/cart
  teral deploy --commit 9f2c1a7
  teral deploy --image ghcr.io/acme/api:v1.4.0
  teral deploy --from d_1234abcd`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, _, err := a.Client()
			if err != nil {
				return err
			}
			t, err := a.ResolveTarget(ctx, c, NeedEnvironment)
			if err != nil {
				return err
			}
			body := api.V1DeploymentsCreateDeploymentRequestBody{Project: t.Project, App: t.App, Environment: t.Environment}
			sources := 0
			for _, v := range []string{image, from, firstNonEmpty(branch, commit)} {
				if v != "" {
					sources++
				}
			}
			if sources > 1 {
				return &UserError{Message: "Pass only one source: --image, --from, or --branch/--commit."}
			}
			switch {
			case image != "":
				body.Image = &api.DeploymentSourceImage{DockerImage: image}
			case from != "":
				body.Deployment = &api.DeploymentSourceDeployment{DeploymentId: from}
			case branch != "" || commit != "":
				body.Git = &api.DeploymentSourceGit{Branch: nonEmpty(branch), CommitSha: nonEmpty(commit)}
			default:
				app, err := getApp(ctx, c, t.Project, t.App)
				if err != nil {
					return err
				}
				switch {
				case app.Oci != nil && app.Oci.Image != "":
					body.Image = &api.DeploymentSourceImage{DockerImage: app.Oci.Image}
				case app.Git != nil && app.Git.Repository != "":
					body.Git = &api.DeploymentSourceGit{}
				default:
					return &UserError{
						Message: fmt.Sprintf("App %s has no repository or image to deploy from.", t.App),
						Hint:    "Deploy local files with `teral up`, or pass --image.",
					}
				}
			}
			a.IO.Step("Deploying %s", a.IO.ErrTheme.Bold.Render(t.String()))
			id, err := createDeployment(ctx, c, body)
			if err != nil {
				return err
			}
			return a.afterCreate(ctx, c, id, detach)
		},
	}
	f := cmd.Flags()
	f.StringVar(&image, "image", "", "Container image to deploy")
	f.StringVar(&branch, "branch", "", "Git branch to build (its latest commit)")
	f.StringVar(&commit, "commit", "", "Git commit to build")
	f.StringVar(&from, "from", "", "Rebuild an existing deployment's source")
	f.BoolVarP(&detach, "detach", "d", false, "Don't wait for the deployment to finish")
	return cmd
}

func createDeployment(ctx context.Context, c *client.Client, body api.V1DeploymentsCreateDeploymentRequestBody) (string, error) {
	res, err := client.Call[api.V1DeploymentsCreateDeploymentResponseBody](ctx, c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
		return cl.DeploymentsCreateDeployment(ctx, body)
	})
	if err != nil {
		return "", err
	}
	return res.Data.DeploymentId, nil
}

// afterCreate either returns right away (--detach) or follows the deployment.
func (a *App) afterCreate(ctx context.Context, c *client.Client, id string, detach bool) error {
	if detach {
		if a.IO.JSON {
			return a.IO.PrintJSON(map[string]string{"deploymentId": id})
		}
		a.IO.Success("Created deployment %s", id)
		a.IO.Hint("Follow it with `teral deployments get %s --wait` or `teral logs --build -d %s -f`.", id, id)
		return nil
	}
	a.IO.Info("Deployment %s", a.IO.ErrTheme.Muted.Render(id))
	dep, err := a.follow(ctx, c, id)
	if err != nil {
		return err
	}
	if a.IO.JSON {
		return a.IO.PrintJSON(dep)
	}
	return nil
}

// follow streams a deployment's build output and status until it settles.
// Interrupting stops watching, never the deployment.
func (a *App) follow(ctx context.Context, c *client.Client, id string) (*api.Deployment, error) {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()

	started := time.Now()
	spin := a.IO.Spin("Queued")
	defer spin.Stop()

	var (
		lastStatus string
		nextSeq    int64
		seenSteps  = map[string]bool{}
		buildDone  bool
	)
	printBuild := func() {
		for i := 0; i < 20; i++ { // drain at most 20 pages per tick
			res, err := client.Call[api.V1LogsGetBuildLogsResponseBody](ctx, c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
				afterSeq := nextSeq
				return cl.LogsGetBuildLogs(ctx, api.V1LogsGetBuildLogsRequestBody{DeploymentId: id, AfterSeq: &afterSeq})
			})
			if err != nil {
				return
			}
			d := res.Data
			stepNames := map[string]string{}
			for _, s := range d.Steps {
				stepNames[s.Id] = s.Name
			}
			lines := d.Lines
			spin.Interrupt(func() {
				for _, s := range d.Steps {
					if seenSteps[s.Id] {
						continue
					}
					seenSteps[s.Id] = true
					suffix := ""
					if s.Cached {
						suffix = a.IO.ErrTheme.Muted.Render(" cached")
					}
					fmt.Fprintf(a.IO.Err, "  %s %s%s\n", a.IO.ErrTheme.Accent.Render("▸"), s.Name, suffix)
				}
				for _, l := range lines {
					style := a.IO.ErrTheme.Muted
					if l.Stderr {
						style = a.IO.ErrTheme.Warning
					}
					for _, part := range strings.Split(strings.TrimRight(l.Message, "\n"), "\n") {
						fmt.Fprintf(a.IO.Err, "    %s\n", style.Render(part))
					}
				}
			})
			nextSeq = d.NextSeq
			if !d.HasMore {
				return
			}
		}
	}

	for {
		dep, err := getDeployment(ctx, c, id)
		if err != nil {
			if ctx.Err() != nil {
				return nil, a.detachedHint(id)
			}
			return nil, err
		}
		status := string(dep.Status)

		if status == "building" || (lastStatus == "building" && !buildDone) {
			printBuild()
			if status != "building" {
				buildDone = true
			}
		}
		if status != lastStatus {
			lastStatus = status
			spin.Interrupt(func() {
				if status != "pending" && status != "building" {
					fmt.Fprintf(a.IO.Err, "%s %s\n", a.IO.ErrTheme.Accent.Render("→"), statusLabel(status))
				}
			})
		}
		spin.Update(fmt.Sprintf("%s %s", statusLabel(status), a.IO.ErrTheme.Muted.Render(ui.Duration(time.Since(started)))))

		switch status {
		case "ready":
			spin.Stop()
			a.IO.Success("Deployed in %s", ui.Duration(time.Since(started)))
			if dep.Domains != nil {
				for _, d := range *dep.Domains {
					fmt.Fprintf(a.IO.Err, "  %s\n", a.IO.Link("https://"+d))
				}
			}
			return dep, nil
		case "failed":
			spin.Stop()
			msg := "The deployment failed."
			hint := fmt.Sprintf("See the build output with `teral logs --build -d %s`.", id)
			if dep.Error != nil && dep.Error.Message != "" {
				msg = "The deployment failed: " + dep.Error.Message
				if dep.Error.Step != "" && dep.Error.Step != "build" {
					hint = fmt.Sprintf("See the runtime logs with `teral logs -d %s`.", id)
				}
			}
			return dep, &UserError{Message: msg, Hint: hint}
		case "cancelled", "superseded", "skipped", "stopped":
			spin.Stop()
			return dep, &UserError{Message: fmt.Sprintf("The deployment was %s.", status)}
		case "awaiting_approval":
			spin.Stop()
			return dep, &UserError{Message: "The deployment is waiting for approval in the dashboard.", Hint: "Approve it in the dashboard; it continues afterwards."}
		}

		select {
		case <-ctx.Done():
			spin.Stop()
			return nil, a.detachedHint(id)
		case <-time.After(1500 * time.Millisecond):
		}
	}
}

func (a *App) detachedHint(id string) error {
	return &UserError{
		Message: "Stopped watching. The deployment continues.",
		Hint:    fmt.Sprintf("Check on it with `teral deployments get %s`.", id),
	}
}

func statusLabel(status string) string {
	switch status {
	case "pending":
		return "Queued"
	case "starting":
		return "Starting"
	case "building":
		return "Building"
	case "deploying":
		return "Deploying"
	case "network":
		return "Configuring network"
	case "finalizing":
		return "Finalizing"
	case "ready":
		return "Ready"
	default:
		return strings.ReplaceAll(status, "_", " ")
	}
}

func getDeployment(ctx context.Context, c *client.Client, id string) (*api.Deployment, error) {
	res, err := client.Call[api.V1DeploymentsGetDeploymentResponseBody](ctx, c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
		return cl.DeploymentsGetDeployment(ctx, api.V1DeploymentsGetDeploymentRequestBody{DeploymentId: id})
	})
	if err != nil {
		return nil, err
	}
	return &res.Data, nil
}

// latestDeployment returns the newest deployment of the target, or with
// liveOnly the one currently serving traffic when there is one.
func latestDeployment(ctx context.Context, c *client.Client, t Target, liveOnly bool) (*api.Deployment, error) {
	res, err := client.Call[api.V1DeploymentsListDeploymentsResponseBody](ctx, c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
		return cl.DeploymentsListDeployments(ctx, api.V1DeploymentsListDeploymentsRequestBody{
			Project: &t.Project, App: &t.App, Environment: nonEmpty(t.Environment), Limit: ptr(20),
		})
	})
	if err != nil {
		return nil, err
	}
	if len(res.Data) == 0 {
		return nil, &UserError{Message: fmt.Sprintf("%s has no deployments yet.", t), Hint: "Deploy with `teral up` or `teral deploy`."}
	}
	if liveOnly {
		for i := range res.Data {
			if res.Data[i].IsCurrent || res.Data[i].Status == api.DeploymentStatusReady {
				return &res.Data[i], nil
			}
		}
	}
	return &res.Data[0], nil
}

func deploymentSource(d api.Deployment) string {
	switch {
	case d.Upload != nil:
		s := "teral up"
		if d.Git != nil && d.Git.CommitSha != "" {
			s += " @ " + ui.ShortSHA(d.Git.CommitSha)
			if d.Git.Branch != nil && *d.Git.Branch != "" {
				s += " (" + *d.Git.Branch + ")"
			}
		}
		if d.Upload.Dirty != nil && *d.Upload.Dirty {
			s += " + local changes"
		}
		return s
	case d.Git != nil:
		s := ui.ShortSHA(d.Git.CommitSha)
		if d.Git.Branch != nil && *d.Git.Branch != "" {
			s = *d.Git.Branch + " @ " + s
		}
		return s
	case d.Docker != nil:
		return d.Docker.Image
	default:
		return "—"
	}
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
