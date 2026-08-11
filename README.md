# promptd

`promptd` is a small, self-hosted daemon for running prompts and AI agent commands on a schedule without per-job cron entries or systemd timers.

The project is Linux-first. Its scheduler and execution engine are designed to remain independent from systemd; systemd will supervise one long-running `promptd` process.

> [!IMPORTANT]
> `promptd` is in early development. The daemon now schedules jobs, executes command, Codex, and Claude Code runners, stores run history in SQLite, exposes a local control API, and supports planned user/system systemd installation. Release publishing is the next milestone.

## Why promptd?

Running an unattended coding agent usually requires a script, a cron entry or timer, environment setup, locking, logs, and failure handling. `promptd` aims to make that one declarative job:

```yaml
version: 1

jobs:
  daily-report:
    schedule:
      cron: "0 9 * * 1-5"
      timezone: Asia/Yekaterinburg

    agent:
      type: codex
      prompt_file: prompts/daily-report.md

    run:
      working_directory: /srv/project
      timeout: 20m
      overlap: skip
      misfire: run_once
```

## Design

```text
systemd / launchd / container runtime
                  |
                  v
             promptd daemon
             |- scheduler
             |- run history
             |- agent runners
             `- local control socket
```

- The daemon owns scheduling, locking, recovery, and run history.
- systemd is a supervisor, not the per-job scheduler.
- Jobs are executed without an implicit shell.
- User and system installation modes share the same core.
- The initial security model is trusted, single-owner operation. Multi-tenant isolation is out of scope for the first release.

## Current commands

```text
promptd version
promptd validate --config path/to/config.yaml
promptd daemon --config path/to/config.yaml --state path/to/promptd.db --log-dir path/to/logs --socket path/to/promptd.sock
promptd status --state path/to/promptd.db
```

The daemon reloads its configuration on `SIGHUP`. An invalid reload is rejected while the last valid configuration remains active. Its default per-user state database is `~/.local/state/promptd/promptd.db` on Linux; logs default to the adjacent `logs/` directory and the control socket to the adjacent `promptd.sock`.

`promptd status` asks the running daemon for a consistent status view over its owner-only Unix socket. If the daemon is unavailable, it falls back to opening the state database read-only. It shows every registered job, including jobs disabled after removal from the current configuration, plus the next scheduled occurrence and latest run result. Use `--format json` for machine-readable output or `--offline` to skip the control API explicitly.

The versioned local control API currently exposes `GET /v1/status` over the Unix socket. The daemon refuses to replace a regular file or an active socket and creates its socket with mode `0600`.

Interval schedules stay anchored to their persisted `next_run`, so daemon restarts do not introduce drift. After downtime, `misfire: skip` advances to the next future occurrence, while `misfire: run_once` coalesces missed occurrences into one run. `overlap: skip` records overlapping occurrences as skipped; `overlap: queue_one` retains at most one pending run.

## Build from source

Requirements:

- Go 1.25 or newer.

```bash
git clone https://github.com/frux/promptd.git
cd promptd
make check
./bin/promptd validate --config examples/config.yaml
```

## Configuration

The default per-user configuration path follows the operating system's user config directory. On Linux it is normally:

```text
~/.config/promptd/config.yaml
```

The decoder rejects unknown fields. Relative paths are resolved from the directory containing the configuration file.

Each job selects exactly one schedule:

```yaml
schedule:
  every: 15m
```

or:

```yaml
schedule:
  cron: "0 9 * * *"
  timezone: Europe/Amsterdam
```

Supported execution policies in the initial schema:

| Field | Values | Default |
| --- | --- | --- |
| `overlap` | `skip`, `queue_one` | `skip` |
| `misfire` | `skip`, `run_once` | `skip` |
| `timeout` | Go duration such as `30s`, `15m`, `2h` | `30m` |

Codex jobs use the official [`codex exec` non-interactive mode](https://developers.openai.com/codex/noninteractive). Prompts are passed over stdin, sessions are ephemeral, approvals are disabled for unattended operation, and the sandbox defaults to `read-only`. Jobs that need to modify their workspace must opt in explicitly:

```yaml
agent:
  type: codex
  prompt: Fix the failing tests and verify the result.
  sandbox: workspace-write
