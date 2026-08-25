#!/bin/sh
set -eu

DEPLOY_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
ROOT=$(CDPATH= cd -- "$DEPLOY_DIR/.." && pwd)
WORKFLOW="$ROOT/.github/workflows/deploy-compose.yml"
MAKEFILE="$ROOT/Makefile"
INSTALLER="$DEPLOY_DIR/install.sh"
INSTALL_CONTRACT="$DEPLOY_DIR/test-install-contract.sh"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

require_text() {
  file=$1
  text=$2
  grep -F -- "$text" "$file" >/dev/null || fail "$file is missing: $text"
}

require_order() {
  file=$1
  first=$2
  second=$3
  first_line=$(grep -nF -- "$first" "$file" | head -1 | cut -d: -f1)
  second_line=$(grep -nF -- "$second" "$file" | head -1 | cut -d: -f1)
  [ -n "$first_line" ] && [ -n "$second_line" ] && [ "$first_line" -lt "$second_line" ] ||
    fail "$file does not order '$first' before '$second'"
}

reject_pattern() {
  file=$1
  pattern=$2
  message=$3
  if grep -E -- "$pattern" "$file" >/dev/null; then fail "$message"; fi
}

for file in "$ROOT/README.md" "$DEPLOY_DIR/README.md" "$DEPLOY_DIR/DOCKER.md" "$DEPLOY_DIR/config.example.yaml"; do
  [ -s "$file" ] || fail "$file is missing or empty"
done

# Canonical copy/start commands and public bootstrap path must remain accurate.
for file in "$ROOT/README.md" "$DEPLOY_DIR/README.md" "$DEPLOY_DIR/DOCKER.md"; do
  require_text "$file" 'cp .env.example .env'
  require_text "$file" 'docker compose up -d'
done
for file in "$ROOT/README.md" "$DEPLOY_DIR/README.md"; do
  require_text "$file" 'https://raw.githubusercontent.com/is7Qin/sub2api/main/deploy/docker-deploy.sh'
done

# Git refs use vX.Y.Z, while release image tags use X.Y.Z.
require_text "$DEPLOY_DIR/DOCKER.md" 'ghcr.io/is7qin/sub2api:1.2.3'
reject_pattern "$DEPLOY_DIR/DOCKER.md" '(^|[[:space:]])[^[:space:]]*sub2api:v[0-9]+\.[0-9]+\.[0-9]+' \
  'deploy/DOCKER.md must not advertise a leading-v image tag'

# Current docs must not restore stale Viper-variable contracts.
for file in "$ROOT/README.md" "$DEPLOY_DIR/README.md" "$DEPLOY_DIR/DOCKER.md"; do
  reject_pattern "$file" '(^|[^A-Z_])(DATABASE_URL|REDIS_URL|GIN_MODE)([^A-Z_]|$)' \
    "$file contains a stale deployment variable"
done
reject_pattern "$DEPLOY_DIR/DOCKER.md" '(^|[-`|[:space:]])PORT([-`|[:space:]]|$)' \
  'deploy/DOCKER.md must document SERVER_PORT rather than bare PORT'

# The documented parser floor, tested CI pin, no-clobber contract, and migration
# warnings are cheap static gates for docs-only workflow runs.
require_text "$ROOT/README.md" 'Compose v2.24.4+'
require_text "$DEPLOY_DIR/README.md" 'Compose v2.24.4+'
require_text "$DEPLOY_DIR/README.md" 'CI uses v2.24.7'
require_text "$DEPLOY_DIR/README.md" 'destination must not exist'
require_text "$DEPLOY_DIR/README.md" 'COMPOSE_PROJECT_NAME'
require_text "$DEPLOY_DIR/README.md" 'fresh Compose project'
require_text "$DEPLOY_DIR/README.md" 'docker compose down -v'

# Both workflow path filters must include every Task 4 docs/config dependency.
for path in README.md deploy/README.md deploy/DOCKER.md deploy/config.example.yaml; do
  count=$(grep -F -- "- '$path'" "$WORKFLOW" | wc -l | tr -d ' ')
  [ "$count" = 2 ] || fail "$WORKFLOW must include $path in push and pull_request paths"
