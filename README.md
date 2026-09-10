# promptd

`promptd` is a small, self-hosted scheduler for running prompts and AI agent commands on a recurring schedule. One daemon replaces per-job cron entries, systemd timers, wrapper scripts, locking, and ad hoc log handling.

`promptd` is Linux-first and supports Codex, Claude Code, Gemini CLI, and direct command execution. It stores schedules and run history locally in SQLite and can be supervised by systemd without making systemd part of the job format.

> [!IMPORTANT]
> `promptd` is an early-stage project. Its current security model assumes a trusted, single-owner installation; multi-tenant isolation is out of scope.

## Installation

The installer supports Linux on amd64 and arm64:

```bash
curl -fsSL https://raw.githubusercontent.com/frux/promptd/main/install.sh | sh
```

It downloads the latest release, verifies its SHA-256 checksum, installs `promptd` to `~/.local/bin`, detects the best supervision mode, prints the proposed setup, and asks before applying it. Ensure `~/.local/bin` is in `PATH` after installation.

The installer does not install or authenticate agent CLIs. Install every CLI used by your jobs (`codex`, `claude`, or `gemini`) and make it available in the service `PATH`.

For unattended installation:

```bash
curl -fsSL https://raw.githubusercontent.com/frux/promptd/main/install.sh \
  | PROMPTD_SETUP=apply PROMPTD_MODE=auto sh
```

Installer settings:

| Variable | Values | Default | Purpose |
| --- | --- | --- | --- |
| `PROMPTD_VERSION` | A release tag or `latest` | `latest` | Select the release to install. |
| `PROMPTD_INSTALL_DIR` | Directory path | `~/.local/bin` | Select the binary installation directory. |
| `PROMPTD_SETUP` | `prompt`, `apply`, `skip` | `prompt` | Confirm interactively, apply immediately, or install only. |
| `PROMPTD_MODE` | `auto`, `user-systemd`, `system-systemd`, `portable` | `auto` | Select the supervision mode. |

You can inspect or apply setup again at any time. Without `--apply`, setup is always a dry run:

```bash
promptd setup
promptd setup --apply
```

To pin both the installer and binary to a specific release:

```bash
curl -fsSL https://raw.githubusercontent.com/frux/promptd/v0.1.0/install.sh \
  | PROMPTD_VERSION=v0.1.0 sh
```

## Configuration reference

The default configuration path on Linux is:

```text
~/.config/promptd/config.yaml
```

Configuration is strict: unknown fields, unsupported values, and multiple YAML documents are rejected. Relative `prompt_file`, `working_directory`, and `environment_file` paths are resolved from the directory containing the configuration file.

Validate a configuration before reloading the daemon:

```bash
promptd validate --config ~/.config/promptd/config.yaml
```

### Complete example

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
      sandbox: read-only

    run:
      working_directory: /srv/project
      environment_file: secrets/agent.env
      timeout: 20m
      overlap: skip
      misfire: run_once

  health-check:
    schedule:
      every: 15m

    agent:
      type: command
      command: ["./scripts/health-check"]

    run:
      working_directory: /srv/project