```

The optional `model` field overrides the Codex CLI model for that job.

Claude Code jobs use Anthropic's official [`claude --print` automation mode](https://code.claude.com/docs/en/headless). They default to `dontAsk`, disable session persistence, and can grant only the tools a job needs:

```yaml
agent:
  type: claude
  prompt_file: prompts/fix-tests.md
  permission_mode: acceptEdits
  allowed_tools:
    - Read
    - Edit
    - Bash(go test *)
```

Supported unattended permission modes are `dontAsk`, `acceptEdits`, `auto`, `plan`, and the explicitly dangerous `bypassPermissions`. Set `bare: true` for reproducible API-key-based automation; bare mode intentionally does not use Claude's saved subscription login.

Environment files use a deliberately small dotenv-style subset: one `KEY=VALUE` per line, optional `export`, comments on their own lines, and single- or double-quoted values. Values are not expanded by a shell.

See [`examples/config.yaml`](examples/config.yaml) for a complete example.

## Setup and installation

`promptd setup` detects the best available supervision mode and prints every file and command it would use. It is a dry run unless `--apply` is passed:

```bash
promptd setup
promptd setup --apply
```

Auto mode selects:

- `user-systemd` when the current user manager is reachable. The generated unit lives under `${XDG_CONFIG_HOME:-~/.config}/systemd/user`; setup uses the numeric UID when enabling linger so it also works for virtual users without a stable NSS name.
- `system-systemd` when setup is running as root. Running scheduled agents as root is refused by default; provide a non-root numeric identity with `--service-uid`, `--service-gid`, and `--service-home`, or use the explicit `--allow-root` escape hatch.
- `portable` outside Linux or when no usable systemd manager is available. This creates the initial config but leaves process supervision to the user or container runtime.

The user unit is enabled under `default.target`; the system unit under `multi-user.target`. Both restart on failure, use an owner-only umask, preserve the installation-time `HOME` and `PATH`, and support `systemctl reload promptd` through `SIGHUP`. User lingering keeps the user manager alive after logout, as described by the official [`loginctl enable-linger` documentation](https://www.freedesktop.org/software/systemd/man/latest/loginctl.html).

The intended release installation experience is:

```bash
curl -fsSL https://raw.githubusercontent.com/frux/promptd/main/install.sh | sh
```

The POSIX shell bootstrap supports Linux amd64 and arm64, downloads a release archive over HTTPS, requires its SHA-256 sidecar, installs the binary atomically to `~/.local/bin`, prints the setup plan, and asks for confirmation through `/dev/tty`. Set `PROMPTD_SETUP=skip` for download-only installation or `PROMPTD_SETUP=apply` for non-interactive setup.

Release assets are not published yet, so the curl command will not work until the first GitHub release. Each `v*` tag is configured to run the race test suite, build reproducible static Linux amd64/arm64 archives, publish SHA-256 sidecars, and generate a GitHub build-provenance attestation. Once a release exists, verify an archive with:

```bash
gh attestation verify promptd_linux_amd64.tar.gz --repo frux/promptd
```

This follows GitHub's official [artifact attestation workflow](https://docs.github.com/en/actions/how-tos/secure-your-work/use-artifact-attestations/use-artifact-attestations). Maintainers can reproduce the archives locally with `make release VERSION=v0.1.0`; timestamps and embedded build metadata are derived from the source commit.

## Roadmap to v0.1

- [x] Go project and CLI skeleton
- [x] Strict YAML configuration validation
- [x] Daemon lifecycle and safe configuration reload
- [x] SQLite state and run history
- [x] Process-group supervisor and timeouts
- [x] Command and Codex runners
- [x] Claude Code runner
- [ ] Additional popular agent runners
- [x] Cron/interval scheduler and runner orchestration
- [x] Unix socket control API
- [x] `promptd status` overview for all registered jobs
- [x] `promptd setup` and checksum-verified shell bootstrap
- [x] User and system systemd integration
- [x] Reproducible Linux release archives, checksums, and provenance

## Development

```bash
make test
make vet
make build
```

Please keep platform-specific process and service-manager code behind small interfaces. The scheduler, configuration, and state packages must not depend on systemd.

## License

Licensed under the Apache License 2.0. See [`LICENSE`](LICENSE).