done

# Keep the Linux-only installer behavior authoritative in CI while non-Linux hosts
# retain cheap syntax and wiring coverage through this cross-platform static gate.
require_text "$MAKEFILE" 'test-compose:'
require_text "$MAKEFILE" '@$(MAKE) test-install-contract'
require_text "$MAKEFILE" 'test-install-contract:'
require_text "$MAKEFILE" '@bash deploy/test-install-contract.sh'
require_text "$INSTALL_CONTRACT" 'if [ "$host_kernel" != Linux ]; then'
require_text "$INSTALL_CONTRACT" 'SKIP: full installer/systemd contract runs on Linux CI'
require_order "$INSTALL_CONTRACT" 'if [ "$host_kernel" != Linux ]; then' 'DEPLOY_DIR=$(CDPATH='
require_order "$INSTALL_CONTRACT" 'if [ "$host_kernel" != Linux ]; then' 'TMP_DIR=$(mktemp -d)'

# The production installer itself must reject non-Linux kernels before entering main
# or reaching root, dependency, network, filesystem, account, or systemd commands.
require_text "$INSTALLER" 'require_linux_kernel() {'
require_text "$INSTALLER" 'require_linux_kernel "$installer_kernel" || exit 1'
require_order "$INSTALLER" 'installer_kernel=$(uname -s' 'require_linux_kernel "$installer_kernel" || exit 1'
require_order "$INSTALLER" 'require_linux_kernel "$installer_kernel" || exit 1' '# Colors'
require_order "$INSTALLER" 'require_linux_kernel "$installer_kernel" || exit 1' 'check_root() {'
require_order "$INSTALLER" 'require_linux_kernel "$installer_kernel" || exit 1' 'curl -s --connect-timeout'
require_order "$INSTALLER" 'require_linux_kernel "$installer_kernel" || exit 1' 'mkdir -p "$INSTALL_DIR"'
require_order "$INSTALLER" 'require_linux_kernel "$installer_kernel" || exit 1' 'useradd -r -s /bin/sh'
require_order "$INSTALLER" 'require_linux_kernel "$installer_kernel" || exit 1' 'systemctl daemon-reload'
require_order "$INSTALLER" 'require_linux_kernel "$installer_kernel" || exit 1' 'main() {'
require_order "$INSTALLER" 'require_linux_kernel "$installer_kernel" || exit 1' 'main "$@"'
require_text "$INSTALLER" 'OS="linux"'
require_text "$INSTALLER" 'x86_64)'
require_text "$INSTALLER" 'ARCH="amd64"'
require_text "$INSTALLER" 'aarch64|arm64)'
require_text "$INSTALLER" 'ARCH="arm64"'
reject_pattern "$INSTALLER" '[Dd]arwin' 'install.sh must not retain Darwin platform support'

PLATFORM_TMP=$(mktemp -d)
trap 'rm -rf "$PLATFORM_TMP"' EXIT HUP INT TERM
PLATFORM_BIN="$PLATFORM_TMP/bin"
PLATFORM_LOG="$PLATFORM_TMP/reached.log"
mkdir -p "$PLATFORM_BIN"
cat > "$PLATFORM_BIN/uname" <<'SH'
#!/bin/sh
case "${1:-}" in
  -m) printf 'x86_64\n' ;;
  *) printf 'Darwin\n' ;;
esac
SH
chmod +x "$PLATFORM_BIN/uname"
for command in id curl tar mktemp mkdir cp mv rm chmod chown useradd usermod userdel getent systemctl; do
  cat > "$PLATFORM_BIN/$command" <<'SH'
#!/bin/sh
printf '%s\n' "${0##*/}" >> "$INSTALLER_PLATFORM_PROBE_LOG"
exit 97
SH
  chmod +x "$PLATFORM_BIN/$command"
done

