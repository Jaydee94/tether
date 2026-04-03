# Tether — Kubernetes Privileged Access Management

Tether provides time-limited, audited, privileged Kubernetes access. Engineers request a `TetherLease` CRD, the operator creates a `ClusterRoleBinding` that auto-expires, and all `kubectl exec` / `kubectl logs` traffic is recorded in Asciinema format for audit purposes.

## Architecture

```
 ┌─────────────┐   kubectl   ┌──────────────────┐   forward   ┌─────────────────┐
 │  Engineer   │────────────▶│  Tether Proxy    │────────────▶│  k8s API Server │
 │  (tetherctl)│             │  :8443           │             │                 │
 └─────────────┘             └──────────────────┘             └─────────────────┘
       │                              │ record exec/log
       │ create TetherLease           ▼
       │                     ┌──────────────────┐
       ▼                     │  Audit Engine    │
 ┌─────────────┐             │  (local/S3/ES)   │
 │  k8s API    │             └──────────────────┘
 │  (CRD)      │
 └─────────────┘
       │ watch
       ▼
 ┌─────────────────────┐
 │  Tether Operator    │
 │  creates/deletes    │
 │  ClusterRoleBinding │
 └─────────────────────┘
```

## Components

| Component | Description |
|-----------|-------------|
| **TetherLease CRD** | Cluster-scoped resource tracking access grants with expiry |
| **Tether Operator** | Watches `TetherLease` objects; creates/deletes `ClusterRoleBindings`; generates cryptographic session tokens stored as k8s Secrets; enforces expiry |
| **Tether Proxy** | Reverse proxy in front of the API server; validates tokens against live k8s Secrets and TetherLease status, records all API sessions |
| **tetherctl** | CLI for requesting access, listing/revoking leases, configuring kubeconfig (auto-fetches token), and playing back recordings |
| **Audit Engine** | Pluggable storage: local filesystem, AWS S3, or Elasticsearch |

## TetherLease CRD Example

```yaml
apiVersion: tether.dev/v1alpha1
kind: TetherLease
metadata:
  name: alice-incident-42
spec:
  user: alice
  role: cluster-admin
  duration: 30m
  reason: "Investigating production outage #42"
```

The operator activates the lease by creating a `ClusterRoleBinding` and schedules automatic cleanup at expiry. The lease transitions through phases: `Pending → Active → Expired` (or `Revoked`).

## Directory Structure

```
.
├── cmd/
│   ├── operator/      # Operator binary entry point
│   ├── proxy/         # Proxy binary entry point
│   └── tetherctl/     # CLI binary entry point
├── pkg/
│   ├── api/v1alpha1/  # TetherLease CRD types, scheme registration, deepcopy
│   ├── operator/      # Reconciler controller logic
│   ├── proxy/         # Reverse proxy + session recorder
│   └── audit/         # Storage backends (local, S3, Elasticsearch)
└── config/
    └── crd/           # CRD YAML manifest
```

## Getting Started

### Local Development with Kind (recommended)

