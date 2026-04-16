#!/usr/bin/env bash
# local-setup.sh — Bootstraps a complete local Tether development environment
# using a Kind (Kubernetes-in-Docker) cluster.
#
# Usage:
#   ./scripts/local-setup.sh          # Set up the cluster and start all components
#   ./scripts/local-setup.sh --teardown  # Stop all components and delete the cluster
#   ./scripts/local-setup.sh --help      # Show this help

set -euo pipefail

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
CLUSTER_NAME="${TETHER_CLUSTER:-tether-dev}"
PROXY_PORT="${TETHER_PROXY_PORT:-8443}"
AUDIT_DIR="${TETHER_AUDIT_DIR:-/tmp/tether-audit}"
TETHER_TOKEN="${TETHER_TOKEN:-tether-dev-token}"
TETHER_SESSION="${TETHER_SESSION:-dev-session}"
TETHER_NAMESPACE="${TETHER_NAMESPACE:-tether-system}"
OPERATOR_IMAGE="${TETHER_OPERATOR_IMAGE:-tether/operator:dev}"
PROXY_IMAGE="${TETHER_PROXY_IMAGE:-tether/proxy:dev}"
DEMO_LEASE_NAME=""
PID_DIR="/tmp/tether-pids"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
KIND_CONFIG="${REPO_ROOT}/scripts/kind-config.yaml"

# ---------------------------------------------------------------------------
# Colors
# ---------------------------------------------------------------------------
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
BOLD='\033[1m'
NC='\033[0m' # No Color

log_info()    { echo -e "${GREEN}[INFO]${NC}  $*"; }
log_warn()    { echo -e "${YELLOW}[WARN]${NC}  $*"; }
log_error()   { echo -e "${RED}[ERROR]${NC} $*" >&2; }
log_step()    { echo -e "\n${BLUE}${BOLD}==> $*${NC}"; }
log_success() { echo -e "${GREEN}${BOLD}✔ $*${NC}"; }

# ---------------------------------------------------------------------------
# Help
# ---------------------------------------------------------------------------
usage() {
  cat <<EOF
${BOLD}Tether Local Setup${NC}

Sets up a complete local Tether development environment using a Kind cluster.

${BOLD}USAGE${NC}
  $0 [OPTIONS]

${BOLD}OPTIONS${NC}
  --teardown    Stop all Tether processes and delete the Kind cluster
  --help        Show this help message

${BOLD}ENVIRONMENT${NC}
  TETHER_CLUSTER      Kind cluster name        (default: tether-dev)
  TETHER_PROXY_PORT   Proxy listen port        (default: 8443)
  TETHER_AUDIT_DIR    Local audit directory    (default: /tmp/tether-audit)
  TETHER_TOKEN        Static proxy token       (default: tether-dev-token)
  TETHER_SESSION      Dev session ID           (default: dev-session)
  TETHER_NAMESPACE    Runtime namespace        (default: tether-system)

${BOLD}WHAT THIS SCRIPT DOES${NC}
  1. Checks prerequisites (kind, kubectl, docker, go)
  2. Creates a Kind cluster (or reuses an existing one)
  3. Installs the TetherLease CRD into the cluster
  4. Builds the operator, proxy, and tetherctl binaries
  5. Builds container images and loads them into Kind
  6. Deploys the Tether operator and proxy into the cluster
  7. Starts a local port-forward to the in-cluster proxy service
  8. Creates a demo TetherLease for the current user
  9. Prints instructions for testing

${BOLD}TESTING AFTER SETUP${NC}
  # Check the demo lease created by the script
  kubectl get tetherlease demo-kind-tether-dev-setup

  # Wait until the demo lease is active
  kubectl wait --for=jsonpath='{.status.phase}'=Active tetherlease/demo-kind-tether-dev-setup --timeout=30s

  # Activate session (updates kubeconfig to route through proxy)
  ./bin/tetherctl login --lease demo-kind-tether-dev-setup --proxy https://localhost:${PROXY_PORT} --token ${TETHER_TOKEN} --insecure-skip-tls-verify

  # Trigger a recordable command through the in-cluster proxy
  kubectl get namespaces

  # Play back the recorded dev session
  ./bin/tetherctl playback --lease ${TETHER_SESSION} --audit-dir ${AUDIT_DIR}

EOF
}

