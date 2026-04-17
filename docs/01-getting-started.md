# Getting Started with Tether

This guide helps new users install, configure, and run Tether for the first time. It includes a quick overview, prerequisites, installation steps, a first-run walkthrough, and basic verification commands.

## Quick Overview

Tether enables short-lived access leases to cluster resources. It provides a lease request workflow, an operator that enforces lease lifecycle, and a lightweight CLI (`tetherctl`) for interacting with leases.

Core components:

- `tether-operator` — manages TetherLease CRs and associated resources
- `tether-proxy` — network proxy for session traffic
- `tetherctl` — user CLI for requesting and managing leases

## Prerequisites

- Kubernetes cluster (v1.20+ recommended)
- kubectl configured with appropriate cluster credentials
- Helm 3 (optional, recommended for production installs)
- A service account with permissions to install the operator (cluster-admin for quick start)

## Installation (Quick Start - Helm)

1. Add the Tether Helm repo:

```bash
helm repo add tether https://example.com/helm-charts
helm repo update
```

2. Install the operator and proxy in the `tether-system` namespace:

```bash
kubectl create namespace tether-system || true
helm install tether tether/tether --namespace tether-system
```

3. Verify pods are running:

```bash
kubectl get pods -n tether-system
```

## Installation (Development - Local)

For local development you can run the operator and proxy as processes. See `DEVELOPMENT.md` in the repo for developer-focused instructions.

## First-Run Walkthrough

1. Create a sample TetherLease CR to request access to a namespace:

```yaml
apiVersion: tether.example.com/v1alpha1
kind: TetherLease
metadata:
  name: demo-lease
  namespace: default
spec:
  requester: alice@example.com
  duration: 1h
  scopes:
    - namespaces: ["default"]
  reason: "Onboarding demo"
```

Apply it:

```bash
kubectl apply -f demo-lease.yaml
kubectl get tetherlease -n default
```

2. Using `tetherctl` to request a lease:

```bash
tetherctl request --namespace default --duration 1h --reason "onboarding demo"
# show active leases
tetherctl list
```

## Verification

- Confirm operator and proxy pods are running: `kubectl get pods -n tether-system`
- Confirm a lease appears when requested: `tetherctl list` or `kubectl get tetherlease -A`

## Troubleshooting

- If `tetherctl` cannot connect, check proxy service: `kubectl get svc -n tether-system`
- If leases are pending, inspect operator logs: `kubectl logs -l app=tether-operator -n tether-system`

## Next Steps

- Read `02-requesting-leases.md` for detailed lease workflows and examples
- Read `03-access-control.md` for access scoping and RBAC integration
