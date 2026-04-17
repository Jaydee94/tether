# Contributing to Tether

Thank you for your interest in contributing to Tether! This document outlines the development workflow, testing practices, and code quality expectations.

## Development Setup

### Prerequisites

- Go 1.24+
- Kubernetes cluster (or `kind` for local development)
- `kubectl` configured
- `docker` for building container images

### Quick Start

```bash
# Clone the repository
git clone https://github.com/Jaydee94/tether.git
cd tether

# Build all binaries
make build

# Run tests
make test

# Start a local Kind cluster with Tether components
make local-setup

# Teardown when done
make local-teardown
```

## Testing

### Unit Tests

Run the full test suite:

```bash
make test
```

Run tests with race condition detection (recommended for concurrent code):

```bash
make test-race
```

### Integration Testing

After running `make local-setup`, you can manually test the full workflow:

```bash
# Request a lease
./bin/tetherctl request --role cluster-admin --for 30m --reason "testing"

# Activate the session
./bin/tetherctl login --lease <lease-name> --token "$TETHER_TOKEN"

# Verify proxied access works
kubectl get pods -A

# Playback the recorded session
./bin/tetherctl playback --lease <lease-name> --audit-dir /tmp/tether-audit
```

## Code Quality

### Linting and Formatting

- **Format code:** `make fmt`
- **Run Go vet:** `make vet`
- **Run golangci-lint:** `make lint` (requires `golangci-lint` installed separately)

### Vendoring

Keep dependencies up to date:

```bash
make tidy
```

## Branch Protection and PR Workflow

### Branch Naming Conventions

Use descriptive branch names with a prefix:

- `feature/` for new features (e.g., `feature/s3-audit-backend`)
- `fix/` for bug fixes (e.g., `fix/token-validation-race`)
- `docs/` for documentation (e.g., `docs/deployment-guide`)
- `refactor/` for code refactoring (e.g., `refactor/proxy-handlers`)

### Pull Requests

1. **Create a feature branch** from `main`
2. **Make your changes** and ensure tests pass locally (`make test`)
3. **Format code** (`make fmt`, `make vet`)
4. **Write a concise PR title** (e.g., "Add S3 audit backend support")
5. **Fill in the PR description:**
   - What problem does this solve?
   - How were changes tested?
   - Any breaking changes?
6. **Include QA steps** if the change affects user-facing behavior
7. **Link related issues** (e.g., "Closes #42")

### Branch Protection Rules

The `main` branch is protected. All PRs must:

- Pass automated tests (`make test`)
- Pass linting (`make lint`)
- Have at least one approval from a code owner
- Be up to date with `main` (no merge conflicts)

## Reporting Issues

When opening an issue, please include:

- A clear description of the problem
- Steps to reproduce (if applicable)
- Expected vs. actual behavior
- Tether version and Kubernetes cluster version
- Relevant logs or error messages

## Code Style

- Follow standard Go conventions (effective Go)
- Use meaningful variable and function names
- Add comments for exported functions and complex logic
- Keep functions small and testable
- Prefer explicit over implicit error handling

## Commit Messages

Write clear, concise commit messages:

- Use imperative mood ("Add feature" not "Added feature")
- Keep the first line under 50 characters
- Add a blank line before the body (if present)
- Reference related issues (e.g., "Fixes #42")

Example:

```
Fix token validation race condition

Previously, concurrent token lookups could cause stale cache
entries to be used. Add a read-write lock to ensure consistency.

Fixes #42
```

## Security

If you discover a security vulnerability, please email security@example.com with:

- Description of the vulnerability
- Affected versions
- Proof of concept (if applicable)

Do not open a public issue for security vulnerabilities. See [SECURITY.md](SECURITY.md) for more details.

## License

By contributing to Tether, you agree that your contributions will be licensed under the same license as the project.

## Questions?

Open an issue on GitHub or check the [README.md](README.md) for additional resources.
