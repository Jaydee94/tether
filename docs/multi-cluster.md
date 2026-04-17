# Multi-Cluster Support

Tether supports managing privileged access across multiple Kubernetes clusters from a single control plane deployment.

## Overview

With multi-cluster support, you can:
- Request access to specific clusters using `tetherctl request --cluster <name>`
- Manage RBAC bindings across multiple clusters from one operator
- Route proxy traffic to the correct cluster automatically
- Maintain centralized audit logs with cluster context

## Architecture

- **Control Plane Cluster**: Runs the Tether operator and stores TetherLease CRDs
- **Target Clusters**: Clusters where RBAC bindings are created based on TetherLeases
- **Cluster Registry**: Configuration mapping cluster names to API servers and credentials

## Configuration

### Operator Configuration

Create a cluster configuration file (e.g., `/etc/tether/clusters.yaml`):

```yaml
clusters:
  - name: local
    apiServer: https://kubernetes.default.svc
    default: true
    
  - name: prod-us-west-2
    apiServer: https://prod-us-west-2.k8s.example.com:6443
    kubeconfig: /var/secrets/prod-kubeconfig
    
  - name: staging-eu
    apiServer: https://staging-eu.k8s.example.com:6443
    serviceAccountToken: /var/secrets/staging-token
    caCert: /var/secrets/staging-ca.crt
```

Start the operator with the `--cluster-config` flag:

```bash
./operator --cluster-config /etc/tether/clusters.yaml
```

### Cluster Authentication

Three authentication methods are supported:

1. **In-cluster** (for the control plane cluster)
2. **Kubeconfig file**: Mount as a Secret/ConfigMap
3. **Service Account token + CA cert**: Create a ServiceAccount in the target cluster and extract its token

#### Example: Service Account Setup for Target Cluster

On the target cluster:

```bash
# Create ServiceAccount
kubectl create namespace tether-system
kubectl create serviceaccount tether-operator -n tether-system

# Grant RBAC permissions
kubectl apply -f - <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: tether-operator
rules:
- apiGroups: ["rbac.authorization.k8s.io"]
  resources: ["clusterrolebindings", "rolebindings"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
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
EOF

# Extract token and CA cert
kubectl get secret -n tether-system \
  $(kubectl get sa tether-operator -n tether-system -o jsonpath='{.secrets[0].name}') \
  -o jsonpath='{.data.token}' | base64 -d > /tmp/target-token

kubectl get secret -n tether-system \
  $(kubectl get sa tether-operator -n tether-system -o jsonpath='{.secrets[0].name}') \
  -o jsonpath='{.data.ca\.crt}' | base64 -d > /tmp/target-ca.crt
```

Mount these as Secrets in the control plane cluster operator pod.

### Proxy Configuration

The proxy also needs cluster configuration to route requests. Create a similar YAML file with cluster API servers (no credentials needed since the proxy only routes):

```yaml
clusters:
  - name: local
    apiServer: https://kubernetes.default.svc
    default: true
  - name: prod-us-west-2
    apiServer: https://prod-us-west-2.k8s.example.com:6443
  - name: staging-eu
    apiServer: https://staging-eu.k8s.example.com:6443
```

## Usage

### Request Access to a Specific Cluster

```bash
tetherctl request --role cluster-admin --for 30m --cluster prod-us-west-2 --reason "prod incident"
```

### List Leases with Cluster Information

```bash
kubectl get tetherleases

NAME                    USER    ROLE            CLUSTER          PHASE   EXPIRES
alice-prod-incident     alice   cluster-admin   prod-us-west-2   Active  2026-04-17T07:30:00Z
bob-staging-debug       bob     view            staging-eu       Active  2026-04-17T08:00:00Z
```

### Login and Use

```bash
tetherctl login --lease alice-prod-incident
kubectl get pods -A  # Routed to prod-us-west-2 cluster
```

## Audit Logs

All audit logs include the target cluster name in the session metadata:

```json
{"version":2,"width":220,"height":50,"timestamp":1704067200,"title":"cluster=prod-us-west-2 tether/alice-prod-incident GET /api/v1/pods"}
```

This enables filtering and analysis by cluster in your audit backend (S3, Elasticsearch, etc.).

## Backward Compatibility

Single-cluster deployments continue to work without any configuration changes:
- If `--cluster-config` is not provided, the operator defaults to in-cluster authentication
- TetherLeases without a `cluster` field default to the "local" cluster
- Existing deployments are not affected

## Troubleshooting

### Operator cannot reach target cluster

Check the operator logs:
```bash
kubectl logs -n tether-system deployment/tether-operator
```

Verify network connectivity and credentials.

### RBAC binding not created

1. Verify the ServiceAccount in the target cluster has the required RBAC permissions
2. Check if the cluster name in the TetherLease matches the cluster registry configuration
3. Review operator logs for errors

### Proxy routing to wrong cluster

1. Check that the proxy's cluster configuration file matches the operator's
2. Verify the TetherLease.Status.Cluster field is set correctly
3. Restart the proxy to clear any cached configurations

## Security Considerations

1. **Least Privilege**: Target cluster ServiceAccounts should only have permissions to manage RBAC bindings
2. **Credential Isolation**: Store cluster credentials as Kubernetes Secrets with restricted RBAC access
3. **Network Segmentation**: Consider network policies to restrict which pods can access target clusters
4. **Audit Trail**: All requests include cluster context for compliance and forensics
5. **Token Security**: Session tokens are stored in the control plane cluster, so compromising a target cluster doesn't allow token forgery

## Limitations

- Maximum recommended clusters: 10-20 per operator
- Cross-cluster requests require network connectivity from operator and proxy to all target clusters
- High availability requires running multiple operators with leader election (standard Kubernetes pattern)
