# git-fat task runner.

# Native build.
build:
    go build ./cmd/git-fat

# Unit and end-to-end tests (no S3 store needed).
test:
    go test ./...

# Unit tests plus the S3 integration tests; needs rustfs on PATH (nix develop).
integration:
    go test -tags integration ./...

# Lint both build configurations.
lint:
    golangci-lint run ./...
    golangci-lint run --build-tags integration ./...

# bump the version in the tracked files (patch|minor|major|...)
# --no-prompt: versionbump's confirmation loops forever when stdin isn't a TTY.
bump part="patch":
    go tool versionbump {{part}} --no-prompt
