# Best Practices

Operational and security best practices for running Tether in production.

## Security

- Principle of least privilege: grant minimal RBAC permissions
- Short lease durations and explicit renewal workflows
- Record requestor identity and reasons for audit
- Rotate credentials and certificates regularly

## Operations

- Use multi-environment configurations (dev/staging/prod)
- Monitor operator and proxy health with Prometheus/Grafana
- Automate deploys with GitOps for reproducibility

## Troubleshooting Patterns

- Lease not provisioning: check operator logs and CR status
- TLS errors: validate CA bundle and certificate mount paths
- Metrics missing: check service discovery and endpoints

## Example Checklists

- Pre-production checklist:
  - cert-manager configured and issuing certs
  - monitoring and alerts enabled
  - backup and restore tested