The quickest way to spin up a fully working Tether environment locally is the
included setup script.  It requires [kind](https://kind.sigs.k8s.io/),
[kubectl](https://kubernetes.io/docs/tasks/tools/), [docker](https://docs.docker.com/get-docker/),
and [go](https://go.dev/doc/install).

```bash
# Bootstrap a Kind cluster, install the CRD, build binaries,
# and start the operator + proxy in the background.
make local-setup
# or: ./scripts/local-setup.sh

# Tear everything down when you're done.
make local-teardown
# or: ./scripts/local-setup.sh --teardown
```

The script will:
1. Create a Kind cluster called `tether-dev` (if one already exists with that name, it is reused — run `make local-teardown` first for a clean start)
2. Install the `TetherLease` CRD
3. Build the `operator`, `proxy`, and `tetherctl` binaries into `./bin/`
4. Start the operator in the background (logs → `/tmp/tether-pids/operator.log`)
5. Start the proxy on `:8443` in the background (logs → `/tmp/tether-pids/proxy.log`)
6. Create a demo `TetherLease` so you can see the full lifecycle immediately

After setup, follow the printed instructions to request leases and route
`kubectl` through the proxy for a recorded session.

> **Environment variables** you can override:
> | Variable | Default | Description |
> |---|---|---|
> | `TETHER_CLUSTER` | `tether-dev` | Kind cluster name |
> | `TETHER_PROXY_PORT` | `8443` | Proxy listen port |
> | `TETHER_AUDIT_DIR` | `/tmp/tether-audit` | Local audit directory |
> | `TETHER_TOKEN` | `tether-dev-token` | Static dev token for the proxy |

---

### Manual Setup

### Prerequisites

- Go 1.24+
- A Kubernetes cluster (or `kind`/`minikube` for local development)
- `kubectl` configured

### Build

```bash
make build
# Binaries are placed in ./bin/
```

### Install the CRD

```bash
make install
# or: kubectl apply -f config/crd/tetherlease.yaml
```

### Run the Operator

```bash
# Locally against the current kubeconfig context
make run-operator
# or: go run ./cmd/operator/...
```

### Run the Proxy

```bash
# Development mode (HTTP, no TLS)
go run ./cmd/proxy/... --target https://$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}') --tls-skip-verify

# Production mode (with TLS)
go run ./cmd/proxy/... --tls-cert /path/to/tls.crt --tls-key /path/to/tls.key
```

### Request Access

```bash
# Request cluster-admin for 30 minutes
tetherctl request --role cluster-admin --for 30m --reason "investigating outage"

# List all TetherLeases and their status
tetherctl list

# Activate session (auto-fetches token from k8s Secret; configures kubeconfig to route through proxy)
tetherctl login --lease alice-1234567890

# Normal kubectl commands are now proxied and all requests are recorded
kubectl get pods -A
kubectl logs -n kube-system deployment/coredns

# Revoke access early (operator deletes CRB and token Secret immediately)
tetherctl revoke --lease alice-1234567890

# Play back a recorded session
tetherctl playback --lease alice-1234567890
```

## Asciinema Recording Format

Sessions are stored as [Asciinema v2](https://docs.asciinema.org/manual/asciicast/v2/) `.cast` files:

```
{"version":2,"width":220,"height":50,"timestamp":1704067200,"title":"tether/sess-id GET /api/v1/namespaces/default/pods/web/exec"}
[0.123,"o","$ kubectl exec -it web -- /bin/bash\r\n"]
[1.456,"o","root@web:/# "]
```

Each file is named `<sessionID>.cast`. Play back with:
```bash
tetherctl playback --lease <session-id>
# or natively with: asciinema play <session>.cast
```

## Security Considerations

- **Least privilege**: The operator only creates bindings for the explicitly requested `ClusterRole`.
- **Time-limited**: All access automatically expires; the operator enforces expiry via requeueing.
- **Finalizer-based cleanup**: `ClusterRoleBindings` and session-token Secrets are always cleaned up even if the `TetherLease` is deleted.
- **Cryptographic session tokens**: The operator generates a 32-byte cryptographically random token (base64-URL-encoded) per lease, stored as a k8s Secret in the `tether-system` namespace. The token is deleted when the lease expires or is revoked.
- **Live token validation**: The proxy validates each request against the live k8s Secret *and* the current TetherLease phase — revoking a lease immediately invalidates all subsequent proxy requests.
- **Audit trail**: All API requests through the proxy are recorded in Asciinema v2 format before being forwarded.
- **TLS**: The proxy supports TLS termination; `--tls-skip-verify` is for development only.
- **Token namespace**: Session-token Secrets are isolated in `tether-system`; restrict RBAC access to this namespace in production.

## Development Commands

```bash
make build          # Build all binaries
make test           # Run unit tests
make test-race      # Run tests with race detector
make vet            # Run go vet
make fmt            # Format source code
make lint           # Run golangci-lint (must be installed)
make tidy           # Run go mod tidy
make install        # Apply CRD to current cluster
make local-setup    # Bootstrap Kind cluster + start operator & proxy
make local-teardown # Stop all components and delete the Kind cluster
make clean          # Remove build artifacts
```