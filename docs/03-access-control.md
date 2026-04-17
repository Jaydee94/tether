# Namespace-scoped vs Cluster-scoped Access

This guide explains the access control models supported by Tether, including namespace-scoped and cluster-scoped deployments, RBAC integration, and migration guidance.

## Models

- Namespace-scoped: Tether operator and resources are installed per-namespace or limited to a set of namespaces. Good for multi-tenant or restricted environments.
- Cluster-scoped: Operator has cluster-wide scope and can grant leases across namespaces. Good for single-tenant or operator-controlled environments.

## Namespace-scoped Deployment

Pros:

- Reduced blast radius
- Easier RBAC policies per tenant

Cons:

- Requires deployment in each namespace that needs leases

Example RBAC (namespace-scoped):

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: tether-operator
  namespace: tether-system

---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: tether-operator-role
  namespace: default
rules:
  - apiGroups: ["tether.example.com"]
    resources: ["tetherleases"]
    verbs: ["get","list","watch","create","update","patch","delete"]
```

## Cluster-scoped Deployment

Pros:

- Easier centralized management
- Single operator instance

Cons:

- Broader permissions required
- Increased security review

ClusterRole example:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: tether-operator-cluster-role
rules:
  - apiGroups: ["tether.example.com"]
    resources: ["tetherleases"]
    verbs: ["*"]
```

## Migration Guidance

1. Evaluate scope and tenant boundaries
2. Export existing leases and recreate under new model if necessary
3. Gradually roll out operator changes with feature flags

## Audit & Compliance

- Ensure requesters include identifying metadata (email, ticket id)
- Export lease audit logs to a central system
