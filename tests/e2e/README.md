# E2E Test Guide

## Overview

This directory contains end-to-end (E2E) tests for Tether. These tests validate complete user workflows against a real Kubernetes cluster, including lease creation, approval flows, active phase operations, and expiry handling.

## Test Coverage

The E2E test suite covers the following core workflows:

1. **RequestWorkflow** - Validates lease creation with proper initialization
2. **ApprovalWorkflow** - Verifies ClusterRoleBinding creation and token generation
3. **LeaseActiveWorkflow** - Tests sustained active phase with multiple operations
4. **LeaseExpiryWorkflow** - Validates automatic cleanup and expiry handling
5. **NamespaceScopedWorkflow** - Tests namespace-scoped RoleBinding access
6. **SessionRecording** - Verifies proxy audit backend integration capability
7. **ConcurrentLeases** - Stress test with multiple simultaneous leases

## Prerequisites

Before running E2E tests, ensure you have:

1. **Kubernetes cluster** running (local or remote)
   - Options: Kind, K3S, Docker Desktop, or any accessible cluster
   
2. **kubectl** configured and accessible
   ```bash
   kubectl cluster-info
   ```

3. **Go 1.19+** installed
   ```bash
   go version
   ```

4. **Tether operator and proxy deployed** into your cluster
   ```bash
   make deploy
   # or for local development:
   make local-setup
   ```

5. **TetherLease CRD installed** in your cluster
   ```bash
   kubectl apply -f config/crd/tetherlease.yaml
   ```

## Setup Instructions

### Option 1: Local Kind Cluster (Recommended for Development)

Use the provided local setup script:

```bash
# Bootstrap a local Kind cluster with Tether pre-installed
make local-setup

# After testing, tear down the cluster
make local-teardown
```

This script:
- Creates a Kind cluster named `tether-dev`
- Installs the TetherLease CRD
- Builds and deploys the operator and proxy
- Configures kubeconfig for local testing

### Option 2: Existing Cluster

If using an existing cluster, ensure:

1. The `tether-system` namespace exists:
   ```bash
   kubectl create namespace tether-system
   ```

2. TetherLease CRD is installed:
   ```bash
   kubectl apply -f config/crd/tetherlease.yaml
   ```

3. Operator and proxy are deployed:
   ```bash
   make deploy
   ```

4. Your kubeconfig is properly configured:
   ```bash
   export KUBECONFIG=/path/to/kubeconfig
   # or rely on default ~/.kube/config
   ```

## Running E2E Tests

### Run All E2E Tests

```bash
make test-e2e
```

### Run Specific Test

```bash
go test -tags=e2e ./tests/e2e/... -v -run TestRequestWorkflow
```

### Run with Verbose Output

```bash
go test -tags=e2e ./tests/e2e/... -v -count=1
```

### Run with Custom kubeconfig

```bash
KUBECONFIG=/path/to/custom/kubeconfig make test-e2e
```

## Test Execution Flow

Each test follows this pattern:

1. **Setup** - Creates required Kubernetes resources
2. **Validation** - Verifies expected behavior and state transitions
3. **Cleanup** - Removes created resources (via context cleanup)

Example flow for RequestWorkflow:
```
1. Create TetherLease in Pending phase
2. Verify lease exists and is in Pending phase
3. Delete lease (cleanup)
```

## Debugging Failed Tests

### Check Cluster State

```bash
# List all TetherLeases
kubectl get tetherlease -n tether-system

# Describe a specific lease
kubectl describe tetherlease <lease-name> -n tether-system

# Check operator logs
kubectl logs -n tether-system -l app=tether-operator -f

# Check proxy logs
kubectl logs -n tether-system -l app=tether-proxy -f
```

### Enable Verbose Kubeconfig Logging

```bash
export KUBECONFIG_DEBUG=1
go test -tags=e2e ./tests/e2e/... -v
```

### Check kubeconfig Access

```bash
# Verify cluster connectivity
kubectl cluster-info

# Check auth
kubectl auth can-i get tetherlease --as system:admin
```

### Common Issues

**Issue**: `kubeconfig not found`
- **Solution**: Ensure `KUBECONFIG` env var is set or `~/.kube/config` exists

**Issue**: `the server could not find the requested resource (post tetherlease.tether.dev)`
- **Solution**: Install the TetherLease CRD: `kubectl apply -f config/crd/tetherlease.yaml`

**Issue**: `leases did not reach Active phase`
- **Solution**: Check operator logs: `kubectl logs -n tether-system -l app=tether-operator`

**Issue**: `permission denied` when creating ClusterRoleBinding
- **Solution**: Ensure your user has cluster-admin rights: `kubectl auth can-i create clusterrolebindings`

## Test Isolation

Each test:
- Runs in a unique context with its own timeout
- Uses distinct resource names (e.g., `test-request-workflow-*`)
- Cleans up its resources even if the test fails
- Does not interfere with other concurrent tests

## Performance Considerations

- Each test has a **30-second timeout** by default
- Tests **poll for phase transitions** at 1-second intervals
- Total suite runtime typically **60-90 seconds** depending on cluster responsiveness

## Extending Tests

To add a new E2E test:

1. Create a test function following the pattern:
   ```go
   func TestMyWorkflow(t *testing.T) {
       ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
       defer cancel()
       
       // Your test here
   }
   ```

2. Use helper functions:
   - `createTetherLease()` - Create a lease
   - `getTetherLease()` - Retrieve a lease
   - `updateTetherLeasePhase()` - Change phase
   - `waitForPhase()` - Poll for phase transition
   - `deleteTetherLease()` - Delete a lease

3. Always clean up resources in the test

4. Run and verify:
   ```bash
   go test -tags=e2e ./tests/e2e/... -v -run TestMyWorkflow
   ```

## CI/CD Integration

For continuous integration pipelines:

```bash
# Start local cluster
make local-setup

# Run E2E tests
make test-e2e

# Capture results
TEST_RESULTS=/tmp/e2e-results.json go test -tags=e2e ./tests/e2e/... -json > $TEST_RESULTS

# Teardown
make local-teardown
```

## Related Documentation

- [Contributing Guide](../../CONTRIBUTING.md)
- [Tether Architecture](../../docs/architecture.md)
- [API Reference](../../docs/api.md)
