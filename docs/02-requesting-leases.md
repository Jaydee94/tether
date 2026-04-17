# Requesting Leases

This document covers the TetherLease resource, the lifecycle of a lease, and examples for creating and managing leases via YAML and `tetherctl`.

## TetherLease Overview

`TetherLease` is the primary CRD that represents a short-lived access grant. Key fields:

- `spec.requester` — identity requesting access
- `spec.duration` — requested lease duration (e.g. `1h`, `30m`)
- `spec.scopes` — resources/namespaces the lease should allow access to
- `spec.reason` — human-readable reason

## Lease Lifecycle

1. `Pending` — request created and awaiting approval or provisioning
2. `Active` — resources provisioned and access granted
3. `Expired` — duration ended and resources cleaned up
4. `Revoked` — manually revoked by an operator or system

## YAML Example

```yaml
apiVersion: tether.example.com/v1alpha1
kind: TetherLease
metadata:
  name: alice-lease
  namespace: default
spec:
  requester: alice@example.com
  duration: 2h
  scopes:
    - namespaces: ["default"]
  reason: "Debugging database issue"
```

Apply it with:

```bash
kubectl apply -f alice-lease.yaml
kubectl get tetherlease -n default alice-lease -o yaml
```

## tetherctl Examples

Request a lease via CLI:

```bash
tetherctl request --namespace default --duration 2h --reason "debugging"

# List leases
tetherctl list

# Revoke a lease
tetherctl revoke <lease-id>
```

## Approval Workflows

For clusters that enforce approvals, leases may require manual approval by an approver role. The operator will record the approval history in the CR status.

## Best Practices

- Request minimal scope required for the task
- Use short durations and renew as needed
- Include a clear reason for auditing

## Troubleshooting

- If the operator does not provision resources, check operator logs: `kubectl logs -l app=tether-operator -n tether-system`
- If the CLI fails, verify kubeconfig and proxy reachability
