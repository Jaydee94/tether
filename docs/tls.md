# TLS Configuration for the Tether Proxy

The Tether proxy **requires TLS in production**. If you start the proxy without
`--tls-cert` and `--tls-key`, it exits with:

```
TLS cert and key are required in production mode. Use --dev-mode to disable TLS (development only).
```

Use `--dev-mode` only for local development — never in production.

---

## Flags

| Flag | Default | Description |
|---|---|---|
| `--tls-cert` | `""` | Path to the PEM-encoded TLS certificate (or full chain). |
| `--tls-key` | `""` | Path to the PEM-encoded private key. |
| `--dev-mode` | `false` | Bypass TLS enforcement. Starts plain HTTP. **Development only.** |

---

## Production: cert-manager + Let's Encrypt

The recommended path for production clusters is cert-manager with an ACME
issuer. A ready-to-use example is in [`config/tls/certificate.yaml`](../config/tls/certificate.yaml).

1. Install cert-manager (v1.x):

   ```bash
   kubectl apply -f https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml
   ```

2. Edit `config/tls/certificate.yaml` — replace `<your-email>` and `<your-domain>`.

3. Apply the manifests:

   ```bash
   kubectl apply -f config/tls/certificate.yaml
   ```

4. Wait for the `tether-proxy-tls` Secret to become ready:

   ```bash
   kubectl get certificate -n tether-system tether-proxy-tls
   ```

5. Mount the Secret into the proxy Pod and pass the paths:

   ```
   --tls-cert /etc/tether/tls/tls.crt
   --tls-key  /etc/tether/tls/tls.key
   ```

cert-manager automatically renews the certificate 15 days before expiry (see
`renewBefore` in the manifest) and updates the Secret in place. The Go TLS
stack reads the cert/key files on each new connection, so rotation is zero-downtime.

---

## Development: self-signed certificates

### Option A — mkcert (recommended for local development)

`mkcert` creates certificates trusted by your local browsers and OS without
manual CA import steps.

```bash
# Install mkcert
brew install mkcert          # macOS
sudo apt install mkcert      # Debian/Ubuntu (universe)
# or download from https://github.com/FiloSottile/mkcert/releases

# Install the local CA into your system trust store
mkcert -install

# Generate a cert+key pair for localhost
mkcert -cert-file tls.crt -key-file tls.key localhost 127.0.0.1 ::1

# Start the proxy with the generated files
./bin/proxy \
  --tls-cert tls.crt \
  --tls-key  tls.key \
  --listen   :8443
```

### Option B — openssl (no extra dependencies)

```bash
# Generate a self-signed certificate valid for 365 days
openssl req -x509 -newkey rsa:4096 -sha256 -days 365 -nodes \
  -keyout tls.key \
  -out    tls.crt \
  -subj   "/CN=localhost" \
  -addext "subjectAltName=DNS:localhost,IP:127.0.0.1"

# Start the proxy
./bin/proxy \
  --tls-cert tls.crt \
  --tls-key  tls.key \
  --listen   :8443
```

> **Note:** Clients will see a certificate warning because the CA is not
> trusted. Pass `--tls-skip-verify` to kubectl/curl during development, or
> add the generated cert to your trust store manually.

---

## Automatic rotation with cert-manager

cert-manager's controller watches the `Certificate` resource and renews
the backing Secret before `renewBefore` is reached. Because the proxy uses
`ListenAndServeTLS` (which calls `tls.LoadX509KeyPair` on each TLS handshake
internally via `tls.Config.GetCertificate`), the proxy will pick up the new
certificate without a restart.

For clusters that need ACME DNS-01 challenges (wildcard certs), update the
`solvers` section in `config/tls/certificate.yaml` to use a supported
[DNS-01 provider](https://cert-manager.io/docs/configuration/acme/dns01/).