```

See [`examples/config.yaml`](examples/config.yaml) for the example shipped with the repository.

### Top-level fields

| Field | Required | Value |
| --- | --- | --- |
| `version` | Yes | Configuration schema version. The current and only accepted value is `1`. |
| `jobs` | No | Mapping of job IDs to job definitions. An omitted or empty mapping registers no jobs. |

Job IDs must match `[a-z0-9][a-z0-9_-]{0,63}`. Each job contains `schedule`, `agent`, and optional `run` settings.

### `schedule`

Set exactly one of `cron` or `every`.

| Field | Required | Default | Value |
| --- | --- | --- | --- |
| `cron` | One schedule type | — | Standard five-field cron expression: minute, hour, day of month, month, day of week. |
| `every` | One schedule type | — | Positive Go duration such as `30s`, `15m`, or `2h`. |
| `timezone` | No | `Local` | IANA timezone used by cron, for example `Europe/Amsterdam`. |

Interval schedules stay anchored to the persisted next run, so daemon restarts do not introduce drift.

### `agent`

Every job uses one runner selected by `type`:

| `type` | Required fields | Optional fields | Safety default |
| --- | --- | --- | --- |
| `codex` | Exactly one of `prompt`, `prompt_file` | `model`, `sandbox` | `sandbox: read-only` |
| `claude` | Exactly one of `prompt`, `prompt_file` | `model`, `permission_mode`, `allowed_tools`, `bare` | `permission_mode: dontAsk` |
| `gemini` | Exactly one of `prompt`, `prompt_file` | `model`, `approval_mode`, `allowed_tools`, `sandbox` | `approval_mode: plan` |
| `command` | `command` | — | Direct execution without a shell |

Agent fields:

| Field | Used by | Value |
| --- | --- | --- |
| `type` | All | `codex`, `claude`, `gemini`, or `command`. |
| `prompt` | Codex, Claude, Gemini | Inline prompt text. Mutually exclusive with `prompt_file`. |
| `prompt_file` | Codex, Claude, Gemini | Path to a non-empty prompt file. Mutually exclusive with `prompt`. |
| `command` | Command | YAML list containing the executable followed by its arguments. |
| `model` | Codex, Claude, Gemini | Optional model override passed to the selected CLI. |
| `sandbox` | Codex | `read-only`, `workspace-write`, or `danger-full-access`. |
| `sandbox` | Gemini | Set to `enabled` to pass Gemini's sandbox flag; omit otherwise. |
| `permission_mode` | Claude | `dontAsk`, `acceptEdits`, `auto`, `plan`, or `bypassPermissions`. |
| `approval_mode` | Gemini | `plan`, `auto_edit`, or `yolo`. |
| `allowed_tools` | Claude, Gemini | List of tools the agent may use. Gemini entries cannot contain commas. |
| `bare` | Claude | Boolean. Uses Claude's bare mode for reproducible API-key-based automation. |

#### Codex

Codex jobs use [`codex exec` non-interactive mode](https://developers.openai.com/codex/noninteractive). Prompts are passed over stdin, sessions are ephemeral, and interactive approvals are disabled. `promptd` also passes `--skip-git-repo-check` so unattended jobs can use an explicitly configured working directory that is not a Git repository. The configured sandbox still controls filesystem access. Jobs that must edit their workspace need an explicit sandbox opt-in:

```yaml
agent:
  type: codex
  prompt: Fix the failing tests and verify the result.
  sandbox: workspace-write
```

#### Claude Code

Claude jobs use [`claude --print` automation mode](https://code.claude.com/docs/en/headless), disable session persistence, and can grant only the tools a job requires:

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

`permission_mode: bypassPermissions` is explicitly dangerous. Set `bare: true` for API-key-based automation that must not use Claude's saved subscription login.

#### Gemini CLI

Gemini jobs use the [`--prompt` headless mode](https://geminicli.com/docs/cli/tutorials/automation/), force text output, and skip the interactive workspace-trust question for the configured working directory:

```yaml
agent:
  type: gemini
  prompt: Review the repository and propose a migration plan.
  approval_mode: plan
```

Use `approval_mode: auto_edit` to permit edit tools. `approval_mode: yolo` auto-approves every tool and is explicitly dangerous.

#### Command

Command jobs execute the configured argument vector directly. Shell syntax such as pipes, redirects, variable expansion, and `&&` is not interpreted:

```yaml
agent:
  type: command
  command: ["git", "fetch", "--prune"]
