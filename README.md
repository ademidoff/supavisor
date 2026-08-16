# Supavisor

[![Coverage](https://img.shields.io/endpoint?url=https%3A%2F%2Fademidoff.github.io%2Fsupavisor%2Fcoverage.json)](https://ademidoff.github.io/supavisor/)

A process supervisor daemon written in Go, that is largely inspired by supervisord. It efficiently manages child processes with dependency support, config-based lifecycle management, and log rotation.

## Contents

- [Features](#features)
- [Installation](#installation)
  - [Download a release](#download-a-release)
  - [Build from source](#build-from-source)
- [Quick Start](#quick-start)
- [Command-Line Options](#command-line-options)
  - [supavisor](#supavisor-1)
  - [sctl](#sctl)
  - [Machine-readable output](#machine-readable-output)
- [Configuration](#configuration)
  - [Multi-file configuration](#multi-file-configuration)
  - [Configuration is strict](#configuration-is-strict)
  - [supavisor section](#supavisor-section)
  - [programs section](#programs-section)
- [Process States](#process-states)
  - [What a program is doing versus what it was asked to do](#what-a-program-is-doing-versus-what-it-was-asked-to-do)
- [Signals](#signals)
- [Reloading configuration](#reloading-configuration)
- [Recovering from a crash](#recovering-from-a-crash)
- [Process groups](#process-groups)
- [Running as PID 1](#running-as-pid-1)
  - [Start it with `exec`](#start-it-with-exec)
- [Stopping processes](#stopping-processes)
  - [When a program needs `stopsignal: INT`](#when-a-program-needs-stopsignal-int)
  - [Checking whether a program handles its stop signal](#checking-whether-a-program-handles-its-stop-signal)
- [Restart behavior](#restart-behavior)
- [Dependency Management](#dependency-management)
  - [Waiting for readiness instead of for the process](#waiting-for-readiness-instead-of-for-the-process)
- [Health checks](#health-checks)
  - [Probe kinds](#probe-kinds)
  - [Settings](#settings)
  - [What health does and does not do](#what-health-does-and-does-not-do)
- [Log Rotation](#log-rotation)
- [Best practices](#best-practices)
  - [One-off programs should not have dependents](#one-off-programs-should-not-have-dependents)
- [Examples](#examples)
  - [Basic Process](#basic-process)
  - [Process with Dependencies](#process-with-dependencies)
  - [Process that waits for its database to be ready](#process-that-waits-for-its-database-to-be-ready)
  - [Process with Log Rotation](#process-with-log-rotation)
  - [Process with Environment Variables](#process-with-environment-variables)
- [Architecture](#architecture)
- [Development](#development)
  - [Testing](#testing)
  - [Coverage reporting](#coverage-reporting)
  - [Releasing](#releasing)
- [License](#license)
- [Contributing](#contributing)

## Features

- **Process Management**: Start, stop, restart, and monitor child processes
- **Dependency Management**: Launch processes based on whether other processes are running
- **Health Checks**: Probe a program to tell whether it is ready to serve, and hold
  its dependents back until it is
- **Configuration-Based**: Configure process lifetime and behavior via YAML config files
- **Log Rotation**: Automatic log file rotation based on file size with configurable retention periods
- **CLI Tool**: Command-line interface for managing processes
- **Process States**: Track process states
  - STOPPED
  - STARTING
  - RUNNING
  - BACKOFF
  - STOPPING
  - EXITED
  - FATAL
- **Auto-restart Policies**: Configure restart behavior (always, never, unexpected)

## Installation

### Download a release

Each release publishes one `tar.gz` per architecture, holding both binaries, the
sample config, and a `checksums.txt` to verify the download against. Linux only,
`amd64` and `arm64`.

```bash
VERSION=0.9.0
ARCH=amd64  # or arm64

curl -sSfL -O https://github.com/ademidoff/supavisor/releases/download/v${VERSION}/supavisor_${VERSION}_linux_${ARCH}.tar.gz
curl -sSfL -O https://github.com/ademidoff/supavisor/releases/download/v${VERSION}/checksums.txt
sha256sum --ignore-missing -c checksums.txt

tar -xzf supavisor_${VERSION}_linux_${ARCH}.tar.gz
sudo install -m 0755 supavisor sctl /usr/local/bin/
```

The binaries are built with `CGO_ENABLED=0`, so they carry no libc dependency
and run on any distribution.

### Build from source

```bash
git clone https://github.com/ademidoff/supavisor
cd supavisor
make build
```

A source build reports its version as `dev`; only released binaries carry a
version, commit and build date.

## Quick Start

1. Create a configuration file (e.g., `supavisor.yml`):

```yaml
supavisor:
  logfile: /var/log/supavisor/supavisor.log
  pidfile: /var/run/supavisor.pid
  socket: /tmp/supavisor.sock

programs:
  database:
    command: /usr/bin/postgres
    autostart: true
    autorestart: unexpected
    stdout_logfile: /var/log/database/stdout.log
    stdout_logfile_maxbytes: 50MB
    stdout_logfile_backups: 10

  webapp:
    command: /usr/bin/python app.py
    directory: /opt/webapp
    autostart: true
    autorestart: always
    startsecs: 10
    depends_on:
      - database
    stdout_logfile: /var/log/webapp/stdout.log
    stdout_logfile_maxbytes: 10MB
    stdout_logfile_backups: 5
    stdout_logfile_maxage: 7
```

2. Start the supavisor daemon:

```bash
# Run in foreground
./supavisor -c supavisor.yml

# Run in background
./supavisor -c supavisor.yml &

# Or use nohup for persistent background execution
nohup ./supavisor -c supavisor.yml &
```

**Note**:
- When a logfile is configured, all logs are written to the log file only (no console output)
- When no logfile is configured, logs are written to stdout (useful for container environments)
- To run without a logfile, comment out or omit the `logfile` setting in the config
- Supavisor holds an exclusive lock on its PID file for as long as it runs, which
  is what prevents a second instance from starting. Starting one reports the PID
  of the daemon that already holds the lock.
- The kernel releases the lock when the daemon exits, including on a crash, so a
  PID or socket file left behind by a crash is not an obstacle: supavisor takes
  the lock, reuses the paths and starts normally. There is nothing to remove by
  hand, and supavisor can run under `systemd`'s `Restart=` or a container restart
  policy without needing intervention to come back up.
- On shutdown supavisor removes its PID file and socket only if those paths still
  refer to the files it created, so a slow shutdown can never delete the files of
  a daemon that has already replaced it.
- Processes that outlive a supavisor crash are stopped when supavisor next starts.
  See [Recovering from a crash](#recovering-from-a-crash).

3. Use the CLI tool to manage processes:

```bash
# Check status
./sctl status

# Look at one process, including why it is not running
./sctl status webapp

# Start a process
./sctl start webapp

# Stop a process
./sctl stop webapp

# Restart a process
./sctl restart webapp

# Reload configuration
./sctl reload

# Shutdown supavisor
./sctl shutdown
```

## Command-Line Options

### supavisor

```bash
./supavisor [options]
```

Options:
- `-c, -config <path>`: Path to configuration file (default: `/etc/supavisor/supavisor.yml`)
- `-logfile <path>`: Override log file path from config (optional)
- `-version`: Print version, commit and build date, then exit

### sctl

```bash
./sctl [options] <command> [process-name]
```

Options must be given before the command; `sctl -s /run/supavisor.sock status`
works, `sctl status -s /run/supavisor.sock` is rejected.

Options:
- `-s, -socket <path>`: Path to supavisor socket (default: `/tmp/supavisor.sock`)
- `-o, -output <format>`: `table` (default) or `json`, see [Machine-readable output](#machine-readable-output)
- `-version`: Print version, commit and build date, then exit

Access to the socket is access to the daemon: it can stop supervised processes,
and `start` causes configured commands to run as the user supavisor runs as. The
socket is therefore created `0660`, owned by `socket_group` when one is set.

Commands:
- `status`: Show status of all processes
- `status <name>`: Show one process in detail, including why it is not running
- `start <name>`: Start a specific process
- `stop <name>`: Stop a specific process
- `restart <name>`: Restart a specific process
- `reload`: Re-read the configuration and apply what changed
- `shutdown`: Shutdown supavisor daemon

### Machine-readable output

The table is for people. Its aligned columns, the `N/A` it prints for numbers
that mean nothing yet and the `-` it prints for absent health are presentation
choices, and they are not a stable interface. `-o json` prints the response the
daemon actually sent:

```bash
$ sctl -o json status
{
  "data": {
    "processes": [
      {
        "desired": "RUNNING",
        "exit_code": 0,
        "health": "HEALTHY",
        "name": "db",
        "pid": 4211,
        "restart_count": 0,
        "state": "RUNNING",
        "uptime": "2m 10s"
      }
    ]
  },
  "success": true
}
```

The shape is the same for every command, so `data` carries the processes for
`status` and the applied diff for `reload`, and the envelope is printed whether
or not the command succeeded. A failure is reported as JSON rather than on
stderr, with the exit code still carrying the outcome:

```bash
$ sctl -o json start nosuchprog
{
  "message": "process nosuchprog not found",
  "success": false
}
$ echo $?
1
```

`sctl -o json status <name>` keeps the same envelope as the whole-list form, with
one entry in `processes`, so parsing does not depend on how the command was
invoked.

Two things to read carefully, because the table hides them behind placeholders:

- **`pid` is whatever the daemon last knew.** For a program that is not running
  it is the PID of the run that ended, where the table prints `N/A`. Read it
  together with `state`.
- **`health` is `NONE` when no probe is configured**, which the table shows as
  `-`. It does not mean unhealthy.

## Configuration

### Multi-file configuration

In addition to the main config file passed via `-c`, supavisor will look for a
sibling drop-in directory named after the main file. For
`/etc/supavisor/supavisor.yml` the directory is `/etc/supavisor/supavisor.d/`.

- Every `*.yml` and `*.yaml` file in the drop-in directory is loaded in lexical
  order and merged with the main file.
- Fragment files may only define the `programs:` section. A `supavisor:` section
  in a fragment is rejected; daemon-level settings must live in the main file.
- A program name defined in two files is a hard error — supavisor refuses to
  start and names both source files.
- `depends_on` can reference programs defined in any file; dependencies are
  resolved across the merged program set.

If the drop-in directory does not exist, supavisor simply loads the main file.

### Configuration is strict

Unknown keys are rejected. A misspelled setting such as `autorestrt: always` fails
at startup and names the offending key, rather than being ignored and leaving the
program running on defaults nobody chose.

Settings whose zero value is meaningful distinguish "set to zero" from "not set":
`max_restarts: 0` means never retry, `startsecs: 0` means treat the process as
started immediately, and `stopwaitsecs: 0` means kill without waiting. Omitting a
setting takes its default.

### supavisor section

- `logfile`: Path to supavisor's own log file (optional)
  - When specified, all logs are written to this file only (no console output)
  - When not specified, logs are written to stdout (if running in a terminal)
  - Can be overridden with the `-logfile` command-line flag
- `pidfile`: Path to PID file (default: `/var/run/supavisor.pid`)
- `socket`: Path to Unix domain socket for CLI communication (default:
  `/tmp/supavisor.sock`). Unix sockets cap the path at about 104 bytes; a longer
  one is rejected at startup rather than failing with `bind: invalid argument`.
- `socket_group`: Group given ownership of the control socket, by name or numeric
  gid (optional). The socket is created with mode `0660`, so by default only the
  user running supavisor can use `sctl`. Set this to an administrators' group to
  let its members manage processes without being that user.
- `log_format`: Log format - `text` (default) or `json`
- `log_level`: Log level - `debug`, `info` (default), `warn`, or `error`

### programs section

Each program is defined under `programs` with its name as the key:

- `command`: Command to run (required). This is not run through a shell: there is
  no expansion, globbing or substitution. Quoting and backslash escapes are
  supported, so `command: /bin/app --msg "hello world"` and `--path a\ b` both
  work. To use shell features, invoke a shell explicitly:
  `command: /bin/sh -c "a && b"`.
- `directory`: Working directory for the process
- `autostart`: Start process automatically on supavisor startup (default: true)
- `autorestart`: Restart policy - `always`, `never`, or `unexpected` (default: unexpected)
- `startsecs`: Seconds to wait before considering start successful (default: 1)
- `stopsignal`: Signal sent to stop the process - `TERM` (default), `INT`, `QUIT`,
  `HUP`, `USR1`, `USR2` or `KILL`. The `SIG` prefix is optional. See
  [When a program needs `stopsignal: INT`](#when-a-program-needs-stopsignal-int).
- `stopwaitsecs`: Seconds to wait for the process to exit on `stopsignal` before it
  is killed (default: 10)
- `max_restarts`: Maximum number of *consecutive* restarts before giving up (default: 3).
  See [Restart behavior](#restart-behavior) for how the counter is reset.
- `depends_on`: Programs that must come up first, either as a list of names or as a
  mapping that says what each one has to reach. See
  [Dependency Management](#dependency-management).
- `health_check`: How to tell whether this program is ready to serve, rather than
  merely running. See [Health checks](#health-checks).
- `stdout_logfile`: Path to stdout log file. If omitted, the process's stdout is
  discarded (connected to `/dev/null`).
- `stderr_logfile`: Path to stderr log file. If omitted, the process's stderr is
  discarded (connected to `/dev/null`).
- `stdout_logfile_maxbytes`: Maximum size before rotation (supports KB, MB, GB suffixes, default: 50MB).
  An unparseable size is a startup error, not a silent fallback to the default.
- `stdout_logfile_backups`: Number of rotated logs to keep (default: 10)
- `stdout_logfile_maxage`: Days to keep rotated logs (0 = no limit, default: 0)
- `stderr_logfile_maxbytes`: Maximum size before rotation (default: 50MB)
- `stderr_logfile_backups`: Number of rotated logs to keep (default: 10)
- `stderr_logfile_maxage`: Days to keep rotated logs (default: 0)
- `environment`: Map of environment variables (e.g., `APP_ENV: production`)
- `priority`: Order among programs that are ready to start at the same moment,
  lower first (default: 999). Dependencies still decide what *may* start;
  priority only settles ties between programs that are all ready.

The `user` setting is not implemented. Rather than accept it and run the program
as supavisor's own user anyway, a config that sets it is rejected at startup. Run
supavisor itself as the intended user instead.

## Process States

- `STOPPED`: Process was stopped by supavisor (e.g., via `sctl stop`)
- `STARTING`: Process is starting up
- `RUNNING`: Process is running normally
- `BACKOFF`: Process failed to start, waiting before retry
- `STOPPING`: Process is being stopped (transitional state)
- `EXITED`: Process exited on its own (completed normally or crashed)
- `FATAL`: Process failed to start after all retries

`sctl status` lists every configured program, including ones that have never been
started; those are reported as `STOPPED`.

`sctl stop` always ends in `STOPPED`, including for a process that had already
exited or reached `FATAL` on its own. Stopping a process that is waiting out a
restart backoff cancels that pending restart.

The `HEALTH` column of `sctl status` reports separately on programs that declare a
`health_check`; see [Health checks](#health-checks).

### What a program is doing versus what it was asked to do

A state on its own does not say whether anything is wrong: a `STOPPED` program may
be one nobody has asked for, or one that wants to run and cannot. The `DESIRED`
column separates them, and `sctl status <name>` says why:

```
$ sctl status
NAME     STATE     DESIRED   HEALTH      PID     EXIT_CODE   RESTARTS   UPTIME
----     -----     -------   ------      ---     ---------   --------   ------
api      STOPPED   RUNNING   -           N/A     0           0          N/A
db       RUNNING   RUNNING   UNHEALTHY   89734   0           0          8s
doomed   FATAL     RUNNING   -           N/A     1           1          N/A
idle     STOPPED   STOPPED   -           N/A     0           0          N/A

$ sctl status api
Name:        api
State:       STOPPED
Desired:     RUNNING
Health:      -
PID:         N/A
Exit code:   0
Restarts:    0
Uptime:      N/A
Reason:      dependency db is running but its health check is UNHEALTHY
```

`Reason` appears only when there is something to explain, and covers every way a
program can be not running while it is still wanted:

| State | Reason |
| --- | --- |
| `STOPPED` | `dependency db is running but its health check is UNHEALTHY` |
| `FATAL` | `gave up after 1 restart` |
| `EXITED` | `exited with status 3; autorestart is never` |
| `EXITED` | `exited cleanly; autorestart is unexpected` |

The two `EXITED` forms name the policy that decided not to bring the program back,
which is the setting to change if that was not what you wanted. A program that is
running has no `Reason` line, and neither has one that is simply not wanted. The
same string is available to API clients in the `reason` field of the status
response.

## Signals

Supavisor handles:

- `SIGTERM` / `SIGINT`: stop all processes and exit. A second one, sent while
  that shutdown is still in progress, exits immediately rather than being
  ignored, so a program that refuses to stop cannot leave the daemon unkillable
  by anything short of `SIGKILL`.
- `SIGHUP`: reload the configuration, equivalent to `sctl reload`.

## Reloading configuration

`sctl reload` re-reads the config file and its drop-in fragments and applies the
difference:

- Programs that are unchanged keep running and are not disturbed.
- Programs that were removed are stopped and forgotten.
- Programs that were added are started if they are set to autostart.
- Programs whose definition changed are stopped and started again on the new one.

A program that was deliberately stopped stays stopped, even if its definition
changed: reload applies configuration, it does not override what an operator
asked for.

If the new configuration is invalid, the reload is refused and nothing changes,
so a typo cannot take running programs down. Daemon-level settings (`pidfile`,
`socket`, `socket_group`, `logfile`, `log_format`, `log_level`) are bound at
startup; changing one is reported by name and needs a restart.

Reload reports what it applied, naming only the categories that have something in
them:

```
$ sctl reload
configuration reloaded
  changed: victoriametrics, vmalert

$ sctl reload
configuration reloaded, nothing changed
```

This matters to anything driving supavisor over the socket rather than by hand. A
program that owns a drop-in directory, writes fragments into it and then reloads
can check whether its own name is in `changed`, because a program whose definition
changed is stopped and replaced — including when it is the one that asked for the
reload. The reload itself still completes; what the caller loses is the chance to
see it finish.

## Recovering from a crash

If supavisor is killed rather than shut down, the processes it manages keep
running: they are reparented to init and nothing supervises them any more.
Starting supavisor again would otherwise add a second copy of every program.

Supavisor therefore records the processes it owns in a state file next to its PID
file (`/var/run/supavisor.pid` is tracked by `/var/run/supavisor.state`), and on
startup stops anything from that file that is still running before starting
anything of its own. Survivors are sent `SIGTERM`, and `SIGKILL` if they have not
exited within five seconds. Recovery needs no operator intervention.

Supavisor stops these processes rather than adopting them because they are no
longer its children: it cannot wait on them, so it could neither collect their
exit status nor notice them exiting.

A recorded PID is only acted on if the process running under it is still the one
supavisor started. PIDs get reused, so each record also stores the process's start
time; if it does not match, the PID now belongs to something unrelated and is left
alone.

Records are also tied to the boot they were made on. A start time on Linux is
measured in ticks since boot, so it repeats every boot: without this, a state file
that outlived a restart could match a PID against a process that merely started at
the same offset of the current boot. Records from an earlier boot are discarded
rather than acted on.

Both checks need platform support, which supavisor implements for Linux and macOS.
Anywhere else, recorded processes are left running rather than risking killing the
wrong one.

The state file is removed on a clean shutdown, so it only ever has contents to act
on after a crash.

## Process groups

Each managed process is started in a process group of its own, and everything it
spawns inherits that group. Stopping a process signals the whole group rather than
just the command supavisor launched, so a wrapper script does not leave the real
workload running:

```yaml
programs:
  worker:
    # 'sh' is the direct child, but the python process is what matters
    command: /bin/sh -c "exec_setup && python worker.py"
```

After a process exits, anything still left in its group is killed, so a program
that backgrounds work and returns does not leak it.

One consequence: because managed processes are in their own groups, `Ctrl+C` in a
terminal running supavisor in the foreground reaches supavisor only. Supavisor
then stops its processes itself, in its own order.

## Running as PID 1

Supavisor is safe to run as PID 1 in a container, with no `tini` or `dumb-init`
in front of it.

PID 1 inherits every orphan on the system. When a supervised program spawns a
child and exits first, that child is reparented to PID 1, and when it eventually
exits somebody has to call `wait()` for it or it stays a zombie forever. Nothing
else will: its original parent is gone. A supervisor that only waits for its own
children accumulates them for as long as the container runs.

Supavisor therefore does all of its waiting in one place. A single reaper calls
`wait4(-1)`, hands each status to whichever program owns that PID, and discards
the rest, which is exactly the orphan case. It follows that nothing else may
wait: `os/exec`'s own `Wait` is a second waiter, and two waiters race for the
same statuses, so an exit code goes missing every so often and a program that
exited cleanly looks like it crashed. That is the reason the process package
never calls `cmd.Wait()`.

None of this needs configuring, and it costs nothing when supavisor is not PID 1:
with a real init above it, orphans reparent there instead and the reaper simply
never sees them.

### Start it with `exec`

An entrypoint script has to hand PID 1 over rather than keep it:

```bash
#!/bin/sh
# ... setup ...

exec supavisor -c /etc/supavisor/supavisor.yml
```

Without the `exec` the script stays PID 1 and supavisor runs as its child. Docker
then sends `SIGTERM` to the script on `docker stop`, and a shell does not pass it
on: supavisor never learns it is meant to stop, and every program is killed
outright when the container is torn down. None of the shutdown described in
[Stopping processes](#stopping-processes) happens — no `stopsignal`, no
`stopwaitsecs`, no dependency order. A database configured for a fast shutdown
gets a kill instead, and recovers on next boot.

Measured, stopping the same container both ways:

| | supavisor sees `SIGTERM` | shuts down cleanly |
|---|---|---|
| without `exec` | no | no, last line is a program reaching `RUNNING` |
| with `exec` | yes | yes, down to `Supavisor daemon stopped` |

Reaping is *not* the reason, which is what makes this easy to miss: a shell left
as PID 1 does reap orphans, both `ash` and `bash`, so nothing accumulates and the
container looks healthy. The damage only appears when you stop it.

Both halves of that are reproducible rather than asserted. `probes/pid1-zombies.sh`
runs supavisor as PID 1 in a container and counts what it leaves behind, and
`probes/wait4-race/main.go` demonstrates the exit codes that go missing if a second
waiter is added. See `probes/README.md`.

## Stopping processes

`sctl stop`, `sctl restart` and daemon shutdown all follow the same sequence:

1. `stopsignal` (default `TERM`) is sent to the process group.
2. Supavisor waits up to `stopwaitsecs` (default 10) for the group to exit.
3. Anything still alive is sent `SIGKILL`.

### When a program needs `stopsignal: INT`

`SIGTERM` is the right default, because it is the signal a daemon is
conventionally asked to shut down with. But a program only shuts down cleanly on
a signal it actually handles, and some programs only handle `SIGINT`. Sending
those `SIGTERM` skips their cleanup entirely and they are killed `stopwaitsecs`
later.

Set `stopsignal: INT` when the program is one of these:

- **Written to be stopped with Ctrl+C.** A program developed by running it in a
  terminal usually grows a `SIGINT` handler and nothing for `SIGTERM`, because
  Ctrl+C was the only way it was ever stopped.
- **A Python program relying on `KeyboardInterrupt`.** `SIGINT` raises
  `KeyboardInterrupt`, so `try`/`finally`, `with` blocks and `atexit` handlers all
  run. `SIGTERM` has no default handler in Python at all: the process is
  terminated with nothing run. A program that catches `KeyboardInterrupt`, or
  relies on `atexit`, but never calls `signal.signal(signal.SIGTERM, ...)` needs
  `stopsignal: INT`.
- **A Node.js program registering only `process.on('SIGINT', ...)`.** Same shape:
  the default disposition for `SIGTERM` terminates the process without running
  the handler.
- **A program that treats the two asymmetrically**, using `SIGINT` to drain
  gracefully and `SIGTERM` to stop at once. Some queue workers and batch jobs do
  this deliberately so an operator can pick.
- **Anything that stopped cleanly under an older supavisor.** Stopping used to be
  hard-coded to `SIGINT`; `stopsignal: INT` restores the previous behavior for a
  program that regressed.

The same reasoning applies to the other values. `QUIT` is worth knowing about
because some servers, nginx among them, document `SIGQUIT` as their graceful
shutdown and `SIGTERM` as a fast one.

### Checking whether a program handles its stop signal

A program that ignores its stop signal always takes the full `stopwaitsecs` and
is then killed. In supavisor's log that looks like:

```
msg="Signaling the process group for graceful shutdown" process=importer signal=terminated
msg="Graceful shutdown timeout, sending SIGKILL to the process group" process=importer
msg="Force killed" process=importer
```

A program that handles the signal logs this instead, and promptly:

```
msg="Signaling the process group for graceful shutdown" process=importer signal=terminated
msg="Process exited gracefully" process=importer
```

Timing shows it too: `time sctl stop <name>` taking about `stopwaitsecs` rather
than returning straight away means the signal was ignored. You can also check the
program directly, outside supavisor:

```bash
# Run it by hand, then from another terminal:
kill -TERM <pid>   # does it clean up and exit?
kill -INT  <pid>   # does this work where TERM did not?
```

Putting it together:

```yaml
programs:
  importer:
    command: /usr/bin/python3 /opt/importer/run.py
    stopsignal: INT    # cleanup hangs off KeyboardInterrupt
    stopwaitsecs: 30   # let the in-flight batch finish
```

If a program handles neither signal there is nothing to configure: it will always
be killed. Lower its `stopwaitsecs` so shutdown is not held up waiting for a
graceful exit that is never going to happen.

## Restart behavior

When a process exits on its own and its `autorestart` policy calls for a restart,
supavisor waits before starting it again, doubling the delay on each consecutive
attempt: 1s, 2s, 4s, 8s, 16s, then 30s for every attempt after that.

The consecutive-restart counter is compared against `max_restarts`; exceeding it
puts the process in `FATAL`. The counter is reset to zero whenever a run lasts at
least 60 seconds, so `max_restarts` bounds crash loops rather than the total number
of restarts over the lifetime of the daemon. A process that is restarted once a
week will not eventually reach `FATAL`.

## Dependency Management

Supavisor works from a desired state for each program rather than from a fixed
startup sequence. `autostart` sets the initial desired state, `sctl start` and
`sctl stop` change it, and a reconcile loop continuously moves each program
towards whatever its desired state currently is.

Dependencies fall out of that: a program is started once every entry in its
`depends_on` has reached the condition it is waited on for, and until then it
simply stays `STOPPED` and is reconsidered. Nothing is scheduled in advance, so a
dependency that takes a long time to come up, or that is started by hand much
later, does not strand the programs behind it:

```yaml
programs:
  db:
    command: /usr/bin/postgres
    autostart: false   # started by hand, whenever
  api:
    command: /usr/bin/api
    depends_on: [db]   # starts on its own once db is RUNNING
```

### Waiting for readiness instead of for the process

`depends_on` also accepts a mapping, which says what each dependency has to reach:

```yaml
programs:
  db:
    command: /usr/bin/postgres
    health_check:
      exec: pg_isready -q

  api:
    command: /usr/bin/api
    depends_on:
      db:
        condition: healthy   # wait for the check to pass, not just for the process

  logs:
    command: /usr/bin/tailer
    depends_on:
      db:                    # no condition means started, as the list form does
```

- `condition: started` (the default) waits for the dependency to be `RUNNING`,
  which is what `depends_on` has always meant.
- `condition: healthy` also waits for the dependency's `health_check` to pass.
  The dependency must declare one, or the configuration is rejected at startup
  rather than leaving the dependent waiting for something that can never happen.

Both forms can be mixed across programs, and the list form is unchanged: adding a
`health_check` to a program does not start gating dependents that only asked for it
to be running.

Other behavior:

- When a dependency stops or crashes, it is restarted according to its own
  `autorestart` policy. Dependent processes keep running: supavisor does not
  stop or restart them.
- Restarts within a run are the `autorestart` policy's business, not the loop's.
  A program that exited on its own, or that gave up after `max_restarts`, is left
  alone rather than being started again by reconciliation.
- `sctl start` waits for the outcome and reports it. If the program cannot start
  because something it depends on will never come up, it says so immediately and
  names the program actually responsible, however far down the chain it is.
- A program that is being held back logs why, once per distinct reason rather than
  on every pass of the reconcile loop. A dependency that stays down for an hour
  costs one line, and the line changes as the reason does. The same explanation is
  available at any time from `sctl status <name>`.
- A desired state persists: stopping a program keeps it stopped, and a program
  waiting on a dependency starts as soon as that dependency is up, with no second
  command needed.

Shutdown runs the same relationships in reverse: programs are stopped from the
outermost dependents inwards, so nothing is pulled out from under something still
using it. Programs that do not depend on each other are stopped at the same time,
so total shutdown time is bounded by the slowest tier rather than by the sum of
every program's `stopwaitsecs`.

Circular dependencies are detected and rejected during configuration validation.

## Health checks

`RUNNING` means the process is alive: it stayed up for `startsecs` and its PID still
answers. That is liveness, not readiness. A database forks early and then spends
time on recovery before it accepts connections, and a service can be up long before
it binds its socket or finishes its migrations. A dependent started in that window
fails to connect and exits, and whether the stack recovers comes down to its restart
policy.

`startsecs` cannot close that gap, because it is a fixed sleep: too short on a cold
start, which is exactly when the race bites, and paid in full on every restart when
it is long enough. A health check observes readiness instead of guessing at it:

```yaml
programs:
  db:
    command: /usr/bin/postgres
    health_check:
      exec: pg_isready -q -h 127.0.0.1
      interval: 2s
      timeout: 5s
      retries: 3
      start_period: 60s
```

### Probe kinds

Exactly one of these is required:

- `exec`: run a command; exit status 0 means ready. It runs from the program's
  `directory` and with the program's `environment`, so a check like `pg_isready`
  sees the same settings the program was given. Like `command`, it is not run
  through a shell; use `/bin/sh -c '...'` if you need one.
- `tcp`: connect to `host:port`; a completed connection means ready.
- `http`: issue a `GET`; any status below 400 means ready.

### Settings

- `interval`: how often to probe (default: 2s). The first probe runs immediately
  rather than after one interval, so a program that is ready straight away does not
  pay the interval as startup latency. A probe never overlaps itself: the next one
  is due only after the previous attempt has finished.
- `timeout`: how long one attempt may take before it counts as a failure (default: 5s)
- `retries`: consecutive failures before the program is reported `UNHEALTHY`
  (default: 3)
- `start_period`: a window after the process starts during which failures do not
  count, for a program that is known to need time to initialize (default: 0). It
  applies only until the first successful check; after that, failures count normally.

Durations are written as Go durations, such as `500ms`, `2s` or `1m`. An unparseable
one is a startup error rather than a silent fallback to the default, as is a probe
that sets none or more than one of `exec`, `tcp` and `http`.

### What health does and does not do

Health is reported alongside the process state, in the `HEALTH` column of
`sctl status` and in the `health` field of the status API:

```
NAME       STATE     HEALTH      PID    EXIT_CODE   RESTARTS   UPTIME
----       -----     ------      ---    ---------   --------   ------
db         RUNNING   HEALTHY     4211   0           0          2m 10s
api        RUNNING   -           4230   0           0          1m 44s
```

- `-`: no health check is configured, or the program is not running, where
  readiness would mean nothing
- `STARTING`: a configured check that has not passed yet
- `HEALTHY`: the last attempt passed
- `UNHEALTHY`: `retries` attempts in a row have failed

A failing probe **does not** stop or restart the program. Supavisor reports what it
observes, and restarts stay tied to the process actually exiting, so a flapping
probe cannot take a working process down. What health decides is whether dependents
that asked for `condition: healthy` may start, and a dependency that goes unhealthy
after they are already up does not stop them, in the same way a dependency that
crashes does not.

Probes belong to a run: they start with the process and stop when it exits or is
stopped, and the health goes back to `-`.

## Log Rotation

Processes do not write to their log files directly. Supavisor gives each stream a
pipe, reads it line by line, and writes to the log file itself. This is what makes
rotation reliable: the log descriptor belongs to supavisor, so renaming the file
and opening a new one actually redirects subsequent output. A process holding its
own descriptor would keep writing to the renamed file no matter what the supervisor
did to the directory entry.

When a line would take the file past its configured maximum size:

1. Existing backups are rotated (`.1` -> `.2`, `.2` -> `.3`, etc.)
2. Current log is moved to `.1`
3. A new file is created and output continues into it
4. Backups beyond the configured count are removed
5. Backups older than `maxage` days are removed

Because rotation happens on line boundaries, a log file can exceed its maximum by
at most one line. A run of output longer than 64KB with no newline in it is written
out in pieces rather than buffered.

Backup pruning runs when a log is opened and at each rotation, so a process that
never produces enough output to rotate keeps its existing backups until it is
next restarted.

Notes:

- If a stream has no `stdout_logfile`/`stderr_logfile` configured, it is connected
  to `/dev/null` and nothing is captured.
- If both streams point at the same file, they share one pipe, so their output
  interleaves in the order it was written.
- Two different programs may not share a log file. Supavisor owns each log
  descriptor so that rotation works, so sharing one would have two writers
  rotating the same files and destroying each other's output. This is rejected at
  startup.
- Because output flows through supavisor, a process that logs faster than the disk
  can absorb will eventually block on write, and a process that outlives a
  supavisor crash will see its output descriptor close.

## Best practices

### One-off programs should not have dependents

A one-off program — one that does a piece of work and exits, such as a migration, a
bootstrap script or an init playbook — is a perfectly good thing to supervise. With
the default `autorestart: unexpected` it runs once, settles in `EXITED` and is left
there, while a non-zero exit is retried and eventually reaches `FATAL`, so a failure
is still visible in `sctl status` rather than passing silently.

What a one-off should not be is the subject of another program's `depends_on`.

`condition: started` is satisfied while the dependency is `RUNNING`, and a one-off is
`RUNNING` only for as long as its work takes. A dependent either starts inside that
window — concurrently with the task it was meant to follow, not after it — or, if it
comes to be started after the work has finished, does not start at all. The
dependency has exited and will never come up again, so `sctl start` refuses it and
says which program is responsible, and the reconcile loop leaves it `STOPPED`.

The second case is the one that hurts, because it surfaces long after the
configuration was written and looks nothing like a configuration problem. A
dependent that crashes and is restarted an hour after the one-off completed cannot
come back.

`condition: healthy` does not rescue it. Probes belong to a run, so a one-off's
health returns to `-` when it exits; unless a probe happened to pass while the
process was still alive, the dependent is waiting for a state that can no longer
occur. Whether it does is a matter of timing between the check interval and the
exit, which is not something to build on.

So leave one-offs out of the graph. Start them early with `priority` and let the
programs that care about the result gate on something long-lived instead:

```yaml
programs:
  # Runs once and exits. Nothing lists it in depends_on.
  migrate:
    command: /usr/bin/migrate --up
    autorestart: unexpected
    priority: 1

  db:
    command: /usr/bin/postgres
    priority: 5
    health_check:
      exec: pg_isready -q

  api:
    command: /usr/bin/api
    priority: 10
    depends_on:
      db:
        condition: healthy
```

Note that `priority` orders the launch of programs that are ready to start; it does
not wait for one to finish. There is currently no condition meaning "has completed
successfully", so when a dependent genuinely must not run until the work is done,
that ordering has to be expressed some other way: as a readiness check on the
long-lived service that consumes the result, or by doing the work in the dependent's
own startup path.

## Examples

### Basic Process

```yaml
supavisor: {}

programs:
  myapp:
    command: /usr/bin/myapp
    autostart: true
    autorestart: always
    stdout_logfile: /var/log/myapp/stdout.log
```

### Process with Dependencies

```yaml
supavisor: {}

programs:
  database:
    command: /usr/bin/postgres
    autostart: true
    autorestart: unexpected

  webapp:
    command: /usr/bin/python app.py
    depends_on:
      - database
    autostart: true
    autorestart: always
```

### Process that waits for its database to be ready

```yaml
supavisor: {}

programs:
  database:
    command: /usr/bin/postgres
    autostart: true
    autorestart: unexpected
    health_check:
      exec: pg_isready -q -h 127.0.0.1
      interval: 2s
      start_period: 60s

  webapp:
    command: /usr/bin/python app.py
    depends_on:
      database:
        condition: healthy
    autostart: true
    autorestart: always
```

### Process with Log Rotation

```yaml
supavisor: {}

programs:
  worker:
    command: /usr/bin/worker
    stdout_logfile: /var/log/worker/stdout.log
    stdout_logfile_maxbytes: 100MB
    stdout_logfile_backups: 10
    stdout_logfile_maxage: 30
```

### Process with Environment Variables

```yaml
supavisor: {}

programs:
  myapp:
    command: /usr/bin/myapp
    environment:
      APP_ENV: production
      APP_PORT: "8080"
      PATH: /usr/bin:/usr/local/bin:/opt/bin
      DEBUG: "false"
```

## Architecture

- `cmd/supavisor`: Main daemon entry point
- `cmd/sctl`: CLI tool for managing processes
- `internal/config`: Configuration file parsing
- `internal/process`: Process lifecycle management
- `internal/dependency`: Dependency resolution engine
- `internal/logrotate`: Log rotation and retention
- `internal/server`: Core supavisor daemon
- `internal/api`: API types for IPC communication

## Development

### Testing

```bash
# Run the full suite with the race detector, verbose output
make test

# Same, plus coverage: prints the total and writes coverage.out
make cover

# Same, then open the annotated per-line report in a browser
make cover-html

# Bypass Go's test result cache and genuinely re-run everything
make cover GOTESTFLAGS=-count=1
```

Go caches test results, so a repeated `make cover` over unchanged source
replays the previous verdict in about a second. That is convenient locally but
wrong for CI: several tests here are timing-dependent, and a replayed result
means the race detector never gets a fresh attempt. CI therefore passes
`GOTESTFLAGS=-count=1`.

`make cover` passes `-coverpkg=./...` so that code exercised across package
boundaries is credited to the package that defines it. Without it, helpers in
`internal/config` and `internal/process` that are driven by the `internal/server`
tests are reported as uncovered.

The `cmd/supavisor` and `cmd/sctl` packages show low coverage because they are
binary entry points. Covering them meaningfully requires building an
instrumented binary with `go build -cover` and setting `GOCOVERDIR`, rather than
more unit tests.

### Coverage reporting

The `Coverage` workflow (`.github/workflows/coverage.yml`) runs on every push to
`main` and on every pull request. On `main` it publishes three files to
[GitHub Pages](https://ademidoff.github.io/supavisor/), built by
`.github/scripts/coverage-site.sh`:

- `index.html`: the total, a per-file table sorted least-covered first, and a link to the annotated report
- `report.html`: `go tool cover -html` output, annotated line by line
- `coverage.json`: a [shields.io endpoint](https://shields.io/badges/endpoint-badge) descriptor backing the badge at the top of this README

Pull requests run the tests and build the site, but do not deploy.

The per-file numbers are computed from `coverage.out` rather than read from
`go tool cover -func`, which reports per function. Under `-coverpkg=./...` every
test binary emits a record for every block it can see, so the same block appears
once per binary; the script deduplicates on the block key and sums the execution
counts before deciding whether a block was covered. The resulting total is
checked against `go tool cover -func` on each change.

Enabling this on a fork requires setting **Settings > Pages > Source** to
**GitHub Actions**.

### Releasing

Releases are cut by the `Release` workflow (`.github/workflows/release.yml`),
which only the repository owner can run: from the Actions tab, run **Release**
against the branch to release and give it a version tag such as `v0.9.0`.

The job validates the tag format, refuses a tag that already exists, and runs
the test suite before tagging the commit, then hands off to
[GoReleaser](https://goreleaser.com) (`.goreleaser.yaml`). GoReleaser builds
`linux/amd64` and `linux/arm64`, stamps each binary with the tag, commit and
commit date, packs both binaries into one `tar.gz` per architecture, and uploads
the archives and `checksums.txt` to a GitHub release. The release notes are the
commit subjects since the previous tag, minus `docs:`, `test:` and `chore:`.

The release is left as a **draft**: review the artifacts and notes on the
Releases page, then publish it by hand. Nothing is visible to users until you
do. Note that the tag is pushed regardless — a draft does not defer that.

Because the tests run before the tag is created, a failing build leaves no tag
behind. If GoReleaser itself fails the tag has already been pushed, and it must
be deleted (`git push --delete origin v0.9.0`) before that version can be
retried.

The build is reproducible: `-trimpath` keeps local paths out of the binaries and
the commit timestamp is used instead of the build time, so rebuilding a tag
yields byte-identical archives.

To exercise the release locally without publishing anything:

```bash
goreleaser check
goreleaser release --snapshot --clean --skip=publish
```

## License

MIT

## Contributing

Contributions are welcome! Please open an issue or submit a pull request.
