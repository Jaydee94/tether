<p align="center">
  <img src="docs/assets/banner.svg" alt="Tether — Kubernetes Privileged Access Management" width="520"/>
</p>

<p align="center">
  <strong>Time-limited, audited, privileged Kubernetes access — zero standing permissions.</strong>
</p>

<p align="center">
  <a href="#architecture">Architecture</a> ·
  <a href="#table-of-contents">Docs</a> ·
  <a href="#security-considerations">Security</a> ·
  <a href="#getting-started">Getting Started</a>
</p>

---

Tether provides time-limited, audited, privileged Kubernetes access. Engineers request a `TetherLease` CRD, the operator creates a `ClusterRoleBinding` that auto-expires, and all `kubectl exec` / `kubectl logs` traffic is recorded in Asciinema format for audit purposes.

---

## Table of Contents

- [Architecture](#architecture)
- [Components](#components)
- [TetherLease CRD](#tetherlease-crd)
- [Lease Lifecycle](#lease-lifecycle-and-status-runbook)
- [Quick Start — Local Dev with Kind](#quick-start--local-dev-with-kind)
- [Production Deployment](#production-deployment)
  - [Prerequisites](#prerequisites-1)
  - [RBAC for the Operator](#rbac-for-the-operator)
  - [Deploying the Operator](#deploying-the-operator)
  - [Deploying the Proxy](#deploying-the-proxy)
  - [TLS Certificate Provisioning](#tls-certificate-provisioning)
  - [Configuring the Audit Backend](#configuring-the-audit-backend)
- [tetherctl Reference](#tetherctl-reference)
- [Audit Backend Reference](#audit-backend-reference)
- [Security Considerations](#security-considerations)
- [Development Commands](#development-commands)
- [Observability — Prometheus Metrics](#observability--prometheus-metrics)
- [Changelog](#changelog)

---

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

**Flow summary:**
1. Engineer runs `tetherctl request` → creates a cluster-scoped `TetherLease` resource.
2. The **Tether Operator** watches `TetherLease` objects and creates a matching `ClusterRoleBinding` when a lease is `Active`. It enforces expiry by requeueing at the lease deadline and deletes the binding when the lease expires or is revoked.
3. The **Tether Proxy** sits in front of the real Kubernetes API server. It validates the `X-Tether-Token` header (populated automatically by `tetherctl login`), forwards the request upstream, and streams a copy of any `exec` or `log` session to the configured audit backend.
4. The **Audit Engine** persists session recordings as Asciinema v2 `.cast` files to local disk, AWS S3, or Elasticsearch.

---

## Components

| Component | Description |
|-----------|-------------|
| **TetherLease CRD** | Cluster-scoped resource tracking access grants with expiry |
| **Tether Operator** | Watches `TetherLease` objects; creates/deletes `ClusterRoleBindings`; enforces expiry |
| **Tether Proxy** | Reverse proxy in front of the API server; validates tokens, records `exec`/`log` sessions |
| **tetherctl** | CLI for requesting access, configuring kubeconfig, approving/denying leases, and playing back recordings |
| **Audit Engine** | Pluggable storage: local filesystem, AWS S3, or Elasticsearch |

---

## TetherLease CRD

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

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `spec.user` | string | yes | Identity of the engineer requesting access |
| `spec.role` | string | yes | Name of an existing `ClusterRole` to bind |
| `spec.duration` | string | yes | Go duration string, e.g. `30m`, `2h` |
| `spec.reason` | string | no | Human-readable justification (strongly recommended) |

The operator activates the lease by creating a `ClusterRoleBinding` (`tether-lease-<name>`) and schedules automatic cleanup at expiry. The lease transitions through phases: `Pending → Active → Expired` (or `Revoked`).

---

## Lease Lifecycle and Status Runbook

`TetherLease.status` is the source of truth for lifecycle state.

### Conditions, reasons, and typical messages

| State | Ready | Reason | Typical message |
|---|---|---|---|
| Lease activated | `True` | `Activated` | `lease activated` |
| Invalid duration | `False` | `InvalidDuration` | `invalid duration "<value>": <parse error>` |
| Activation failed | `False` | `ActivationFailed` | Kubernetes API error text |
| Lease expired | `False` | `Expired` | `lease expired` |
| Lease revoked | `False` | `Revoked` | `lease revoked` |

### Troubleshooting

```bash
# Full status including phase/conditions/observedGeneration
kubectl get tetherlease <lease-name> -o yaml

# Quick summary (conditions + recent events)
kubectl describe tetherlease <lease-name>

# Events directly (cluster-scoped object)
kubectl get events -A \
  --field-selector involvedObject.kind=TetherLease,involvedObject.name=<lease-name> \
  --sort-by=.lastTimestamp

# Verify ClusterRoleBinding state
kubectl get clusterrolebinding tether-lease-<lease-name> -o yaml

# Operator logs
kubectl logs -n tether-system deploy/tether-operator
```

`status.observedGeneration` tracks which spec generation has been fully reconciled. If `.metadata.generation` is newer, the latest change is still in-flight.

---

## Quick Start — Local Dev with Kind

**Prerequisites:** [kind](https://kind.sigs.k8s.io/), [kubectl](https://kubernetes.io/docs/tasks/tools/), [docker](https://docs.docker.com/get-docker/), [go](https://go.dev/doc/install) 1.24+.

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
1. Create a Kind cluster called `tether-dev` (reused if it already exists)
2. Install the `TetherLease` CRD
3. Build `operator`, `proxy`, and `tetherctl` binaries into `./bin/`
4. Start the operator in the background (logs → `/tmp/tether-pids/operator.log`)
5. Start the proxy on `:8443` in the background (logs → `/tmp/tether-pids/proxy.log`)
6. Create a demo `TetherLease` so you can see the full lifecycle immediately

**Environment variables you can override:**

| Variable | Default | Description |
|---|---|---|
| `TETHER_CLUSTER` | `tether-dev` | Kind cluster name |
| `TETHER_PROXY_PORT` | `8443` | Proxy listen port |
| `TETHER_AUDIT_DIR` | `/tmp/tether-audit` | Local audit directory |
| `TETHER_TOKEN` | `tether-dev-token` | Static dev token for the proxy |

**After setup:**

```bash
# Request cluster-admin for 30 minutes
./bin/tetherctl request --role cluster-admin --for 30m --reason "investigating outage"

# Activate session (routes kubectl through proxy + sets token)
./bin/tetherctl login --lease <lease-name> --token "$TETHER_TOKEN"

# Normal kubectl commands are now proxied and recorded
kubectl get pods -A

# Play back a recorded session
./bin/tetherctl playback --lease <lease-name> --audit-dir /tmp/tether-audit
```

---

## Production Deployment

### Prerequisites

- Kubernetes 1.27+ cluster
- `kubectl` with cluster-admin access for initial setup
- [cert-manager](https://cert-manager.io/) v1.0+ for automatic TLS (recommended)
- Go 1.24+ if building from source; or use prebuilt images from `ghcr.io/jaydee94/tether`
- AWS credentials / Elasticsearch endpoint if using those audit backends

### Directory Structure

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
    ├── crd/           # CRD YAML manifest
    └── tls/           # Example cert-manager Certificate + Issuer
```

### Install the CRD

```bash
kubectl apply -f config/crd/tetherlease.yaml
```

### RBAC for the Operator

The operator needs permission to manage `ClusterRoleBindings` and to update `TetherLease` status subresources. Create a `ClusterRole` and `ClusterRoleBinding` for the operator's service account:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: tether-operator
  namespace: tether-system
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: tether-operator
rules:
  - apiGroups: ["tether.dev"]
    resources: ["tetherleases"]
    verbs: ["get", "list", "watch", "update", "patch"]
  - apiGroups: ["tether.dev"]
    resources: ["tetherleases/status", "tetherleases/finalizers"]
    verbs: ["get", "update", "patch"]
  - apiGroups: ["rbac.authorization.k8s.io"]
    resources: ["clusterrolebindings"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  # Leader election
  - apiGroups: ["coordination.k8s.io"]
    resources: ["leases"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  - apiGroups: [""]
    resources: ["events"]
    verbs: ["create", "patch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: tether-operator
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: tether-operator
subjects:
  - kind: ServiceAccount
    name: tether-operator
    namespace: tether-system
```

### Deploying the Operator

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: tether-operator
  namespace: tether-system
spec:
  replicas: 1
  selector:
    matchLabels:
      app: tether-operator
  template:
    metadata:
      labels:
        app: tether-operator
    spec:
      serviceAccountName: tether-operator
      containers:
        - name: operator
          image: ghcr.io/jaydee94/tether/operator:latest
          args:
            - --metrics-bind-address=:8080
            - --health-probe-bind-address=:8081
            - --leader-elect=true
          ports:
            - name: metrics
              containerPort: 8080
            - name: healthz
              containerPort: 8081
          livenessProbe:
            httpGet:
              path: /healthz
              port: 8081
            initialDelaySeconds: 15
            periodSeconds: 20
          readinessProbe:
            httpGet:
              path: /readyz
              port: 8081
            initialDelaySeconds: 5
            periodSeconds: 10
          resources:
            limits:
              cpu: 200m
              memory: 128Mi
            requests:
              cpu: 50m
              memory: 64Mi
```

| Operator Flag | Default | Description |
|---|---|---|
| `--metrics-bind-address` | `:8080` | Prometheus metrics endpoint |
| `--health-probe-bind-address` | `:8081` | Liveness and readiness probe endpoint |
| `--leader-elect` | `false` | Enable leader election (required for HA/multi-replica deployments) |

### Deploying the Proxy

The proxy must be reachable by engineers' `kubectl` clients and must be able to forward requests to the real Kubernetes API server.

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: tether-proxy
  namespace: tether-system
spec:
  replicas: 2
  selector:
    matchLabels:
      app: tether-proxy
  template:
    metadata:
      labels:
        app: tether-proxy
    spec:
      containers:
        - name: proxy
          image: ghcr.io/jaydee94/tether/proxy:latest
          args:
            - --listen=:8443
            - --target=https://kubernetes.default.svc
            - --tls-cert=/tls/tls.crt
            - --tls-key=/tls/tls.key
            - --audit-dir=/var/tether/audit
            - --audit-max-file-size=104857600   # 100 MiB per recording file
          env:
            - name: TETHER_TOKEN
              valueFrom:
                secretKeyRef:
                  name: tether-proxy-token
                  key: token
            - name: TETHER_SESSION_ID
              value: "proxy"
          ports:
            - name: https
              containerPort: 8443
          volumeMounts:
            - name: tls
              mountPath: /tls
              readOnly: true
            - name: audit
              mountPath: /var/tether/audit
          resources:
            limits:
              cpu: 500m
              memory: 256Mi
            requests:
              cpu: 100m
              memory: 128Mi
      volumes:
        - name: tls
          secret:
            secretName: tether-proxy-tls
        - name: audit
          emptyDir: {}   # replace with a PVC for durable local storage
---
apiVersion: v1
kind: Service
metadata:
  name: tether-proxy
  namespace: tether-system
spec:
  selector:
    app: tether-proxy
  ports:
    - name: https
      port: 8443
      targetPort: 8443
  type: LoadBalancer   # or NodePort / Ingress depending on your cluster setup
```

**Proxy flags:**

| Flag | Default | Description |
|---|---|---|
| `--listen` | `:8443` | Address and port the proxy listens on |
| `--target` | `https://kubernetes.default.svc` | Kubernetes API server URL to proxy to |
| `--upstream-kubeconfig` | `~/.kube/config` | kubeconfig used for upstream API auth (falls back to in-cluster config) |
| `--audit-dir` | `/var/tether/audit` | Directory for local audit `.cast` recordings |
| `--audit-max-file-size` | `0` (disabled) | Maximum bytes per `.cast` file before rotation |
| `--tls-cert` | `""` | Path to TLS certificate file |
| `--tls-key` | `""` | Path to TLS private key file |
| `--tls-skip-verify` | `false` | Skip TLS verification of upstream API server (**dev only**) |
| `--dev-mode` | `false` | Disable TLS on the proxy listener (**development only; never use in production**) |

**Environment variables:**

| Variable | Description |
|---|---|
| `TETHER_TOKEN` | Static proxy token (see token validation note below) |
| `TETHER_SESSION_ID` | Session ID associated with `TETHER_TOKEN` |

> **Token validation note:** The current `StaticValidator` maps a single static token to a session ID, suitable for development and simple deployments. For production, replace `StaticValidator` with a Kubernetes Secret-backed token store that maps per-lease tokens to lease names.

### TLS Certificate Provisioning

The recommended approach is [cert-manager](https://cert-manager.io/). An example `Issuer` and `Certificate` resource is provided in `config/tls/certificate.yaml`:

```bash
# Install cert-manager (if not already present)
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml

# Edit config/tls/certificate.yaml — replace <your-email> and <your-domain>
kubectl apply -f config/tls/certificate.yaml
```

cert-manager will create the `tether-proxy-tls` Secret in the `tether-system` namespace. Mount it into the proxy pod as shown in the Deployment example above.

**Alternative — self-signed certificate (testing/air-gapped):**

```bash
openssl req -x509 -newkey rsa:4096 -nodes \
  -keyout tls.key -out tls.crt -days 365 \
  -subj "/CN=tether-proxy" \
  -addext "subjectAltName=DNS:tether-proxy.tether-system.svc"

kubectl create secret tls tether-proxy-tls \
  --cert=tls.crt --key=tls.key \
  -n tether-system
```

---

## tetherctl Reference

### Global flags

| Flag | Default | Description |
|---|---|---|
| `--kubeconfig` | `$KUBECONFIG` or `~/.kube/config` | Path to kubeconfig file |

### `tetherctl request`

Request a new time-limited `TetherLease`.

```
tetherctl request --role <cluster-role> --for <duration> [--reason <text>] [--name <lease-name>]
```

| Flag | Required | Description |
|---|---|---|
| `--role` | yes | `ClusterRole` to request access to |
| `--for` | yes | Duration, e.g. `30m`, `1h`, `2h30m` |
| `--reason` | no | Human-readable justification (strongly recommended for audit) |
| `--name` | no | Custom lease name; defaults to `<user>-<unix-timestamp>` |

**Example:**

```bash
tetherctl request --role cluster-admin --for 30m --reason "investigating outage #42"
```

### `tetherctl login`

Configure `kubeconfig` to route `kubectl` through the Tether proxy for a given lease.

```
tetherctl login --lease <lease-name> [--proxy <addr>] [--token <token>] [--insecure-skip-tls-verify]
```

| Flag | Default | Description |
|---|---|---|
| `--lease` | — (required) | Name of the `TetherLease` to activate |
| `--proxy` | `https://localhost:8443` | Address of the Tether proxy |
| `--token` | `$TETHER_TOKEN` | Proxy auth token |
| `--insecure-skip-tls-verify` | `false` | Skip TLS certificate verification of the proxy (**dev only**) |

After `login`, your active kubeconfig context is changed to `tether-<lease-name>`. All subsequent `kubectl` commands are routed through the proxy and recorded.

### `tetherctl playback`

Play back a recorded session from the local audit directory.

```
tetherctl playback --lease <session-id> [--audit-dir <dir>]
```

| Flag | Default | Description |
|---|---|---|
| `--lease` | — (required) | Lease/session ID (`.cast` file name without the extension) |
| `--audit-dir` | `/var/tether/audit` | Directory containing audit recordings |

The playback renders timing-accurate terminal output. Kubernetes API Table responses are pretty-printed. Gaps longer than 5 seconds are clamped to avoid long waits during replay.

### `tetherctl approve`

Approve a `TetherLease` that is `Pending` approval.

```
tetherctl approve <lease-name>
```

### `tetherctl deny`

Deny a `TetherLease` that is `Pending` approval.

```
tetherctl deny <lease-name>
```

---

## Audit Backend Reference

The audit backend is selected at proxy startup via the `--audit-dir` flag (local) or by building a custom binary that passes a different `audit.Backend` implementation to `proxy.NewTetherProxy`.

### Local Filesystem (default)

```
--audit-dir /var/tether/audit
--audit-max-file-size 104857600   # optional: rotate at 100 MiB
```

Recordings are written to `<audit-dir>/<sessionID>.cast`. When `--audit-max-file-size` is set, the active file is rotated to `<sessionID>-N.cast` before the limit would be exceeded.

### AWS S3

Use `audit.NewS3Backend` or `audit.NewS3BackendWithConfig` in a custom proxy build:

```go
backend, err := audit.NewS3BackendWithConfig(ctx, "my-audit-bucket", "tether/sessions", audit.S3BackendConfig{
    ValidateOnStartup: true, // HeadBucket check at startup
})
```

Authentication uses the standard AWS credential chain (`AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` / instance profile / IRSA). Objects are stored at `<prefix>/<sessionID>.cast` with `Content-Type: application/x-asciicast`.

**Recommended IAM policy:**

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["s3:PutObject", "s3:GetObject"],
      "Resource": "arn:aws:s3:::my-audit-bucket/tether/sessions/*"
    }
  ]
}
```

### Elasticsearch

Use `audit.NewElasticBackendWithConfig` in a custom proxy build:

```go
backend := audit.NewElasticBackendWithConfig("https://es.example.com:9200", "tether-sessions", audit.ElasticBackendConfig{
    Auth: audit.ElasticAuthConfig{
        APIKey: os.Getenv("ES_API_KEY"),
        // — or —
        // Username: "tether",
        // Password: os.Getenv("ES_PASSWORD"),
    },
})
```

Documents are indexed under `<index>/_doc/<sessionID>` with the following shape:

```json
{
  "sessionID": "alice-1704067200",
  "cast": "<full Asciinema v2 text content>"
}
```

Authentication: set `APIKey` (preferred) **or** `Username` + `Password` — if both are set, `APIKey` takes precedence.

---

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
# or natively: asciinema play <session>.cast
```

---

## Security Considerations

- **Least privilege**: The operator only creates `ClusterRoleBindings` for the explicitly requested `ClusterRole`. It does not grant broader access than the named role.
- **Time-limited**: All access automatically expires; the operator enforces expiry via controller requeueing. There are no "permanent" leases.
- **Finalizer-based cleanup**: `ClusterRoleBindings` are always cleaned up via finalizers, even if the `TetherLease` is deleted unexpectedly.
- **Audit trail**: All `exec` and `log` sessions are recorded before being forwarded. The audit record is written before the response is returned to the client.
- **Token validation**: The proxy rejects requests without a valid `X-Tether-Token`. In production, replace the `StaticValidator` with a Kubernetes Secret-backed token store that issues per-lease tokens and validates them against live lease state.
- **TLS**: The proxy enforces TLS in production mode (`--dev-mode` is not set and both `--tls-cert` and `--tls-key` must be provided). `--tls-skip-verify` and `--dev-mode` are explicitly rejected at startup unless the corresponding flag is passed.
- **Reason field**: While not required at the API level, organizational policy should require a `spec.reason` for all lease requests to maintain a human-readable audit trail.
- **RBAC for tetherctl users**: Engineers who use `tetherctl` need `create` permission on `tetherleases` and `get`/`list` permission to observe lease status. Scope these permissions tightly; cluster-wide `create` on `tetherleases` is sufficient for normal use.
- **Operator RBAC**: The operator's service account must have `create`/`delete` permission on `ClusterRoleBindings`. Audit this carefully — a compromised operator could bind any `ClusterRole`.
- **Network policy**: Restrict which pods and external clients can reach the Tether Proxy. Only the intended engineer workstations and CI systems should have network-level access to port 8443.

---

## Development Commands

```bash
make build          # Build all binaries (output: ./bin/)
make test           # Run unit tests
make test-race      # Run tests with race detector
make vet            # Run go vet
make fmt            # Format source code
make lint           # Run golangci-lint (must be installed separately)
make tidy           # Run go mod tidy
make install        # Apply CRD to current cluster
make uninstall      # Remove CRD from current cluster
make local-setup    # Bootstrap Kind cluster + start operator & proxy
make local-teardown # Stop all components and delete the Kind cluster
make clean          # Remove build artifacts
make docker-build   # Build operator and proxy Docker images
make docker-push    # Push images to ghcr.io/jaydee94/tether
make manifests      # Regenerate CRD manifests (requires controller-gen)
make generate       # Regenerate deepcopy functions (requires controller-gen)
```

---

## Observability — Prometheus Metrics

Both the operator and proxy expose Prometheus-compatible metrics endpoints.

### Operator metrics

The operator uses the [controller-runtime](https://github.com/kubernetes-sigs/controller-runtime) metrics server (default `:8080/metrics`).

| Metric | Type | Description |
|--------|------|-------------|
| `lease_activations_total` | Counter | Total TetherLease activations |
| `lease_expirations_total` | Counter | Total TetherLease expirations |
| `lease_revocations_total` | Counter | Total TetherLease revocations |
| `active_leases` | Gauge | Current number of active TetherLeases |
| `tether_operator_reconcile_outcomes_total` | Counter | Reconcile outcomes by `outcome` and `reason` labels |

### Proxy metrics

The proxy exposes a separate metrics server (default `:9090/metrics`, configurable via `--metrics-bind-address`).

| Metric | Type | Description |
|--------|------|-------------|
| `proxy_requests_total` | Counter | Total proxied requests, labeled by HTTP `status` code |
| `proxy_request_duration_seconds` | Histogram | Latency of proxied requests in seconds |
| `token_validation_errors_total` | Counter | Token validation failures |

### Scraping with Prometheus Operator

`ServiceMonitor` manifests for both components are provided in `deploy/`:

```bash
kubectl apply -f deploy/servicemonitor-operator.yaml
kubectl apply -f deploy/servicemonitor-proxy.yaml
```

Both expect the respective Kubernetes `Service` to expose a port named `metrics` pointing to the metrics address. Adjust the `namespace` and `selector` labels as needed for your deployment.

---

## Changelog

### [Unreleased]

- Production deployment documentation and operator/proxy RBAC examples
- `tetherctl approve` / `tetherctl deny` subcommands
- `tetherctl login` now validates placeholder lease names at startup
- Audit file rotation (`--audit-max-file-size`)
- S3 `ValidateOnStartup` option for early bucket access errors
- Elasticsearch authentication (API key + basic auth)
- TLS enforcement in production mode (proxy exits if cert/key missing without `--dev-mode`)

---

*Tether is an open-source project. Contributions welcome — please open a GitHub issue before submitting large changes.*
