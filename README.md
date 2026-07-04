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
- Supavisor prevents multiple instances from running simultaneously by checking PID and socket files
- If you find stale PID or socket files after a crash, remove them manually before starting:
  ```bash
  rm /tmp/supavisor.pid /tmp/supavisor.sock
  ```

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
./sctl <command> [process-name]
```

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
- `max_restarts`: Maximum number of restarts before giving up (default: 3)
- `depends_on`: List of program names that must be running first
- `stdout_logfile`: Path to stdout log file
- `stderr_logfile`: Path to stderr log file
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

## Dependency Management

Processes can depend on other processes using the `depends_on` option. The supavisor will:

1. Start processes in dependency order (topological sort)
2. Ensure dependencies are running before starting dependent processes
3. When a dependency stops (crashes or exits), it is restarted according to its `autorestart` policy. Dependent processes continue running.

Circular dependencies are detected and rejected during configuration validation.

## Log Rotation

Log files are automatically rotated when they exceed the configured maximum size. The rotation strategy:

1. Existing backups are rotated (`.1` -> `.2`, `.2` -> `.3`, etc.)
2. Current log is moved to `.1`
3. A new file is created
4. Old backups beyond the configured count are removed
5. Logs older than `maxage` days are automatically deleted

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
