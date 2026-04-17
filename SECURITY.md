# Security Policy

## Reporting Security Vulnerabilities

If you discover a security vulnerability in Tether, **please do not open a public GitHub issue**. Instead, email the details to **security@example.com** (or your organization's security contact).

### What to Include

- Description of the vulnerability
- Affected versions (if known)
- Proof of concept or steps to reproduce
- Suggested remediation (if you have one)

We will acknowledge receipt within 48 hours and aim to provide a fix in a reasonable timeframe.

## Security Considerations

Tether's core security model relies on several layers:

### 1. Least Privilege

- The operator only grants the explicitly requested `ClusterRole`
- Engineers receive no standing permissions — access is granted on-demand
- Scope RBAC tightly: engineers need `create` on `tetherleases`, not blanket cluster-admin

### 2. Time-Limited Access

- All leases auto-expire after the requested duration
- The operator enforces expiry via controller requeueing
- There are no "permanent" leases
- Early revocation is immediate: the proxy invalidates the session token instantly

### 3. Audit Trail

- All `kubectl exec` and `kubectl logs` sessions are recorded in Asciinema v2 format
- Recordings are written *before* the response is returned to the client (fail-open: if audit fails, the request is rejected)
- Store audit recordings in a centralized, immutable location (AWS S3 or Elasticsearch recommended for production)
- Rotate and archive audit files regularly

### 4. Token Security

- Session tokens are cryptographically random (32 bytes, base64-URL-encoded)
- Tokens are stored as Kubernetes Secrets in the `tether-system` namespace
- In production, replace `StaticValidator` with a per-lease token store
- Never log tokens or include them in debug output

### 5. TLS & Transport Security

- The proxy **enforces TLS in production** (`--dev-mode` is rejected unless explicitly enabled)
- Both `--tls-cert` and `--tls-key` must be provided for production deployments
- Use cert-manager for automatic certificate rotation
- Validate the proxy's TLS certificate on the client side (`--insecure-skip-tls-verify` is for development only)

### 6. RBAC Hardening

**Operator RBAC:**
- Service account must have `create`/`delete` on `ClusterRoleBindings` only
- Audit carefully: a compromised operator can bind any `ClusterRole`
- Use restrictive admission policies to prevent operator escalation

**tetherctl user RBAC:**
- Users need `create` on `tetherleases` and `get`/`list` for status observation
- Cluster-wide `create` permission on `tetherleases` is sufficient for normal use
- Consider namespace-scoped leases if you need fine-grained isolation

**Network policy:**
- Restrict which pods and external clients can reach the proxy (port 8443)
- Only intended engineer workstations and CI systems should have access

### 7. Operational Security

**Audit Storage:**
- Never store audit recordings on the proxy pod's ephemeral filesystem in production
- Use AWS S3 (with encryption at rest and versioning enabled) or Elasticsearch for production deployments
- Restrict access to audit data: engineers should not be able to access each other's recordings
- Implement retention policies (e.g., 90-day retention for compliance)

**Log Management:**
- Monitor operator and proxy logs for unusual activity (e.g., repeated token validation failures, rapid lease creation)
- Encrypt logs in transit and at rest
- Forward logs to a centralized SIEM or log aggregation system

**Access Revocation:**
- Test the immediate revocation flow regularly: request a lease, then revoke it and verify that subsequent API calls are rejected
- Monitor for leaked tokens: if a token is suspected to be compromised, revoke the associated lease immediately

### 8. Kubernetes Cluster Security

- Keep your Kubernetes cluster up to date with security patches
- Enforce network policies to restrict pod-to-pod communication
- Use Pod Security Policies or Pod Security Standards to restrict privileged containers
- Regularly audit RBAC bindings and service account permissions

## Compliance & Audit

If you are using Tether in a regulated environment:

- **Enable audit logging** in your Kubernetes cluster and forward to a centralized system
- **Enable Tether audit** and store recordings in a compliant audit backend (S3 with encryption, Elasticsearch)
- **Document your RBAC model** and review it regularly
- **Test access revocation** as part of your security testing routine
- **Monitor for anomalies** (e.g., unusual access patterns, long-lived leases)

## Version Support

We provide security patches for the latest release. Older versions may not receive updates. Please keep Tether up to date.

## Questions?

For security-related questions, contact **security@example.com**. For general usage questions, open an issue on GitHub.

---

**Last updated:** 2026-04-17