```

To use shell behavior, invoke a shell explicitly and treat the configured command as trusted code.

### `run`

| Field | Required | Default | Value |
| --- | --- | --- | --- |
| `working_directory` | No | Service working directory | Directory in which the agent command runs. An explicit value is recommended. |
| `timeout` | No | `30m` | Positive Go duration. The entire process group is terminated on timeout. |
| `overlap` | No | `skip` | `skip` records an overlapping occurrence as skipped; `queue_one` retains at most one pending run. |
| `misfire` | No | `skip` | After downtime, `skip` advances to the next occurrence; `run_once` coalesces missed occurrences into one run. |
| `environment_file` | No | — | Dotenv-style file whose values override the inherited service environment. |

Environment files support one `KEY=VALUE` per line, optional `export`, blank lines, whole-line comments, and single- or double-quoted values. Values are not expanded by a shell.

## Why promptd?

Running an unattended coding agent usually requires a script, a cron entry or timer, environment setup, locking, logs, and failure handling. With `promptd`, the job is declarative and portable between supported supervision modes.

The daemon owns scheduling, locking, recovery, and run history. systemd supervises one long-running process instead of one timer and service pair per job.

## Commands

```text
promptd version
promptd validate --config path/to/config.yaml
promptd daemon --config path/to/config.yaml --state path/to/promptd.db --log-dir path/to/logs --socket path/to/promptd.sock
promptd status --state path/to/promptd.db
promptd setup
```

Run `promptd <command> --help` for command-specific options.

### Status

`promptd status` shows every registered job, including jobs disabled after removal from the current configuration, together with the next scheduled occurrence and latest run result:

```bash
promptd status
promptd status --format json
promptd status --offline
```

By default, status asks the running daemon for a consistent view over its owner-only Unix socket. If the daemon is unavailable, it falls back to opening the SQLite database read-only. `--offline` skips the control API explicitly.

### Reloading configuration

The daemon reloads its configuration on `SIGHUP`. An invalid reload is rejected while the last valid configuration remains active. With a systemd installation:

```bash
systemctl --user reload promptd
```

For a system service, omit `--user`.

## State, logs, and control API

The default per-user paths on Linux are:

| Data | Default path |
| --- | --- |
| Configuration | `~/.config/promptd/config.yaml` |
| State database | `~/.local/state/promptd/promptd.db` |
| Per-run logs | `~/.local/state/promptd/logs/` |
| Control socket | `~/.local/state/promptd/promptd.sock` |

The versioned local control API exposes `GET /v1/status` over the Unix socket. The daemon creates the socket with mode `0600` and refuses to replace a regular file or an active socket.

## Supervision modes

`promptd setup` detects the best available mode:

- `user-systemd` when the current user manager is reachable. The unit lives under `${XDG_CONFIG_HOME:-~/.config}/systemd/user`. Setup uses the numeric UID when enabling linger, so it also works for virtual users without a stable NSS name.
- `system-systemd` when setup runs as root. Running scheduled agents as root is refused by default; provide a non-root numeric identity with `--service-uid`, `--service-gid`, and `--service-home`, or use the explicit `--allow-root` escape hatch.
- `portable` outside Linux or when no usable systemd manager is available. This creates the initial configuration but leaves process supervision to the user or container runtime.

The user unit is enabled under `default.target`; the system unit under `multi-user.target`. Both restart on failure, use an owner-only umask, preserve the installation-time `HOME` and `PATH`, and support reload through `SIGHUP`. User lingering keeps the user manager alive after logout, as described by the [`loginctl enable-linger` documentation](https://www.freedesktop.org/software/systemd/man/latest/loginctl.html).

## Design

```text
systemd / launchd / container runtime
                  |
                  v
             promptd daemon
             |- scheduler
             |- SQLite run history
             |- agent runners
             `- local control socket
```

- The scheduler and execution engine are independent from systemd.
- Jobs execute without an implicit shell.
- User and system installations use the same core.
- Run timeouts terminate the complete child process group.
- Configuration reloads preserve the last valid state.

## Build from source

Requirements:

- Go 1.25 or newer.

```bash
git clone https://github.com/frux/promptd.git
cd promptd
make check
./bin/promptd validate --config examples/config.yaml
```

## Releases and verification

Every `v*` tag runs the race-enabled test suite and publishes reproducible static Linux amd64/arm64 archives, SHA-256 sidecars, and GitHub build-provenance attestations.

Verify a downloaded archive with:

```bash
sha256sum -c promptd_linux_amd64.tar.gz.sha256
gh attestation verify promptd_linux_amd64.tar.gz --repo frux/promptd
```

This follows GitHub's [artifact attestation workflow](https://docs.github.com/en/actions/how-tos/secure-your-work/use-artifact-attestations/use-artifact-attestations). Maintainers can reproduce archives locally with `make release VERSION=v0.1.0`; timestamps and embedded build metadata are derived from the source commit.

## v0.1 scope

- [x] Go project and CLI skeleton
- [x] Strict YAML configuration validation
- [x] Daemon lifecycle and safe configuration reload
- [x] SQLite state and run history
- [x] Process-group supervisor and timeouts
- [x] Command, Codex, Claude Code, and Gemini CLI runners
- [x] Cron and interval scheduler
- [x] Unix socket control API
- [x] `promptd status` overview for all registered jobs
- [x] `promptd setup` and checksum-verified shell installer
- [x] User and system systemd integration
- [x] Reproducible Linux release archives, checksums, and provenance

## Development

```bash
make test
make vet
make build
```

Keep platform-specific process and service-manager code behind small interfaces. The scheduler, configuration, and state packages must not depend on systemd.

## License

Licensed under the Apache License 2.0. See [`LICENSE`](LICENSE).