assert_non_linux_rejected() {
  label=$1
  shift
  : > "$PLATFORM_LOG"
  set +e
  output=$(INSTALLER_PLATFORM_PROBE_LOG="$PLATFORM_LOG" PATH="$PLATFORM_BIN:$PATH" \
    bash "$INSTALLER" "$@" 2>&1)
  status=$?
  set -e
  [ "$status" -ne 0 ] || fail "install.sh accepted Darwin for $label"
  printf '%s\n' "$output" | grep -F 'Error: deploy/install.sh supports Linux only (detected: Darwin).' >/dev/null ||
    fail "install.sh did not emit the concise Linux-only rejection for $label"
  [ ! -s "$PLATFORM_LOG" ] ||
    fail "install.sh reached forbidden command before Darwin rejection for $label: $(tr '\n' ' ' < "$PLATFORM_LOG")"
}

assert_non_linux_rejected 'default install'
assert_non_linux_rejected 'install' install
assert_non_linux_rejected 'versioned install' install -v v1.2.3
assert_non_linux_rejected 'upgrade' upgrade
assert_non_linux_rejected 'rollback' rollback v1.2.3
assert_non_linux_rejected 'uninstall' uninstall -y
assert_non_linux_rejected 'status' status
assert_non_linux_rejected 'restart' restart
assert_non_linux_rejected 'version listing' list-versions
# This installer has a universal production-entry guard; help is intentionally rejected too.
assert_non_linux_rejected 'help option' --help
assert_non_linux_rejected 'help command' help
rm -rf "$PLATFORM_TMP"
trap - EXIT HUP INT TERM

bash -n "$INSTALLER"
bash -n "$INSTALL_CONTRACT"

PYTHON=
for candidate in python3 python; do
  if "$candidate" -c 'import yaml' >/dev/null 2>&1; then PYTHON=$candidate; break; fi
done
[ -n "$PYTHON" ] || fail 'python with PyYAML is required for deployment YAML validation'

# Parse rather than grep the YAML value/location, workflow provisioning order,
# and both workflow path lists.
"$PYTHON" - "$DEPLOY_DIR/config.example.yaml" "$WORKFLOW" <<'PY'
import sys
import yaml

config_path, workflow_path = sys.argv[1:]
with open(config_path, encoding="utf-8") as handle:
    config = yaml.safe_load(handle)
retention = config.get("dashboard_aggregation", {}).get("retention", {})
assert retention.get("usage_billing_dedup_days") == 365, (
    "usage_billing_dedup_days must equal 365 under dashboard_aggregation.retention"
)
assert isinstance(retention.get("usage_logs_days"), int), "usage_logs_days must be an integer"
assert retention["usage_billing_dedup_days"] >= retention["usage_logs_days"], (
    "usage_billing_dedup_days must be >= usage_logs_days"
)

with open(workflow_path, encoding="utf-8") as handle:
    workflow = yaml.safe_load(handle)
triggers = workflow.get("on") or workflow.get(True)
assert isinstance(triggers, dict), "workflow on mapping is missing"
required = {"README.md", "deploy/README.md", "deploy/DOCKER.md", "deploy/config.example.yaml"}
for event in ("push", "pull_request"):
    paths = set(triggers.get(event, {}).get("paths", []))
    missing = required - paths
    assert not missing, f"{event} paths missing: {sorted(missing)}"

contract_job = workflow.get("jobs", {}).get("contract", {})
assert contract_job.get("runs-on") == "ubuntu-latest", (
    "deployment contract job must run on ubuntu-latest"
)
steps = contract_job.get("steps", [])
validate_index = next(
    (index for index, step in enumerate(steps) if step.get("run") == "make test-compose"),
    None,
)
assert validate_index is not None, "make test-compose validation step is missing"
setup_indices = [
    index
    for index, step in enumerate(steps)
    if step.get("uses") == "actions/setup-python@v6"
    and step.get("with", {}).get("python-version") == "3.13"
]
assert len(setup_indices) == 1, "exactly one pinned actions/setup-python@v6 Python 3.13 step is required"
install_indices = [
    index
    for index, step in enumerate(steps)
    if step.get("run") == "python -m pip install --disable-pip-version-check PyYAML==6.0.2"
]
assert len(install_indices) == 1, "exactly one PyYAML==6.0.2 install step is required"
assert setup_indices[0] < install_indices[0] < validate_index, (
    "setup-python and pinned PyYAML installation must precede make test-compose"
)
PY

printf 'PASS: deployment documentation, workflow, and config references\n'