# ---------------------------------------------------------------------------
# Prerequisite checks
# ---------------------------------------------------------------------------
check_prerequisites() {
  log_step "Checking prerequisites"

  local missing=()

  for cmd in kind kubectl docker go openssl; do
    if command -v "$cmd" &>/dev/null; then
      log_info "$cmd found: $(command -v "$cmd")"
    else
      log_error "$cmd not found"
      missing+=("$cmd")
    fi
  done

  if [[ ${#missing[@]} -gt 0 ]]; then
    log_error "Missing required tools: ${missing[*]}"
    echo ""
    echo "Install instructions:"
    for tool in "${missing[@]}"; do
      case "$tool" in
        kind)    echo "  kind:    https://kind.sigs.k8s.io/docs/user/quick-start/#installation" ;;
        kubectl) echo "  kubectl: https://kubernetes.io/docs/tasks/tools/" ;;
        docker)  echo "  docker:  https://docs.docker.com/get-docker/" ;;
        go)      echo "  go:      https://go.dev/doc/install" ;;
        openssl) echo "  openssl: install your distro package (e.g. apt install openssl)" ;;
      esac
    done
    exit 1
  fi

  # Verify Docker daemon is running
  if ! docker info &>/dev/null; then
    log_error "Docker daemon is not running. Please start Docker and retry."
    exit 1
  fi

  if [[ "${AUDIT_DIR}" != "/tmp/tether-audit" ]]; then
    log_error "TETHER_AUDIT_DIR must currently be /tmp/tether-audit for the in-cluster local setup flow."
    exit 1
  fi

  log_success "All prerequisites satisfied"
}

# ---------------------------------------------------------------------------
# Kind cluster
# ---------------------------------------------------------------------------
create_cluster() {
  log_step "Setting up Kind cluster: ${CLUSTER_NAME}"

  local node_name="${CLUSTER_NAME}-control-plane"

  if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
    if docker inspect "${node_name}" --format '{{range .Mounts}}{{printf "%s:%s\n" .Source .Destination}}{{end}}' 2>/dev/null | grep -Fqx "${AUDIT_DIR}:${AUDIT_DIR}"; then
      log_warn "Cluster '${CLUSTER_NAME}' already exists — reusing it."
    else
      log_warn "Cluster '${CLUSTER_NAME}' exists without the required audit mount — recreating it."
      kind delete cluster --name "${CLUSTER_NAME}"
      kind create cluster --name "${CLUSTER_NAME}" --config "${KIND_CONFIG}" --wait 120s
      log_success "Kind cluster '${CLUSTER_NAME}' recreated"
    fi
  else
    log_info "Creating Kind cluster '${CLUSTER_NAME}'…"
    kind create cluster --name "${CLUSTER_NAME}" --config "${KIND_CONFIG}" --wait 120s
    log_success "Kind cluster '${CLUSTER_NAME}' created"
  fi

  # Set kubectl context
  kubectl config use-context "kind-${CLUSTER_NAME}"
  log_info "kubectl context set to: kind-${CLUSTER_NAME}"

  # Some environments write 0.0.0.0 into kubeconfig, which is not in the
  # API server cert SANs. Rewrite to 127.0.0.1 for local access.
  local server
  server="$(kubectl config view --raw -o jsonpath="{.clusters[?(@.name=='kind-${CLUSTER_NAME}')].cluster.server}")"
  if [[ "${server}" == "https://0.0.0.0:"* ]]; then
    kubectl config set-cluster "kind-${CLUSTER_NAME}" --server "${server/0.0.0.0/127.0.0.1}" >/dev/null
    log_warn "Rewrote kubeconfig server from 0.0.0.0 to 127.0.0.1 for TLS compatibility"
  fi
}

