# Teral CLI

Deploy and manage apps on [Teral](https://teral.dev) from your terminal.

```sh
curl -fsSL https://teral.dev/install.sh | sh   # or: brew install teraldev/tap/teral
teral login                                    # sign in with your browser
teral init                                     # create a project and app, link this directory
teral up                                       # deploy this directory
```

## Install

| Method | Command |
| --- | --- |
| Script (macOS, Linux) | `curl -fsSL https://teral.dev/install.sh \| sh` |
| Homebrew | `brew install teraldev/tap/teral` |
| Scoop (Windows) | `scoop bucket add teral https://github.com/teraldev/scoop-bucket && scoop install teral` |
| Go | `go install github.com/teraldev/cli/cmd/teral@latest` |
| Binaries | [GitHub releases](https://github.com/teraldev/cli/releases) |

Shell completion: `teral completion bash|zsh|fish|powershell --help`.

## Signing in

`teral login` uses a device login, like `gh auth login`: it shows a one-time
code and opens the dashboard, where you check the code and approve access for
the workspace you're in. The CLI then holds a session that acts as you, with
your role's permissions, in that workspace.

- Sessions last 90 days from their last use, and are listed and revocable in
  the dashboard (Settings → CLI Sessions) and with `teral sessions`.
- Tokens are stored in the system keychain; where there is none, in
  `~/.config/teral/credentials.toml` with user-only permissions.
- Run `teral login` again to add more workspaces and `teral workspace switch`
  to move between them.
- Over SSH or in a container, `teral login --no-browser` prints the link to
  open on any device.

### CI and automation

Don't log in from CI. Create an API key and set it as `TERAL_API_KEY`; every
command then uses it:

```sh
teral keys create github-actions --preset deploy   # or in the dashboard
```

```yaml
# .github/workflows/deploy.yml
- uses: actions/checkout@v4
- uses: teraldev/setup-teral@v1
- run: teral up --project acme --app api --env production
  env:
    TERAL_API_KEY: ${{ secrets.TERAL_API_KEY }}
```

`teral up` and `teral deploy` wait for the deployment and exit non-zero when it
fails, so the job fails with it. Prompts never appear in CI: a missing value
fails with the flag to pass.

## Commands

| | |
| --- | --- |
| `teral login` / `logout` / `whoami` | Sign in, sign out (revokes the session), show the current identity |
| `teral workspace list` / `switch` | Move between signed-in workspaces |
| `teral init` | Create a project and app for this directory and link it |
| `teral link` / `unlink` / `status` | Remember which app this directory deploys to |
| `teral up [dir]` | Upload a directory and deploy it, streaming the build |
| `teral deploy` | Deploy from git (`--branch`, `--commit`), an image (`--image`), or an existing deployment (`--from`) |
| `teral deployments list` / `get` / `promote` / `rollback` / `stop` / `start` | Operate deployments |
| `teral logs [-f] [--build]` | Runtime or build logs, with `--since`, `--level`, `--grep` |
| `teral env list` / `get` / `set` / `unset` / `pull` | Environment variables (`--secret` for write-only) |
| `teral run -- <cmd>` | Run a local command with the app's variables |
| `teral open` | Open the linked app in the dashboard |
| `teral projects` / `apps` / `domains` / `keys` / `sessions` | Manage resources |
| `teral config init` / `validate` | Create and check `teral.toml` |

Every command takes `--project/-p`, `--app/-a`, `--env/-e`, `--workspace/-w`,
`--output json`, `--yes`, and `--quiet`. Run `teral <command> --help` for
details.

### What `teral up` uploads

In a git checkout: the files git tracks plus untracked files that aren't
ignored. Outside git: everything not matched by a `.gitignore`. In both cases
`.teralignore` (gitignore syntax) and `.dockerignore` exclude more, and `.git`
and `.teral` are never uploaded. The directory is the build context; a
`teral.toml` in it applies to the deployment. Uploads are limited to 500 MB.

### Choosing the app

Commands find the project, app, and environment in this order: flags,
`TERAL_PROJECT` / `TERAL_APP` / `TERAL_ENVIRONMENT`, the directory's link
(`.teral/link.json`, found in this or any parent directory), then a prompt.
The environment defaults to `production`.

## Configuration

| Variable | Purpose |
| --- | --- |
| `TERAL_API_KEY` | API key for CI; takes precedence over `teral login` |
| `TERAL_WORKSPACE` | Workspace slug to use |
| `TERAL_OUTPUT=json` | JSON output for every command |
| `TERAL_CONFIG_DIR` | Settings directory (default `~/.config/teral`) |
| `TERAL_API_URL` / `TERAL_DASHBOARD_URL` | Point at another Teral installation |
| `TERAL_NO_KEYRING=1` | Store tokens in the credentials file instead of the keychain |
| `NO_COLOR` | Disable colors |

## Development

```sh
go test ./...
go run ./cmd/teral --help
```

The API client in `internal/api` is generated from the platform's OpenAPI
spec. To update it, copy `svc/api/openapi/openapi-generated.yaml` and
`overlay.yaml` from the platform repo into `internal/api/` (as `openapi.yaml`)
and run `go generate ./internal/api`. The `teral.toml` schema in
`internal/schema` comes from `pkg/teralconfig/schema.json`.

Releases: push a `v*` tag; GoReleaser publishes binaries, the Homebrew tap,
and the Scoop bucket.

## License

Apache License 2.0. See [LICENSE](LICENSE).
