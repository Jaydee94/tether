# Branch Protection Rules

Configure these rules on `main` via **GitHub → Settings → Branches → Add rule**.

## Required Settings

| Setting | Value |
|---|---|
| Branch name pattern | `main` |
| Require a pull request before merging | ✅ |
| Required approving reviews | 1 |
| Dismiss stale reviews on new pushes | ✅ |
| Require status checks to pass before merging | ✅ |
| Require branches to be up to date before merging | ✅ |
| Required status checks | `Lint and Test` (from ci.yml) |
| Do not allow bypassing the above settings | ✅ |
| Restrict who can push to matching branches | repo admins / maintainer team |

## Notes

- The `CI` workflow (`ci.yml`) runs `go vet`, `golangci-lint`, and `go test -race` on every PR and push to `main`.
- The `Release` workflow (`release.yml`) triggers on `v*` tag pushes and:
  - Builds and pushes multi-arch Docker images (`linux/amd64`, `linux/arm64`) to `ghcr.io` for the `operator` and `proxy` components.
  - Builds `tetherctl` binaries for Linux, macOS, and Windows (amd64 + arm64) and attaches them as GitHub Release assets.
- **Dockerfiles required:** Before tagging a release, add `cmd/operator/Dockerfile` and `cmd/proxy/Dockerfile` to the repo. The release workflow expects them at those paths.