# ---------------------------------------------------------------------------
# Install CRD
# ---------------------------------------------------------------------------
install_crd() {
  log_step "Installing TetherLease CRD"

  kubectl apply -f "${REPO_ROOT}/config/crd/tetherlease.yaml"

  # Wait for CRD to become established
  kubectl wait --for=condition=Established \
    crd/tetherleases.tether.dev \
    --timeout=30s

  log_success "TetherLease CRD installed"
}

# ---------------------------------------------------------------------------
# Build binaries
# ---------------------------------------------------------------------------
build_binaries() {
  log_step "Building Tether binaries"

  cd "${REPO_ROOT}"
  make build
  log_success "Binaries built in ${REPO_ROOT}/bin/"
}

# ---------------------------------------------------------------------------
# Build container images
# ---------------------------------------------------------------------------
build_images() {
  log_step "Building container images"

  cd "${REPO_ROOT}"
  docker build -t "${OPERATOR_IMAGE}" -f Dockerfile.operator .
  docker build -t "${PROXY_IMAGE}" -f Dockerfile.proxy .

  kind load docker-image --name "${CLUSTER_NAME}" "${OPERATOR_IMAGE}" "${PROXY_IMAGE}"
  log_success "Container images built and loaded into Kind"
}

# ---------------------------------------------------------------------------
# Ensure proxy TLS assets
# ---------------------------------------------------------------------------
ensure_proxy_tls_assets() {
  mkdir -p "${AUDIT_DIR}" "${PID_DIR}"
  chmod 0777 "${AUDIT_DIR}"

  local tls_cert_file="${PID_DIR}/proxy.crt"
  local tls_key_file="${PID_DIR}/proxy.key"

  if [[ ! -f "${tls_cert_file}" || ! -f "${tls_key_file}" ]]; then
    log_info "Generating local TLS certificate for proxy"
    openssl req -x509 -nodes -newkey rsa:2048 \
      -keyout "${tls_key_file}" \
      -out "${tls_cert_file}" \
      -days 365 \
      -subj "/CN=localhost" \
      -addext "subjectAltName=DNS:localhost,IP:127.0.0.1" \
      >/dev/null 2>&1
  fi
}

# ---------------------------------------------------------------------------
# Reset session recording
# ---------------------------------------------------------------------------
reset_session_recording() {
  mkdir -p "${AUDIT_DIR}"
  chmod 0777 "${AUDIT_DIR}"

  local session_file="${AUDIT_DIR}/${TETHER_SESSION}.cast"
  if [[ -f "${session_file}" ]]; then
    log_info "Removing previous recording for session '${TETHER_SESSION}'"
    rm -f "${session_file}"
  fi
}

