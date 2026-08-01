# Supavisor

A process supervisor daemon written in Go, that is largely inspired by supervisord. It efficiently manages child processes with dependency support, config-based lifecycle management, and log rotation.

## Features

- **Process Management**: Start, stop, restart, and monitor child processes
- **Dependency Management**: Launch processes based on whether other processes are running
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

```bash
git clone https://github.com/ademidoff/supavisor
cd supavisor
make build
```

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

3. Use the CLI tool to manage processes:

```bash
# Check status
./sctl status

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

### sctl

```bash
./sctl [options] <command> [process-name]
```

Options must be given before the command; `sctl -s /run/supavisor.sock status`
works, `sctl status -s /run/supavisor.sock` is rejected.

Options:
- `-s, -socket <path>`: Path to supavisor socket (default: `/tmp/supavisor.sock`)

Commands:
- `status`: Show status of all processes
- `start <name>`: Start a specific process
- `stop <name>`: Stop a specific process
- `restart <name>`: Restart a specific process
- `reload`: Reload configuration
- `shutdown`: Shutdown supavisor daemon

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

### supavisor section

- `logfile`: Path to supavisor's own log file (optional)
  - When specified, all logs are written to this file only (no console output)
  - When not specified, logs are written to stdout (if running in a terminal)
  - Can be overridden with the `-logfile` command-line flag
- `pidfile`: Path to PID file (default: `/var/run/supavisor.pid`)
- `socket`: Path to Unix domain socket for CLI communication (default: `/tmp/supavisor.sock`)
- `log_format`: Log format - `text` (default) or `json`
- `log_level`: Log level - `debug`, `info` (default), `warn`, or `error`

### programs section

Each program is defined under `programs` with its name as the key:

- `command`: Command to run (required)
- `directory`: Working directory for the process
- `autostart`: Start process automatically on supavisor startup (default: true)
- `autorestart`: Restart policy - `always`, `never`, or `unexpected` (default: unexpected)
- `startsecs`: Seconds to wait before considering start successful (default: 1)
- `max_restarts`: Maximum number of *consecutive* restarts before giving up (default: 3).
  See [Restart behavior](#restart-behavior) for how the counter is reset.
- `depends_on`: List of program names that must be running first
- `stdout_logfile`: Path to stdout log file. If omitted, the process's stdout is
  discarded (connected to `/dev/null`).
- `stderr_logfile`: Path to stderr log file. If omitted, the process's stderr is
  discarded (connected to `/dev/null`).
- `stdout_logfile_maxbytes`: Maximum size before rotation (supports KB, MB, GB suffixes, default: 50MB)
- `stdout_logfile_backups`: Number of rotated logs to keep (default: 10)
- `stdout_logfile_maxage`: Days to keep rotated logs (0 = no limit, default: 0)
- `stderr_logfile_maxbytes`: Maximum size before rotation (default: 50MB)
- `stderr_logfile_backups`: Number of rotated logs to keep (default: 10)
- `stderr_logfile_maxage`: Days to keep rotated logs (default: 0)
- `environment`: Map of environment variables (e.g., `APP_ENV: production`)
- `user`: User to run process as (not implemented yet)
- `priority`: Startup priority (lower numbers start first, default: 999)

## Process States

- `STOPPED`: Process was stopped by supavisor (e.g., via `sctl stop`)
- `STARTING`: Process is starting up
- `RUNNING`: Process is running normally
- `BACKOFF`: Process failed to start, waiting before retry
- `STOPPING`: Process is being stopped (transitional state)
- `EXITED`: Process exited on its own (completed normally or crashed)
- `FATAL`: Process failed to start after all retries

`sctl stop` always ends in `STOPPED`, including for a process that had already
exited or reached `FATAL` on its own. Stopping a process that is waiting out a
restart backoff cancels that pending restart.

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
that backgrounds work and returns does not leak it. If the group does not exit on
`SIGINT` within the shutdown timeout, the whole group is sent `SIGKILL`.

One consequence: because managed processes are in their own groups, `Ctrl+C` in a
terminal running supavisor in the foreground reaches supavisor only. Supavisor
then stops its processes itself, in its own order.

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

Processes can depend on other processes using the `depends_on` option. The supavisor will:

1. Start processes in dependency order (topological sort)
2. Ensure dependencies are running before starting dependent processes
3. When a dependency stops (crashes or exits), it is restarted according to its `autorestart` policy. Dependent processes continue running.

Circular dependencies are detected and rejected during configuration validation.

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
- Because output flows through supavisor, a process that logs faster than the disk
  can absorb will eventually block on write, and a process that outlives a
  supavisor crash will see its output descriptor close.

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

## License

MIT

## Contributing

Contributions are welcome! Please open an issue or submit a pull request.
