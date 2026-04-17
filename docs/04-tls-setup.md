# TLS Setup and Security

This guide documents TLS requirements for secure production deployments, certificate options, and integration with cert-manager. It references JAY-7 for implementation details.

## Summary

TLS is required for production to secure control and data planes. Tether supports multiple certificate provisioning approaches, including self-signed (dev), cert-manager (recommended), and external/internal CAs.

Reference: [JAY-7](/JAY/issues/JAY-7)

## Certificate Options

- Self-signed (development): `openssl` or `mkcert`
- cert-manager (production): Use ACME or an internal CA issuer
- External CA: Obtain certificates from your PKI provider

## Example: cert-manager Issuer

```yaml
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: tether-issuer
spec:
  acme:
    server: https://acme-v02.api.letsencrypt.org/directory
    email: admin@example.com
    privateKeySecretRef:
      name: tether-issuer-key
    solvers:
      - http01:
          ingress:
            class: nginx
```

## Proxy TLS Configuration

When deploying the tether-proxy behind a Kubernetes Service, mount the TLS certificate and key and configure the proxy to use them. Example Helm values:

```yaml
proxy:
  tls:
    enabled: true
    secretName: tether-proxy-tls
```

## Certificate Rotation

cert-manager automates rotation. For self-signed certificates, rotate using a scheduled job and update the secret referenced by the proxy deployment.

## Troubleshooting

- If clients cannot establish TLS, ensure the CA bundle contains the issuing CA
- Check proxy logs for TLS handshake errors: `kubectl logs -l app=tether-proxy -n tether-system`
