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

${BOLD}WHAT THIS SCRIPT DOES${NC}
  1. Checks prerequisites (kind, kubectl, docker, go)
  2. Creates a Kind cluster (or reuses an existing one)
  3. Installs the TetherLease CRD into the cluster
  4. Builds the operator, proxy, and tetherctl binaries
  5. Starts the Tether operator in the background
  6. Starts the Tether proxy in the background
  7. Creates a demo TetherLease for the current user
  8. Prints instructions for testing

${BOLD}TESTING AFTER SETUP${NC}
  # Request access (creates a TetherLease)
  ./bin/tetherctl request --role view --for 30m --reason "local testing"

  # Activate session (updates kubeconfig to route through proxy)
  ./bin/tetherctl login --lease LEASE_NAME --proxy https://localhost:${PROXY_PORT} --token ${TETHER_TOKEN} --insecure-skip-tls-verify

  # Trigger a recordable command (exec/log are recorded)
  kubectl logs -n kube-system deployment/coredns

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

  log_success "All prerequisites satisfied"
}

# ---------------------------------------------------------------------------
# Kind cluster
# ---------------------------------------------------------------------------
create_cluster() {
  log_step "Setting up Kind cluster: ${CLUSTER_NAME}"

  if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
    log_warn "Cluster '${CLUSTER_NAME}' already exists — reusing it."
    log_info "Run '$0 --teardown' first to start with a fresh cluster."
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
# Create tether-system namespace
# ---------------------------------------------------------------------------
create_namespace() {
  log_step "Ensuring tether-system namespace exists"
  if ! kubectl get namespace tether-system &>/dev/null; then
    kubectl create namespace tether-system
    log_success "Namespace 'tether-system' created"
  else
    log_info "Namespace 'tether-system' already exists"
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
# Start operator
# ---------------------------------------------------------------------------
start_operator() {
  log_step "Starting Tether operator"

  mkdir -p "${PID_DIR}"
  local pid_file="${PID_DIR}/operator.pid"
  local log_file="${PID_DIR}/operator.log"

  # Kill any previously started operator
  if [[ -f "${pid_file}" ]]; then
    local old_pid
    old_pid=$(cat "${pid_file}")
    if kill -0 "${old_pid}" 2>/dev/null; then
      log_warn "Stopping previous operator process (PID ${old_pid})"
      kill "${old_pid}" || true
      sleep 1
    fi
  fi

  KUBECONFIG="${HOME}/.kube/config" \
    nohup "${REPO_ROOT}/bin/operator" \
      --metrics-bind-address ":8082" \
      --health-probe-bind-address ":8083" \
      --token-namespace "tether-system" \
      > "${log_file}" 2>&1 &

  echo $! > "${pid_file}"
  log_info "Operator started (PID $(cat "${pid_file}")), logs: ${log_file}"

  # Brief pause to catch immediate startup errors
  sleep 2
  if ! kill -0 "$(cat "${pid_file}")" 2>/dev/null; then
    log_error "Operator failed to start. Check logs: ${log_file}"
    tail -20 "${log_file}" >&2
    exit 1
  fi

  log_success "Tether operator running"
}

# ---------------------------------------------------------------------------
# Start proxy
# ---------------------------------------------------------------------------
start_proxy() {
  log_step "Starting Tether proxy"

  mkdir -p "${AUDIT_DIR}" "${PID_DIR}"
  local pid_file="${PID_DIR}/proxy.pid"
  local log_file="${PID_DIR}/proxy.log"
  local tls_cert_file="${PID_DIR}/proxy.crt"
  local tls_key_file="${PID_DIR}/proxy.key"

  # Kill any previously started proxy
  if [[ -f "${pid_file}" ]]; then
    local old_pid
    old_pid=$(cat "${pid_file}")
    if kill -0 "${old_pid}" 2>/dev/null; then
      log_warn "Stopping previous proxy process (PID ${old_pid})"
      kill "${old_pid}" || true
      sleep 1
    fi
  fi

  # Resolve Kind API server URL from kubeconfig
  local api_server
  api_server=$(kubectl config view \
    --minify \
    -o jsonpath='{.clusters[0].cluster.server}')

  if [[ -z "${api_server}" ]]; then
    log_error "Could not determine Kubernetes API server URL from kubeconfig."
    exit 1
  fi

  log_info "Proxying to API server: ${api_server}"

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

  nohup "${REPO_ROOT}/bin/proxy" \
      --listen ":${PROXY_PORT}" \
      --target "${api_server}" \
      --tls-skip-verify \
      --tls-cert "${tls_cert_file}" \
      --tls-key "${tls_key_file}" \
      --audit-dir "${AUDIT_DIR}" \
      --token-namespace "tether-system" \
      > "${log_file}" 2>&1 &

  echo $! > "${pid_file}"
  log_info "Proxy started (PID $(cat "${pid_file}")), logs: ${log_file}"

  # Wait for proxy to be ready
  local retries=15
  while [[ ${retries} -gt 0 ]]; do
    if curl -sf --max-time 2 "http://localhost:${PROXY_PORT}/healthz" &>/dev/null \
       || curl -sf --max-time 2 -k "https://localhost:${PROXY_PORT}/healthz" &>/dev/null; then
      break
    fi
    sleep 1
    retries=$((retries - 1))
  done

  if ! kill -0 "$(cat "${pid_file}")" 2>/dev/null; then
    log_error "Proxy failed to start. Check logs: ${log_file}"
    tail -20 "${log_file}" >&2
    exit 1
  fi

  log_success "Tether proxy listening on :${PROXY_PORT}"
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
  log_info "Watch the lease status:"
  log_info "  kubectl get tetherlease ${lease_name} -w"
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
  echo -e "${BOLD}Cluster:${NC}         kind-${CLUSTER_NAME}"
  echo -e "${BOLD}Proxy:${NC}           https://localhost:${PROXY_PORT}  (self-signed cert / dev mode)"
  echo -e "${BOLD}Audit dir:${NC}       ${AUDIT_DIR}"
  echo -e "${BOLD}Token namespace:${NC} tether-system  (operator stores session tokens here)"
  echo -e "${BOLD}Operator log:${NC}    ${PID_DIR}/operator.log"
  echo -e "${BOLD}Proxy log:${NC}       ${PID_DIR}/proxy.log"
  echo ""
  echo -e "${BOLD}${BLUE}Quick-start workflow (JIT access):${NC}"
  echo ""
  echo -e "  # 1. List existing leases"
  echo -e "     ./bin/tetherctl list"
  echo ""
  echo -e "  # 2. Request just-in-time privileged access"
  echo -e "     ./bin/tetherctl request --role view --for 30m --reason \"testing\""
  echo ""
  echo -e "  # 3. Activate session — token is auto-fetched from k8s (no --token needed)"
  echo -e "     ./bin/tetherctl login --lease LEASE_NAME --proxy https://localhost:${PROXY_PORT} --insecure-skip-tls-verify"
  echo ""
  echo -e "  # 4. Run kubectl commands (all requests recorded, session expires automatically)"
  echo -e "     kubectl get pods -A"
  echo -e "     kubectl logs -n kube-system deployment/coredns"
  echo ""
  echo -e "  # 5. Play back recorded session"
  echo -e "     ./bin/tetherctl playback --lease LEASE_NAME --audit-dir ${AUDIT_DIR}"
  echo ""
  echo -e "  # 6. Revoke access early"
  echo -e "     ./bin/tetherctl revoke --lease LEASE_NAME"
  echo ""
  echo -e "  # 7. Tear down when done"
  echo -e "     $0 --teardown"
  echo ""
}

# ---------------------------------------------------------------------------
# Teardown
# ---------------------------------------------------------------------------
teardown() {
  log_step "Tearing down Tether local environment"

  # Stop operator
  local op_pid_file="${PID_DIR}/operator.pid"
  if [[ -f "${op_pid_file}" ]]; then
    local op_pid
    op_pid=$(cat "${op_pid_file}")
    if kill -0 "${op_pid}" 2>/dev/null; then
      log_info "Stopping operator (PID ${op_pid})"
      kill "${op_pid}" && rm -f "${op_pid_file}"
    else
      log_warn "Operator process ${op_pid} not running"
      rm -f "${op_pid_file}"
    fi
  else
    log_warn "No operator PID file found at ${op_pid_file}"
  fi

  # Stop proxy
  local proxy_pid_file="${PID_DIR}/proxy.pid"
  if [[ -f "${proxy_pid_file}" ]]; then
    local proxy_pid
    proxy_pid=$(cat "${proxy_pid_file}")
    if kill -0 "${proxy_pid}" 2>/dev/null; then
      log_info "Stopping proxy (PID ${proxy_pid})"
      kill "${proxy_pid}" && rm -f "${proxy_pid_file}"
    else
      log_warn "Proxy process ${proxy_pid} not running"
      rm -f "${proxy_pid_file}"
    fi
  else
    log_warn "No proxy PID file found at ${proxy_pid_file}"
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
  create_namespace
  build_binaries
  start_operator
  start_proxy
  create_demo_lease
  print_summary
}

main "$@"
