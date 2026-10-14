# llm-pico Go Port

This branch contains a complete port of llm-pico from Python to Go.

## Features

- Single binary implementation
- Compile-time provider registry (no hot-loading of custom providers)
- Uses `modernc.org/sqlite` as SQLite driver (cgo-free)
- Automatic schema migrations on database open
- Atomic configuration hot-reload
- Goroutine-per-request with proper synchronization
- Comprehensive test coverage

## Building

```bash
# Build for current platform
go build -o llm-pico ./cmd/llm-pico

# Build for specific platform
GOOS=linux GOARCH=amd64 go build -o llm-pico-linux-amd64 ./cmd/llm-pico
```

## Running

```bash
./llm-pico --db ./llm-pico.db --port 4000
```

See `./llm-pico --help` for all options.

## Testing

```bash
go test ./...
```

## Release Process

Releases are automated via GitHub Actions:
- Push a version tag (e.g., `v1.0.0`) to trigger the workflow
- Workflow builds binaries for multiple platforms
- Creates a GitHub release with the binaries as assets
