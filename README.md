# promptd

`promptd` is a small, self-hosted daemon for running prompts and AI agent commands on a schedule without per-job cron entries or systemd timers.

The project is Linux-first. Its scheduler and execution engine are designed to remain independent from systemd; systemd will supervise one long-running `promptd` process.

> [!IMPORTANT]
> `promptd` is in early development. The daemon currently validates configuration, persists registered jobs, and maintains run history in SQLite. The process supervisor handles timeouts and whole process groups; scheduling, agent runners, and installation automation are the next milestones.

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
promptd daemon --config path/to/config.yaml --state path/to/promptd.db
```

The daemon reloads its configuration on `SIGHUP`. An invalid reload is rejected while the last valid configuration remains active. Its default per-user state database is `~/.local/state/promptd/promptd.db` on Linux.

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

See [`examples/config.yaml`](examples/config.yaml) for a complete example.

## Installation direction

The intended installation experience is:

```bash
curl -fsSL https://promptd.dev/install.sh | sh
```

The shell script will only download and verify a release binary. The testable `promptd setup` command will inspect the host, recommend user-systemd, system-systemd, or portable mode, show the planned changes, and perform the selected installation.

The bootstrap installer is not published yet. Do not use the command above until a signed release and installer are available.

## Roadmap to v0.1

- [x] Go project and CLI skeleton
- [x] Strict YAML configuration validation
- [x] Daemon lifecycle and safe configuration reload
- [x] SQLite state and run history
- [x] Process-group supervisor and timeouts
- [ ] Command and Codex runners
- [ ] Popular agent runners, starting with Claude Code
- [ ] Cron/interval scheduler
- [ ] Unix socket control API
- [ ] `promptd status` overview for all registered jobs
- [ ] `promptd setup` and verified shell bootstrap
- [ ] User and system systemd integration
- [ ] Linux release artifacts and packages

## Development

```bash
make test
make vet
make build
```

Please keep platform-specific process and service-manager code behind small interfaces. The scheduler, configuration, and state packages must not depend on systemd.

## License

Licensed under the Apache License 2.0. See [`LICENSE`](LICENSE).
