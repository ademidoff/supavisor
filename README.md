# Go supervisord

A process supervisord daemon written in Go, inspired by supervisord, that efficiently manages child processes with dependency support, config-based lifecycle management, and log rotation.

## Features

- **Process Management**: Start, stop, restart, and monitor child processes
- **Dependency Management**: Launch processes based on whether other processes are running
- **Configuration-Based**: Configure process lifetime and behavior via INI-style config files
- **Log Rotation**: Automatic log file rotation based on file size with configurable retention periods
- **CLI Tool**: Command-line interface for managing processes
- **Process States**: Track process states (STOPPED, STARTING, RUNNING, BACKOFF, STOPPING, EXITED, FATAL)
- **Auto-restart Policies**: Configure restart behavior (always, never, unexpected)

## Installation

```bash
git clone https://github.com/ademidoff/go-supervisord
cd go-supervisord
make build
```

## Quick Start

1. Create a configuration file (e.g., `supervisord.conf`):

```ini
[supervisord]
logfile=/var/log/supervisord/supervisord.log
pidfile=/var/run/supervisord.pid
socket=/tmp/go-supervisord.sock

[program:webapp]
command=/usr/bin/python app.py
directory=/opt/webapp
autostart=true
autorestart=always
startsecs=10
depends_on=database
stdout_logfile=/var/log/webapp/stdout.log
stdout_logfile_maxbytes=10MB
stdout_logfile_backups=5
stdout_logfile_maxage=7

[program:database]
command=/usr/bin/postgres
autostart=true
autorestart=unexpected
stdout_logfile=/var/log/database/stdout.log
stdout_logfile_maxbytes=50MB
stdout_logfile_backups=10
```

2. Start the supervisord daemon:

```bash
./supervisord -c supervisord.conf
```

3. Use the CLI tool to manage processes:

```bash
# Check status
./supervisorctl status

# Start a process
./supervisorctl start webapp

# Stop a process
./supervisorctl stop webapp

# Restart a process
./supervisorctl restart webapp

# Reload configuration
./supervisorctl reload

# Shutdown supervisord
./supervisorctl shutdown
```

## Configuration

### [supervisord] Section

- `logfile`: Path to supervisord's own log file
- `pidfile`: Path to PID file
- `socket`: Path to Unix domain socket for CLI communication

### [program:name] Section

Each program section defines a process to manage:

- `command`: Command to run (required)
- `directory`: Working directory for the process
- `autostart`: Start process automatically on supervisord startup (default: true)
- `autorestart`: Restart policy - `always`, `never`, or `unexpected` (default: unexpected)
- `startsecs`: Seconds to wait before considering start successful (default: 1)
- `startretries`: Number of retries before giving up (default: 3)
- `depends_on`: Comma-separated list of program names that must be running first
- `stop_on_dependency_failure`: Stop this process if a dependency stops (default: false)
- `stdout_logfile`: Path to stdout log file
- `stderr_logfile`: Path to stderr log file
- `stdout_logfile_maxbytes`: Maximum size before rotation (supports KB, MB, GB suffixes, default: 50MB)
- `stdout_logfile_backups`: Number of rotated logs to keep (default: 10)
- `stdout_logfile_maxage`: Days to keep rotated logs (0 = no limit, default: 0)
- `stderr_logfile_maxbytes`: Maximum size before rotation (default: 50MB)
- `stderr_logfile_backups`: Number of rotated logs to keep (default: 10)
- `stderr_logfile_maxage`: Days to keep rotated logs (default: 0)
- `environment`: Comma-separated environment variables (e.g., `KEY1=value1,KEY2=value2,KEY3="value with, comma"`). Values containing commas should be quoted.
- `user`: User to run process as (not implemented yet)
- `priority`: Startup priority (lower numbers start first, default: 999)

## Process States

- `STOPPED`: Process is stopped
- `STARTING`: Process is starting up
- `RUNNING`: Process is running
- `BACKOFF`: Process failed to start, waiting before retry
- `STOPPING`: Process is being stopped
- `EXITED`: Process exited
- `FATAL`: Process failed to start after all retries

## Dependency Management

Processes can depend on other processes using the `depends_on` option. The supervisord will:

1. Start processes in dependency order (topological sort)
2. Ensure dependencies are running before starting dependent processes
3. Optionally stop dependent processes when a dependency fails (if `stop_on_dependency_failure=true`)

Circular dependencies are detected and rejected during configuration validation.

## Log Rotation

Log files are automatically rotated when they exceed the configured maximum size. The rotation strategy:

1. Existing backups are rotated (`.1` -> `.2`, `.2` -> `.3`, etc.)
2. Current log is moved to `.1`
3. A new log file is created
4. Old backups beyond the configured count are removed
5. Logs older than `maxage` days are automatically deleted

## Examples

### Basic Process

```ini
[program:myapp]
command=/usr/bin/myapp
autostart=true
autorestart=always
stdout_logfile=/var/log/myapp/stdout.log
```

### Process with Dependencies

```ini
[program:database]
command=/usr/bin/postgres
autostart=true
autorestart=unexpected

[program:webapp]
command=/usr/bin/python app.py
depends_on=database
autostart=true
autorestart=always
stop_on_dependency_failure=true
```

### Process with Log Rotation

```ini
[program:worker]
command=/usr/bin/worker
stdout_logfile=/var/log/worker/stdout.log
stdout_logfile_maxbytes=100MB
stdout_logfile_backups=10
stdout_logfile_maxage=30
```

### Process with Environment Variables

```ini
[program:myapp]
command=/usr/bin/myapp
environment=APP_ENV=production,APP_PORT=8080,PATH="/usr/bin:/usr/local/bin:/opt/bin",DEBUG=false
```

Note: Values containing commas should be quoted. For example: `PATH="/usr/bin:/usr/local/bin,/opt/bin"`

## Architecture

- `cmd/supervisord`: Main daemon entry point
- `cmd/supervisorctl`: CLI tool for managing processes
- `internal/config`: Configuration file parsing
- `internal/process`: Process lifecycle management
- `internal/dependency`: Dependency resolution engine
- `internal/logrotate`: Log rotation and retention
- `internal/supervisord`: Core supervisord daemon
- `pkg/api`: Public API types for IPC communication

## License

MIT

## Contributing

Contributions are welcome! Please open an issue or submit a pull request.

