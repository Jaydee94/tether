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
| **Tether Operator** | Watches `TetherLease` objects; creates/deletes `ClusterRoleBindings`; enforces expiry |
| **Tether Proxy** | Reverse proxy in front of the API server; validates tokens, records `exec`/`log` sessions |
| **tetherctl** | CLI for requesting access, configuring kubeconfig, and playing back recordings |
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

# Activate session (configures kubeconfig to route through proxy)
tetherctl login --lease alice-1234567890

# Normal kubectl commands are now proxied and recorded
kubectl get pods -A

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
- **Finalizer-based cleanup**: `ClusterRoleBindings` are always cleaned up even if the `TetherLease` is deleted.
- **Audit trail**: All `exec` and `log` sessions are recorded before being forwarded.
- **Token validation**: The proxy rejects requests without a valid `X-Tether-Token`; in production replace `StaticValidator` with a Kubernetes Secret-backed token store.
- **TLS**: The proxy supports TLS termination; `--tls-skip-verify` is for development only.

## Development Commands

```bash
make build        # Build all binaries
make test         # Run unit tests
make test-race    # Run tests with race detector
make vet          # Run go vet
make fmt          # Format source code
make lint         # Run golangci-lint (must be installed)
make tidy         # Run go mod tidy
make install      # Apply CRD to current cluster
make clean        # Remove build artifacts
```