# ---------------------------------------------------------------------------
# Deploy in-cluster components
# ---------------------------------------------------------------------------
deploy_in_cluster() {
  log_step "Deploying Tether operator and proxy into the cluster"

  ensure_proxy_tls_assets
  reset_session_recording

  kubectl create namespace "${TETHER_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  kubectl -n "${TETHER_NAMESPACE}" create secret tls tether-proxy-tls \
    --cert "${PID_DIR}/proxy.crt" \
    --key "${PID_DIR}/proxy.key" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null

  kubectl apply -f - <<EOF
apiVersion: v1
kind: ServiceAccount
metadata:
  name: tether-operator
  namespace: ${TETHER_NAMESPACE}
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: tether-proxy
  namespace: ${TETHER_NAMESPACE}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: tether-operator
rules:
  - apiGroups: ["tether.dev"]
    resources: ["tetherleases"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  - apiGroups: ["tether.dev"]
    resources: ["tetherleases/status"]
    verbs: ["get", "update", "patch"]
  - apiGroups: ["tether.dev"]
    resources: ["tetherleases/finalizers"]
    verbs: ["update"]
  - apiGroups: ["rbac.authorization.k8s.io"]
    resources: ["clusterrolebindings"]
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
    namespace: ${TETHER_NAMESPACE}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: tether-operator-cluster-admin
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cluster-admin
subjects:
  - kind: ServiceAccount
    name: tether-operator
    namespace: ${TETHER_NAMESPACE}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: tether-proxy-cluster-admin
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cluster-admin
subjects:
  - kind: ServiceAccount
    name: tether-proxy
    namespace: ${TETHER_NAMESPACE}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: tether-operator
  namespace: ${TETHER_NAMESPACE}
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
          image: ${OPERATOR_IMAGE}
          imagePullPolicy: IfNotPresent
          args:
            - --metrics-bind-address=:8080
            - --health-probe-bind-address=:8081
          readinessProbe:
            httpGet:
              path: /readyz
              port: 8081
          livenessProbe:
            httpGet:
              path: /healthz
              port: 8081
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: tether-proxy
  namespace: ${TETHER_NAMESPACE}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: tether-proxy
  template:
    metadata:
      labels:
        app: tether-proxy
    spec:
      serviceAccountName: tether-proxy
      containers:
        - name: proxy
          image: ${PROXY_IMAGE}
          imagePullPolicy: IfNotPresent
          args:
            - --listen=:8443
            - --tls-cert=/tls/tls.crt
            - --tls-key=/tls/tls.key
            - --audit-dir=${AUDIT_DIR}
          env:
            - name: TETHER_TOKEN
              value: ${TETHER_TOKEN}
            - name: TETHER_SESSION_ID
              value: ${TETHER_SESSION}
            - name: TETHER_PROXY_BOOT_ID
              value: "$(date +%s)"
          ports:
            - containerPort: 8443
              name: https
          readinessProbe:
            tcpSocket:
              port: 8443
          livenessProbe:
            tcpSocket:
              port: 8443
          volumeMounts:
            - name: tls
              mountPath: /tls
              readOnly: true
            - name: audit
              mountPath: ${AUDIT_DIR}
      volumes:
        - name: tls
          secret:
            secretName: tether-proxy-tls
        - name: audit
          hostPath:
            path: ${AUDIT_DIR}
            type: DirectoryOrCreate
---
apiVersion: v1
kind: Service
metadata:
  name: tether-proxy
  namespace: ${TETHER_NAMESPACE}
spec:
  selector:
    app: tether-proxy
  ports:
    - name: https
      port: 8443
      targetPort: https
EOF

  kubectl -n "${TETHER_NAMESPACE}" rollout status deployment/tether-operator --timeout=120s
  kubectl -n "${TETHER_NAMESPACE}" rollout status deployment/tether-proxy --timeout=120s
  log_success "Tether operator and proxy are running in-cluster"
}

# ---------------------------------------------------------------------------
# Port-forward proxy service locally
# ---------------------------------------------------------------------------
start_proxy_port_forward() {
  log_step "Starting local port-forward to the in-cluster proxy"

  mkdir -p "${PID_DIR}"
  local legacy_proxy_pid_file="${PID_DIR}/proxy.pid"
  local pid_file="${PID_DIR}/proxy-port-forward.pid"
  local log_file="${PID_DIR}/proxy-port-forward.log"

  if [[ -f "${legacy_proxy_pid_file}" ]]; then
    local legacy_pid
    legacy_pid=$(cat "${legacy_proxy_pid_file}")
    if kill -0 "${legacy_pid}" 2>/dev/null; then
      log_warn "Stopping legacy local proxy process (PID ${legacy_pid})"
      kill "${legacy_pid}" || true
      sleep 1
    fi
    rm -f "${legacy_proxy_pid_file}"
  fi

  if [[ -f "${pid_file}" ]]; then
    local old_pid
    old_pid=$(cat "${pid_file}")
    if kill -0 "${old_pid}" 2>/dev/null; then
      log_warn "Stopping previous proxy port-forward (PID ${old_pid})"
      kill "${old_pid}" || true
      sleep 1
    fi
  fi

  nohup kubectl -n "${TETHER_NAMESPACE}" port-forward svc/tether-proxy "${PROXY_PORT}:8443" > "${log_file}" 2>&1 &

  echo $! > "${pid_file}"
  log_info "Proxy port-forward started (PID $(cat "${pid_file}")), logs: ${log_file}"

  local retries=15
  while [[ ${retries} -gt 0 ]]; do
    local status_code
    status_code="$(curl -sk --max-time 2 -o /dev/null -w '%{http_code}' "https://localhost:${PROXY_PORT}/version" || true)"
    if [[ "${status_code}" == "401" ]]; then
      break
    fi
    sleep 1
    retries=$((retries - 1))
  done

  if ! kill -0 "$(cat "${pid_file}")" 2>/dev/null; then
    log_error "Proxy port-forward failed to start. Check logs: ${log_file}"
    tail -20 "${log_file}" >&2
    exit 1
  fi

  log_success "Local proxy endpoint ready on https://localhost:${PROXY_PORT}"
}

# ---------------------------------------------------------------------------
# Demo TetherLease
# ---------------------------------------------------------------------------
create_demo_lease() {
  log_step "Creating demo TetherLease"

  local user
  user="$(kubectl config view --minify -o jsonpath='{.contexts[0].context.user}')"
  [[ -z "${user}" ]] && user="${USER:-developer}"

  local lease_name="demo-${user}-setup"
  DEMO_LEASE_NAME="${lease_name}"

  # Remove previous demo lease if it exists
  kubectl delete tetherlease "${lease_name}" --ignore-not-found=true &>/dev/null

  kubectl apply -f - <<EOF
apiVersion: tether.dev/v1alpha1
kind: TetherLease
metadata:
  name: ${lease_name}
spec:
  user: ${user}
  role: view
  duration: 1h
  reason: "local-setup demo lease"
EOF

  log_success "Demo TetherLease '${lease_name}' created (user: ${user}, role: view, duration: 1h)"
  echo ""
  log_info "Check the lease status:"
  log_info "  kubectl get tetherlease ${lease_name}"
  log_info "Wait for activation:"
  log_info "  kubectl wait --for=jsonpath='{.status.phase}'=Active tetherlease/${lease_name} --timeout=30s"
}

# ---------------------------------------------------------------------------
# Print summary
# ---------------------------------------------------------------------------
print_summary() {
  echo ""
  echo -e "${CYAN}${BOLD}╔══════════════════════════════════════════════════════════════╗${NC}"
  echo -e "${CYAN}${BOLD}║           Tether Local Environment Ready! 🎉                ║${NC}"
  echo -e "${CYAN}${BOLD}╚══════════════════════════════════════════════════════════════╝${NC}"
  echo ""
  echo -e "${BOLD}Cluster:${NC}       kind-${CLUSTER_NAME}"
  echo -e "${BOLD}Namespace:${NC}     ${TETHER_NAMESPACE}"
  echo -e "${BOLD}Proxy:${NC}         https://localhost:${PROXY_PORT}  (port-forward to in-cluster service)"
  echo -e "${BOLD}Audit dir:${NC}     ${AUDIT_DIR}"
  echo -e "${BOLD}Dev token:${NC}     ${TETHER_TOKEN}"
  echo -e "${BOLD}Operator logs:${NC} kubectl -n ${TETHER_NAMESPACE} logs deploy/tether-operator -f"
  echo -e "${BOLD}Proxy logs:${NC}    kubectl -n ${TETHER_NAMESPACE} logs deploy/tether-proxy -f"
  echo -e "${BOLD}PF logs:${NC}       ${PID_DIR}/proxy-port-forward.log"
  echo ""
  echo -e "${BOLD}${BLUE}Quick-start commands:${NC}"
  echo ""
  echo -e "  # 1. Check the demo lease created by setup"
  echo -e "     kubectl get tetherlease ${DEMO_LEASE_NAME}"
  echo ""
  echo -e "  # 2. Wait until the demo lease is active"
  echo -e "     kubectl wait --for=jsonpath='{.status.phase}'=Active tetherlease/${DEMO_LEASE_NAME} --timeout=30s"
  echo ""
  echo -e "  # 3. Activate session (routes kubectl through the proxy)"
  echo -e "     ./bin/tetherctl login --lease ${DEMO_LEASE_NAME} --proxy https://localhost:${PROXY_PORT} --token ${TETHER_TOKEN} --insecure-skip-tls-verify"
  echo ""
  echo -e "  # 4. Use kubectl normally — all Kubernetes API requests are recorded"
  echo -e "     kubectl get namespaces"
  echo ""
  echo -e "  # 5. Play back recorded session (dev session ID)"
  echo -e "     ./bin/tetherctl playback --lease ${TETHER_SESSION} --audit-dir ${AUDIT_DIR}"
  echo ""
  echo -e "  # 6. Tear down when done"
  echo -e "     $0 --teardown"
  echo ""
}

# ---------------------------------------------------------------------------
# Teardown
# ---------------------------------------------------------------------------
teardown() {
  log_step "Tearing down Tether local environment"

  local legacy_proxy_pid_file="${PID_DIR}/proxy.pid"
  if [[ -f "${legacy_proxy_pid_file}" ]]; then
    local legacy_proxy_pid
    legacy_proxy_pid=$(cat "${legacy_proxy_pid_file}")
    if kill -0 "${legacy_proxy_pid}" 2>/dev/null; then
      log_info "Stopping legacy local proxy (PID ${legacy_proxy_pid})"
      kill "${legacy_proxy_pid}" && rm -f "${legacy_proxy_pid_file}"
    else
      rm -f "${legacy_proxy_pid_file}"
    fi
  fi

  local pf_pid_file="${PID_DIR}/proxy-port-forward.pid"
  if [[ -f "${pf_pid_file}" ]]; then
    local pf_pid
    pf_pid=$(cat "${pf_pid_file}")
    if kill -0 "${pf_pid}" 2>/dev/null; then
      log_info "Stopping proxy port-forward (PID ${pf_pid})"
      kill "${pf_pid}" && rm -f "${pf_pid_file}"
    else
      log_warn "Proxy port-forward process ${pf_pid} not running"
      rm -f "${pf_pid_file}"
    fi
  else
    log_warn "No proxy port-forward PID file found at ${pf_pid_file}"
  fi

  # Delete Kind cluster
  if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
    log_info "Deleting Kind cluster '${CLUSTER_NAME}'"
    kind delete cluster --name "${CLUSTER_NAME}"
    log_success "Kind cluster '${CLUSTER_NAME}' deleted"
  else
    log_warn "Kind cluster '${CLUSTER_NAME}' not found"
  fi

  log_success "Teardown complete"
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
main() {
  case "${1:-}" in
    --teardown)
      teardown
      exit 0
      ;;
    --help|-h)
      usage
      exit 0
      ;;
    "")
      ;;
    *)
      log_error "Unknown option: $1"
      usage
      exit 1
      ;;
  esac

  echo -e "${CYAN}${BOLD}"
  echo "  ████████╗███████╗████████╗██╗  ██╗███████╗██████╗ "
  echo "     ██╔══╝██╔════╝╚══██╔══╝██║  ██║██╔════╝██╔══██╗"
  echo "     ██║   █████╗     ██║   ███████║█████╗  ██████╔╝"
  echo "     ██║   ██╔══╝     ██║   ██╔══██║██╔══╝  ██╔══██╗"
  echo "     ██║   ███████╗   ██║   ██║  ██║███████╗██║  ██║"
  echo "     ╚═╝   ╚══════╝   ╚═╝   ╚═╝  ╚═╝╚══════╝╚═╝  ╚═╝"
  echo -e "${NC}"
  echo -e "  ${BOLD}Local Development Setup${NC}"
  echo "  ──────────────────────────────────────────────────"
  echo ""

  check_prerequisites
  create_cluster
  install_crd
  build_binaries
  build_images
  deploy_in_cluster
  start_proxy_port_forward
  create_demo_lease
  print_summary
}

main "$@"
