# Supavisor Development Guidelines

**supavisor** is a process supervisor daemon that manages the configuration of processes and exposes an API for interacting with them. The API is also consumed by [sctl tool](https://github.com/ademidoff/supavisor/tree/main/cmd/sctl).

## Architecture Patterns

supavisor manages processes using the following pattern:
- Processes are configured in a configuration file
- Processes are started and stopped using the sctl tool
- Processes are monitored and restarted if they exit
- Processes are logged to a log file
- Processes are rotated if they exceed a certain size
- Processes are stopped if they are killed

## Testing Conventions

### Unit Tests
- Use `testify/assert` and `testify/require`
- Mock generation via `mockery` (config in `.mockery.yaml`)
- Run tests: `make test`
- Cover all code with tests

## Common Patterns

### Do
- Prefer modern Go idioms (context, error wrapping)
- Prefer modern slice helpers (e.g., `slices.Contains`), range loops
- Use `any` instead of `interface{}`
- Always use `make` to create arrays and maps

### Don't
- Don't edit generated files manually
- Don't create subshells in Makefiles without explicit reason
- Don't commit test binaries or test artifacts (add to `.gitignore` if needed)
- Don't comment on every single line of code unnecessarily, only where clarity is needed
- Don't inline comments (i.e. `code // comment`), always put comments on separate lines

### Error Handling
- Wrap errors with context: `fmt.Errorf("descriptive context: %w", err)`
- Return early on errors to avoid deep nesting
- Use `errors.Is()` and `errors.As()` for error type checking
- Use standard `errors` package

### Logging
- Use structured logging with `slog`
- Format: `slog.Info("message", "key", value)`

## RESTful conventions
- Use RESTful conventions (GET/POST/PUT/DELETE with resource paths)
- Use custom endpoints only when necessary (e.g., actions)

## Documentation
- Always document major behavior in README.md
- Have all configuration parameters properly documented
- Document the edge cases and special behavior
