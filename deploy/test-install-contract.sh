#!/usr/bin/env bash
set -euo pipefail

host_kernel=$(uname -s 2>/dev/null || printf 'unknown')
if [ "$host_kernel" != Linux ]; then
  printf 'SKIP: full installer/systemd contract runs on Linux CI (non-Linux host: %s)\n' "$host_kernel"
  exit 0
fi

DEPLOY_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
ROOT=$(CDPATH= cd -- "$DEPLOY_DIR/.." && pwd)
INSTALLER="$DEPLOY_DIR/install.sh"
TEMPLATE="$DEPLOY_DIR/sub2api.service"
GORELEASER="$ROOT/.goreleaser.yaml"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

require_text() {
  local file=$1
  local text=$2
  grep -F -- "$text" "$file" >/dev/null || fail "$file is missing: $text"
}

reject_pattern() {
  local file=$1
  local pattern=$2
  local message=$3
  if grep -E -- "$pattern" "$file" >/dev/null; then
    fail "$message"
  fi
}

require_mode() {
  local file=$1
  local expected=$2
  local actual
  actual=$(stat -c '%a' "$file" 2>/dev/null || stat -f '%Lp' "$file")
  [ "$actual" = "$expected" ] || fail "$file mode is $actual, expected $expected"
}

require_order() {
  local file=$1
  local first=$2
  local second=$3
  local first_line second_line
  first_line=$(grep -nF -- "$first" "$file" | head -1 | cut -d: -f1)
  second_line=$(grep -nF -- "$second" "$file" | head -1 | cut -d: -f1)
  [ -n "$first_line" ] && [ -n "$second_line" ] && [ "$first_line" -lt "$second_line" ] ||
    fail "$file does not order '$first' before '$second'"
}

require_no_unit_temps() {
  local unit_path=$1
  local temp
  for temp in "$(dirname -- "$unit_path")"/."$(basename -- "$unit_path")".tmp.*; do
    [ ! -e "$temp" ] && [ ! -L "$temp" ] || fail "temporary unit file remains: $temp"
  done
}

function_body() {
  local name=$1
  awk -v signature="${name}() {" '
    $0 == signature { capture = 1 }
    capture { print }
    capture && /^}/ { exit }
  ' "$INSTALLER"
}

[ -s "$INSTALLER" ] || fail "missing installer: $INSTALLER"
[ -s "$TEMPLATE" ] || fail "missing systemd template: $TEMPLATE"

# Keep static release-coupling assertions in addition to executable lifecycle tests.
require_text "$INSTALLER" 'local archive_name="sub2api_${version_num}_${OS}_${ARCH}.tar.gz"'
require_text "$INSTALLER" 'local download_url="https://github.com/${GITHUB_REPO}/releases/download/${LATEST_VERSION}/${archive_name}"'
require_text "$INSTALLER" 'tar -xzf "$TEMP_DIR/$archive_name" -C "$TEMP_DIR"'
require_text "$GORELEASER" '      - deploy/*'
require_text "$GORELEASER" '      - -tags=embed'
case deploy/sub2api.service in
  deploy/*) ;;
  *) fail 'GoReleaser deploy glob does not include the installer service template' ;;
esac

latest_body=$(function_body get_latest_version)
printf '%s\n' "$latest_body" | grep -F 'return 1' >/dev/null || fail 'latest-version lookup must report failure to its caller'
if printf '%s\n' "$latest_body" | grep -E '^[[:space:]]*exit([[:space:]]|$)' >/dev/null; then
  fail 'latest-version helper must not exit past lifecycle recovery'
fi
upgrade_body=$(function_body upgrade)
printf '%s\n' "$upgrade_body" | grep -F 'get_latest_version' >/dev/null || fail 'upgrade must resolve the latest version'
printf '%s\n' "$upgrade_body" | grep -F 'download_and_extract' >/dev/null || fail 'upgrade must install the resolved archive'
version_body=$(function_body install_version)
printf '%s\n' "$version_body" | grep -F 'target_version=$(validate_version "$target_version")' >/dev/null || fail 'version install must validate its target'
printf '%s\n' "$version_body" | grep -F 'LATEST_VERSION="$target_version"' >/dev/null || fail 'rollback must download the requested release archive'
require_text "$INSTALLER" 'install_version "$target_version"'

# The selected archive contains the embed-tagged binary, so its frontend and backend
# roll back together; the installer must not fetch a separately versioned frontend.
reject_pattern "$INSTALLER" 'releases/download/[^[:space:]]*(frontend|dist)' \
  'installer must not use a separately versioned frontend release URL'

# install.sh must render the shipped template rather than own a second full unit.
reject_pattern "$INSTALLER" '^\[Unit\]$' 'install.sh still contains an inline systemd unit'
require_text "$INSTALLER" 'local service_template="$INSTALL_DIR/sub2api.service"'
require_text "$INSTALLER" 'render_service "$service_template"'
require_text "$ROOT/Makefile" 'test-install-contract:'
require_text "$ROOT/Makefile" '@$(MAKE) test-install-contract'
for path in deploy/install.sh deploy/sub2api.service deploy/test-install-contract.sh; do
  count=$(grep -F -- "- '$path'" "$ROOT/.github/workflows/deploy-compose.yml" | wc -l | tr -d ' ')
  [ "$count" = 2 ] || fail "deployment workflow must include $path in push and pull_request paths"
done

# The daemon implementation was removed and the backend now always reports the
# feature as deprecated, so stale deployment ownership must not remain.
[ ! -d "$ROOT/datamanagement" ] || fail 'unexpected supported datamanagement daemon source'
require_text "$ROOT/backend/internal/service/data_management_service.go" 'DataManagementDeprecatedReason'
require_text "$ROOT/backend/internal/service/data_management_service.go" '"data management feature is deprecated"'
for obsolete in \
  "$DEPLOY_DIR/DATAMANAGEMENTD_CN.md" \
  "$DEPLOY_DIR/install-datamanagementd.sh" \
  "$DEPLOY_DIR/sub2api-datamanagementd.service"; do
  [ ! -e "$obsolete" ] || fail "obsolete datamanagement deployment artifact remains: $obsolete"
done
reject_pattern "$ROOT/Makefile" '(^|[[:space:]])(build|test)-datamanagementd([:[:space:]]|$)' \
  'root Makefile still advertises the unsupported datamanagement daemon'

for obsolete in \
  "$DEPLOY_DIR/Dockerfile" \
  "$DEPLOY_DIR/Makefile" \
  "$DEPLOY_DIR/build_image.sh"; do
  [ ! -e "$obsolete" ] || fail "obsolete deploy build asset remains: $obsolete"
done

TMP_DIR=$(mktemp -d)
trap 'rm -rf "$TMP_DIR"' EXIT
mkdir -p "$TMP_DIR/source"
awk '$0 != "main \"$@\"" { print }' "$INSTALLER" > "$TMP_DIR/source/install-functions.sh"
# shellcheck disable=SC1090
source "$TMP_DIR/source/install-functions.sh"

# Configuration values are security boundaries because they become systemd syntax.
for host in '0.0.0.0' '127.0.0.1' 'api.example.com' 'localhost' '::' '::1' '2001:db8::1' '2001:db8:0:1:2:3:4:5'; do
  validate_server_host "$host" || fail "supported SERVER_HOST was rejected: $host"
done
for host in \
  $'host\nEnvironment=EVIL=1' \
  $'host\rRestart=no' \
  $'host\tname' \
  $'host\001name' \
  'host name' \
  'host;Restart=no' \
  'host%n' \
  '"host"' \
  '2001:::1' \
  '1:2:3:4:5:6:7::8' \
  '1:2:3:4:5:6:7:8:9' \
  '999.1.1.1' \
  '-bad.example' \
  'bad_.example'; do
  if validate_server_host "$host"; then
    fail "unsafe or unsupported SERVER_HOST was accepted: $(printf %q "$host")"
  fi
done

for user in 'sub2api' '_sub2api' 'fixture-user_2'; do
  validate_service_user "$user" || fail "supported SERVICE_USER was rejected: $user"
done
for user in 'root%N' 'bad user' $'bad\nUser=root' '../root' 'bad"user' 'bad\user'; do
  if validate_service_user "$user"; then
    fail "unsafe SERVICE_USER was accepted: $(printf %q "$user")"
  fi
done

for path in '/opt/sub2api' '/srv/Sub2API_1.2-release' '/var/lib/sub2api.data' '/srv/blue/-candidate'; do
  validate_install_dir "$path" || fail "supported INSTALL_DIR was rejected: $path"
done
for path in '/' 'relative/path' '/opt//sub2api' '/opt/../root' '/opt/./sub2api' '/opt/sub2api/' '/opt/sub 2api' '/opt/sub%N' $'/opt/sub2api\nExecStart=/bin/sh' '/opt/"sub2api' '/opt/sub\2api'; do
  if validate_install_dir "$path"; then
    fail "unsafe INSTALL_DIR was accepted: $(printf %q "$path")"
  fi
done

setup_service_fixture() {
  local name=$1
  TEST_ROOT="$TMP_DIR/$name"
  INSTALL_DIR="$TEST_ROOT/opt/Sub2API_1.2-release"
  SERVICE_USER='fixture-user_2'
  SERVER_HOST='2001:db8::7'
  SERVER_PORT='18080'
  SYSTEMD_UNIT_PATH="$TEST_ROOT/systemd/sub2api.service"
  SYSTEMCTL_LOG="$TEST_ROOT/systemctl.log"
  OWNERSHIP_LOG="$TEST_ROOT/ownership.log"
  mkdir -p "$INSTALL_DIR" "$(dirname -- "$SYSTEMD_UNIT_PATH")"
  cp "$TEMPLATE" "$INSTALL_DIR/sub2api.service"
  : > "$SYSTEMCTL_LOG"
  : > "$OWNERSHIP_LOG"
}

TEST_UID=1000
FAIL_CHOWN=false
id() {
  if [ "${1:-}" = '-u' ]; then
    printf '%s\n' "$TEST_UID"
  else
    return 0
  fi
}
chown() {
  printf '%s\n' "$*" >> "$OWNERSHIP_LOG"
  [ "$FAIL_CHOWN" != true ]
}
systemctl() {
  printf '%s\n' "$*" >> "$SYSTEMCTL_LOG"
}

setup_service_fixture service-render
umask 000
install_service
umask 022
require_mode "$SYSTEMD_UNIT_PATH" 644
require_text "$SYSTEMD_UNIT_PATH" 'After=network-online.target'
require_text "$SYSTEMD_UNIT_PATH" 'Wants=network-online.target'
require_text "$SYSTEMD_UNIT_PATH" 'User=fixture-user_2'
require_text "$SYSTEMD_UNIT_PATH" 'Group=fixture-user_2'
require_text "$SYSTEMD_UNIT_PATH" "WorkingDirectory=\"$INSTALL_DIR\""
require_text "$SYSTEMD_UNIT_PATH" "ExecStart=\"$INSTALL_DIR/sub2api\""
require_text "$SYSTEMD_UNIT_PATH" "ReadWritePaths=\"$INSTALL_DIR\""
require_text "$SYSTEMD_UNIT_PATH" 'Environment="GIN_MODE=release"'
require_text "$SYSTEMD_UNIT_PATH" 'Environment="SERVER_HOST=2001:db8::7"'
require_text "$SYSTEMD_UNIT_PATH" 'Environment="SERVER_PORT=18080"'
reject_pattern "$SYSTEMD_UNIT_PATH" '@(SERVICE_USER|INSTALL_DIR|SERVER_HOST|SERVER_PORT)@' \
  'rendered systemd unit contains unresolved template placeholders'
reject_pattern "$SYSTEMD_UNIT_PATH" '(postgresql|redis)\.service' \
  'binary installs must not depend on optional local PostgreSQL or Redis units'
require_text "$SYSTEMCTL_LOG" 'daemon-reload'
require_no_unit_temps "$SYSTEMD_UNIT_PATH"

# Existing regular units are atomically replaced and normalized from any old mode.
printf 'old unit bytes\n' > "$SYSTEMD_UNIT_PATH"
chmod 0600 "$SYSTEMD_UNIT_PATH"
install_service
require_mode "$SYSTEMD_UNIT_PATH" 644
reject_pattern "$SYSTEMD_UNIT_PATH" '^old unit bytes$' 'existing unit was not replaced'

# Unsafe destination types are rejected without following or replacing them.
printf 'symlink sentinel\n' > "$TEST_ROOT/symlink-sentinel"
rm -f "$SYSTEMD_UNIT_PATH"
if ln -s "$TEST_ROOT/symlink-sentinel" "$SYSTEMD_UNIT_PATH" 2>/dev/null && [ -L "$SYSTEMD_UNIT_PATH" ]; then
  if install_service >/dev/null 2>&1; then
    fail 'symlink systemd destination was accepted'
  fi
  require_text "$TEST_ROOT/symlink-sentinel" 'symlink sentinel'
  [ -L "$SYSTEMD_UNIT_PATH" ] || fail 'symlink destination was replaced'
else
  printf 'SKIP: host cannot create symlinks; POSIX CI runs installer symlink regression\n'
fi
rm -rf "$SYSTEMD_UNIT_PATH"
mkdir "$SYSTEMD_UNIT_PATH"
if install_service >/dev/null 2>&1; then
  fail 'directory systemd destination was accepted'
fi
[ -d "$SYSTEMD_UNIT_PATH" ] || fail 'directory destination was replaced'
require_no_unit_temps "$SYSTEMD_UNIT_PATH"

# A render that fails after producing partial output must leave the prior unit intact.
rm -rf "$SYSTEMD_UNIT_PATH"
printf 'previous unit survives render failure\n' > "$SYSTEMD_UNIT_PATH"
if (
  printf_count=0
  printf() {
    printf_count=$((printf_count + 1))
    if [ "$printf_count" -ge 5 ]; then
      return 1
    fi
    builtin printf "$@"
  }
  install_service
) >/dev/null 2>&1; then
  fail 'partial render failure unexpectedly installed a unit'
fi
require_text "$SYSTEMD_UNIT_PATH" 'previous unit survives render failure'
require_no_unit_temps "$SYSTEMD_UNIT_PATH"

# Root ownership is explicit and a failed ownership operation preserves the old unit.
TEST_UID=0
FAIL_CHOWN=false
install_service
require_text "$OWNERSHIP_LOG" '0:0'
require_mode "$SYSTEMD_UNIT_PATH" 644
printf 'previous unit survives ownership failure\n' > "$SYSTEMD_UNIT_PATH"
FAIL_CHOWN=true
if install_service >/dev/null 2>&1; then
  fail 'root ownership failure unexpectedly installed a unit'
fi
require_text "$SYSTEMD_UNIT_PATH" 'previous unit survives ownership failure'
require_no_unit_temps "$SYSTEMD_UNIT_PATH"

# A failed same-directory rename also preserves the prior unit and cleans staging.
FAIL_CHOWN=false
mv() {
  return 1
}
if install_service >/dev/null 2>&1; then
  fail 'rename failure unexpectedly installed a unit'
fi
unset -f mv
require_text "$SYSTEMD_UNIT_PATH" 'previous unit survives ownership failure'
require_no_unit_temps "$SYSTEMD_UNIT_PATH"
TEST_UID=1000
FAIL_CHOWN=false

# Build release-layout fixture archives and exercise the real lifecycle functions.
RELEASE_DIR="$TMP_DIR/releases"
mkdir -p "$RELEASE_DIR"
make_release() {
  local version=$1
  local marker=$2
  local version_num=${version#v}
  local build="$TMP_DIR/build-$version_num"
  rm -rf "$build"
  mkdir -p "$build/deploy"
  cat > "$build/sub2api" <<EOF
#!/usr/bin/env bash
if [ "\${1:-}" = "--version" ]; then
  printf 'sub2api $version\\n'
else
  printf '$marker\\n'
fi
EOF
  chmod 0755 "$build/sub2api"
  cp "$TEMPLATE" "$build/deploy/sub2api.service"
  printf '# fixture-template-%s\n' "$version" >> "$build/deploy/sub2api.service"
  tar -czf "$RELEASE_DIR/sub2api_${version_num}_linux_amd64.tar.gz" -C "$build" sub2api deploy
}
make_release v1.0.0 old-release
make_release v2.0.0 latest-release
make_release v1.5.0 rollback-release

# Trap behavior must be checked in subprocesses: signal disposition and shell exit
# cannot be characterized reliably from the contract's own process.
TRAP_PROBE="$TMP_DIR/trap-probe.sh"
cat > "$TRAP_PROBE" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
operation=$1
outcome=$2
functions_file=$3
template=$4
archive=$5
probe_root=$6
mkdir -p "$probe_root/install" "$probe_root/systemd" "$probe_root/tmp"
export TMPDIR="$probe_root/tmp"
# shellcheck disable=SC1090
source "$functions_file"
INSTALL_DIR="$probe_root/install"
SYSTEMD_UNIT_PATH="$probe_root/systemd/sub2api.service"
SERVICE_USER=fixture-user
SERVER_HOST=127.0.0.1
SERVER_PORT=18080
OS=linux
ARCH=amd64
LATEST_VERSION=v2.0.0
cp "$template" "$INSTALL_DIR/sub2api.service"
printf 'old destination bytes\n' > "$SYSTEMD_UNIT_PATH"
printf 'old binary bytes\n' > "$INSTALL_DIR/sub2api"
systemctl() { :; }
id() { [ "${1:-}" != -u ] || printf '1000\n'; }
chown() { :; }

trap 'printf "outer-exit\n" >> "$probe_root/outer.log"' EXIT
trap 'printf "outer-return\n" >> "$probe_root/outer.log"' RETURN
trap 'printf "outer-hup\n" >> "$probe_root/outer.log"' HUP
trap 'printf "outer-int\n" >> "$probe_root/outer.log"' INT
trap 'printf "outer-term\n" >> "$probe_root/outer.log"' TERM
before_exit=$(trap -p EXIT)
before_return=$(trap -p RETURN)
before_hup=$(trap -p HUP)
before_int=$(trap -p INT)
before_term=$(trap -p TERM)

if [ "$operation" = service ]; then
  if [ "$outcome" = error ]; then
    render_service() { printf 'partial unit\n'; return 1; }
  fi
  set +e
  install_service >/dev/null 2>&1
  status=$?
  set -e
else
  curl() {
    local output='' previous='' arg
    for arg in "$@"; do
      if [ "$previous" = -o ]; then output=$arg; previous=''; continue; fi
      [ "$arg" != -o ] || previous=-o
    done
    if [[ "$output" == *.tar.gz ]]; then
      [ "$outcome" != error ] || return 1
      cp "$archive" "$output"
    else
      return 1
    fi
  }
  set +e
  download_and_extract >/dev/null 2>&1
  status=$?
  set -e
fi

[ "$outcome" != success ] || [ "$status" -eq 0 ]
[ "$outcome" != error ] || [ "$status" -ne 0 ]
[ "$(trap -p EXIT)" = "$before_exit" ]
[ "$(trap -p RETURN)" = "$before_return" ]
[ "$(trap -p HUP)" = "$before_hup" ]
[ "$(trap -p INT)" = "$before_int" ]
[ "$(trap -p TERM)" = "$before_term" ]
[ "${#STAGING_CONTEXT_KIND[@]}" -eq 0 ]
[ "${#STAGING_CONTEXT_PRIMARY[@]}" -eq 0 ]
[ "${#STAGING_CONTEXT_SECONDARY[@]}" -eq 0 ]
[ "${#STAGING_CONTEXT_HUP[@]}" -eq 0 ]
[ "${#STAGING_CONTEXT_INT[@]}" -eq 0 ]
[ "${#STAGING_CONTEXT_TERM[@]}" -eq 0 ]
trap - EXIT RETURN HUP INT TERM
EOF
chmod +x "$TRAP_PROBE"

for operation in service download; do
  for outcome in success error; do
    bash -T "$TRAP_PROBE" "$operation" "$outcome" "$TMP_DIR/source/install-functions.sh" \
      "$TEMPLATE" "$RELEASE_DIR/sub2api_2.0.0_linux_amd64.tar.gz" \
      "$TMP_DIR/trap-restore-$operation-$outcome" ||
      fail "$operation $outcome return did not restore pre-existing traps"
  done
done

# Ordinary teardown must release its complete context before any caller signal
# definition becomes reachable again. Inject immediately after restoration so the
# caller handler can observe whether the owning context was already released.
ORDINARY_TEARDOWN_PROBE="$TMP_DIR/ordinary-teardown-probe.sh"
cat > "$ORDINARY_TEARDOWN_PROBE" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
operation=$1
outcome=$2
signal=$3
functions_file=$4
template=$5
archive=$6
probe_root=$7
mkdir -p "$probe_root/install" "$probe_root/systemd" "$probe_root/tmp"
export TMPDIR="$probe_root/tmp"
# shellcheck disable=SC1090
source "$functions_file"
INSTALL_DIR="$probe_root/install"
SYSTEMD_UNIT_PATH="$probe_root/systemd/sub2api.service"
SERVICE_USER=fixture-user
SERVER_HOST=127.0.0.1
SERVER_PORT=18080
OS=linux
ARCH=amd64
LATEST_VERSION=v2.0.0
cp "$template" "$INSTALL_DIR/sub2api.service"
printf 'old destination bytes\n' > "$SYSTEMD_UNIT_PATH"
printf 'old binary bytes\n' > "$INSTALL_DIR/sub2api"
systemctl() { :; }
id() { [ "${1:-}" != -u ] || printf '1000\n'; }
chown() { :; }

record_prior() {
  local caught_signal=$1
  local exit_status=$2
  printf 'outer-%s\n' "$caught_signal" >> "$probe_root/events"
  printf '%s %s %s %s %s %s\n' \
    "${#STAGING_CONTEXT_KIND[@]}" \
    "${#STAGING_CONTEXT_PRIMARY[@]}" \
    "${#STAGING_CONTEXT_SECONDARY[@]}" \
    "${#STAGING_CONTEXT_HUP[@]}" \
    "${#STAGING_CONTEXT_INT[@]}" \
    "${#STAGING_CONTEXT_TERM[@]}" > "$probe_root/contexts-at-signal"
  exit "$exit_status"
}
case "$signal" in
  HUP) trap 'record_prior HUP 71' HUP ;;
  INT) trap 'record_prior INT 72' INT ;;
  TERM) trap 'record_prior TERM 73' TERM ;;
esac

# Support both the old context-ID restorer and the required snapshot restorer so
# this fixture reproduces the ordering defect before and after the API correction.
restore_staging_signal_traps() {
  local saved_hup saved_int saved_term
  if [ "$#" -eq 1 ]; then
    saved_hup="${STAGING_CONTEXT_HUP[$1]-}"
    saved_int="${STAGING_CONTEXT_INT[$1]-}"
    saved_term="${STAGING_CONTEXT_TERM[$1]-}"
  else
    saved_hup=$1
    saved_int=$2
    saved_term=$3
  fi
  restore_trap_definition "$saved_hup" HUP
  restore_trap_definition "$saved_int" INT
  restore_trap_definition "$saved_term" TERM
  kill -s "$signal" "$BASHPID"
  printf 'continued-restorer\n' >> "$probe_root/events"
}

if [ "$outcome" = mktemp ]; then
  mktemp() { return 1; }
fi
if [ "$operation" = service ]; then
  if [ "$outcome" = error ]; then
    render_service() { printf 'partial staged unit\n'; return 1; }
  fi
  install_service
else
  curl() {
    local output='' previous='' arg
    for arg in "$@"; do
      if [ "$previous" = -o ]; then output=$arg; previous=''; continue; fi
      [ "$arg" != -o ] || previous=-o
    done
    if [[ "$output" == *.tar.gz ]]; then
      [ "$outcome" != error ] || return 1
      cp "$archive" "$output"
    else
      return 1
    fi
  }
  download_and_extract
fi
printf 'continued-operation\n' >> "$probe_root/events"
EOF
chmod +x "$ORDINARY_TEARDOWN_PROBE"

for operation in service download; do
  for outcome in success error mktemp; do
    for signal in HUP INT TERM; do
      probe_root="$TMP_DIR/ordinary-teardown-$operation-$outcome-$signal"
      case "$signal" in HUP) expected_status=71 ;; INT) expected_status=72 ;; TERM) expected_status=73 ;; esac
      set +e
      bash "$ORDINARY_TEARDOWN_PROBE" "$operation" "$outcome" "$signal" \
        "$TMP_DIR/source/install-functions.sh" "$TEMPLATE" \
        "$RELEASE_DIR/sub2api_2.0.0_linux_amd64.tar.gz" "$probe_root" \
        >/dev/null 2>&1
      status=$?
      set -e
      [ "$status" -eq "$expected_status" ] ||
        fail "$operation $outcome ordinary teardown $signal exited $status, expected $expected_status"
      require_text "$probe_root/contexts-at-signal" '0 0 0 0 0 0'
      marker_count=$(grep -Fxc -- "outer-$signal" "$probe_root/events" 2>/dev/null || true)
      [ "$marker_count" -eq 1 ] ||
        fail "$operation $outcome ordinary teardown $signal invoked prior handler $marker_count times"
      reject_pattern "$probe_root/events" '^continued-' \
        "$operation $outcome ordinary teardown continued after $signal"
      require_no_unit_temps "$probe_root/systemd/sub2api.service"
      for staged in "$probe_root/install"/.sub2api.tmp.* "$probe_root/tmp"/*; do
        [ ! -e "$staged" ] && [ ! -L "$staged" ] ||
          fail "temporary resource remains after $operation $outcome ordinary teardown $signal: $staged"
      done
      if [ "$outcome" != success ]; then
        require_text "$probe_root/systemd/sub2api.service" 'old destination bytes'
        require_text "$probe_root/install/sub2api" 'old binary bytes'
      fi
    done
  done
done

# A begin failure has no owned context to tear down. It must leave every caller
# trap and unrelated context entry untouched rather than acting on a stale ID.
BEGIN_FAILURE_PROBE="$TMP_DIR/begin-failure-probe.sh"
cat > "$BEGIN_FAILURE_PROBE" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
operation=$1
functions_file=$2
template=$3
probe_root=$4
mkdir -p "$probe_root/install" "$probe_root/systemd" "$probe_root/tmp"
export TMPDIR="$probe_root/tmp"
# shellcheck disable=SC1090
source "$functions_file"
INSTALL_DIR="$probe_root/install"
SYSTEMD_UNIT_PATH="$probe_root/systemd/sub2api.service"
SERVICE_USER=fixture-user
SERVER_HOST=127.0.0.1
SERVER_PORT=18080
OS=linux
ARCH=amd64
LATEST_VERSION=v2.0.0
cp "$template" "$INSTALL_DIR/sub2api.service"
printf 'old destination bytes\n' > "$SYSTEMD_UNIT_PATH"
printf 'old binary bytes\n' > "$INSTALL_DIR/sub2api"
systemctl() { :; }
id() { [ "${1:-}" != -u ] || printf '1000\n'; }
chown() { :; }
curl() { return 1; }
trap ':' HUP
trap ':' INT
trap ':' TERM
trap ':' RETURN
trap ':' EXIT
before_hup=$(trap -p HUP)
before_int=$(trap -p INT)
before_term=$(trap -p TERM)
before_return=$(trap -p RETURN)
before_exit=$(trap -p EXIT)

STAGING_CONTEXT_ID=900
STAGING_CONTEXT_KIND[900]=unrelated
STAGING_CONTEXT_PRIMARY[900]=unrelated-primary
STAGING_CONTEXT_SECONDARY[900]=unrelated-secondary
STAGING_CONTEXT_HUP[900]=unrelated-hup
STAGING_CONTEXT_INT[900]=unrelated-int
STAGING_CONTEXT_TERM[900]=unrelated-term
begin_staging_context() { return 1; }

set +e
if [ "$operation" = service ]; then
  install_service >/dev/null 2>&1
else
  download_and_extract >/dev/null 2>&1
fi
status=$?
set -e
[ "$status" -ne 0 ]
[ "$(trap -p HUP)" = "$before_hup" ]
[ "$(trap -p INT)" = "$before_int" ]
[ "$(trap -p TERM)" = "$before_term" ]
[ "$(trap -p RETURN)" = "$before_return" ]
[ "$(trap -p EXIT)" = "$before_exit" ]
[ "${STAGING_CONTEXT_KIND[900]}" = unrelated ]
[ "${STAGING_CONTEXT_PRIMARY[900]}" = unrelated-primary ]
[ "${STAGING_CONTEXT_SECONDARY[900]}" = unrelated-secondary ]
[ "${STAGING_CONTEXT_HUP[900]}" = unrelated-hup ]
[ "${STAGING_CONTEXT_INT[900]}" = unrelated-int ]
[ "${STAGING_CONTEXT_TERM[900]}" = unrelated-term ]
release_staging_context 900
[ "${#STAGING_CONTEXT_KIND[@]}" -eq 0 ]
[ "${#STAGING_CONTEXT_PRIMARY[@]}" -eq 0 ]
[ "${#STAGING_CONTEXT_SECONDARY[@]}" -eq 0 ]
[ "${#STAGING_CONTEXT_HUP[@]}" -eq 0 ]
[ "${#STAGING_CONTEXT_INT[@]}" -eq 0 ]
[ "${#STAGING_CONTEXT_TERM[@]}" -eq 0 ]
trap - HUP INT TERM RETURN EXIT
EOF
chmod +x "$BEGIN_FAILURE_PROBE"

for operation in service download; do
  bash -T "$BEGIN_FAILURE_PROBE" "$operation" "$TMP_DIR/source/install-functions.sh" \
    "$TEMPLATE" "$TMP_DIR/begin-failure-$operation" ||
    fail "$operation begin failure changed caller trap or unrelated context state"
done

# Setup is a separate ownership phase: caller traps are snapshotted before any
# allocation, all staging signals are ignored while allocation/installation can
# fail, and operation code starts only after all three context handlers exist.
# Wrap the real allocator or Bash trap boundary in a sourced subprocess; no
# production test hook is needed.
SETUP_BOUNDARY_PROBE="$TMP_DIR/setup-boundary-probe.sh"
cat > "$SETUP_BOUNDARY_PROBE" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
operation=$1
boundary=$2
signal=$3
handler_mode=$4
functions_file=$5
template=$6
probe_root=$7
mkdir -p "$probe_root/install" "$probe_root/systemd" "$probe_root/tmp"
export TMPDIR="$probe_root/tmp"
# shellcheck disable=SC1090
source "$functions_file"
INSTALL_DIR="$probe_root/install"
SYSTEMD_UNIT_PATH="$probe_root/systemd/sub2api.service"
SERVICE_USER=fixture-user
SERVER_HOST=127.0.0.1
SERVER_PORT=18080
OS=linux
ARCH=amd64
LATEST_VERSION=v2.0.0
cp "$template" "$INSTALL_DIR/sub2api.service"
printf 'old destination bytes\n' > "$SYSTEMD_UNIT_PATH"
printf 'old binary bytes\n' > "$INSTALL_DIR/sub2api"
systemctl() { :; }
id() { [ "${1:-}" != -u ] || printf '1000\n'; }
chown() { :; }

case "$handler_mode:$signal" in
  prior-exits:HUP) trap 'printf "outer-HUP\n" >> "$probe_root/events"; exit 71' HUP ;;
  prior-exits:INT) trap 'printf "outer-INT\n" >> "$probe_root/events"; exit 72' INT ;;
  prior-exits:TERM) trap 'printf "outer-TERM\n" >> "$probe_root/events"; exit 73' TERM ;;
  prior-returns:HUP) trap 'printf "outer-HUP\n" >> "$probe_root/events"' HUP ;;
  prior-returns:INT) trap 'printf "outer-INT\n" >> "$probe_root/events"' INT ;;
  prior-returns:TERM) trap 'printf "outer-TERM\n" >> "$probe_root/events"' TERM ;;
esac
trap ':' RETURN
trap ':' EXIT
before_hup=$(trap -p HUP)
before_int=$(trap -p INT)
before_term=$(trap -p TERM)
before_return=$(trap -p RETURN)
before_exit=$(trap -p EXIT)

STAGING_CONTEXT_KIND[900]=unrelated
STAGING_CONTEXT_PRIMARY[900]=unrelated-primary
STAGING_CONTEXT_SECONDARY[900]=unrelated-secondary
STAGING_CONTEXT_HUP[900]=unrelated-hup
STAGING_CONTEXT_INT[900]=unrelated-int
STAGING_CONTEXT_TERM[900]=unrelated-term

if [ "$boundary" = allocation ]; then
  original_begin_definition=$(declare -f begin_staging_context)
  eval "${original_begin_definition/begin_staging_context/original_begin_staging_context}"
  begin_staging_context() {
    original_begin_staging_context "$@" || return
    kill -s "$signal" "$BASHPID"
    return 1
  }
else
  target_handler=${boundary#install-}
  boundary_triggered=false
  trap() {
    if [ "$boundary_triggered" = false ] && [ "$#" -eq 2 ] &&
       [[ "${1:-}" == handle_staging_signal\ * ]] && [ "${2:-}" = "$target_handler" ]; then
      boundary_triggered=true
      kill -s "$signal" "$BASHPID"
      return 1
    fi
    builtin trap "$@"
  }
fi

render_service() {
  printf 'unsafe-service-operation\n' >> "$probe_root/events"
  return 1
}
curl() {
  printf 'unsafe-download-operation\n' >> "$probe_root/events"
  return 1
}

set +e
if [ "$operation" = service ]; then
  install_service >/dev/null 2>&1
else
  download_and_extract >/dev/null 2>&1
fi
status=$?
set -e
printf '%s\n' "$status" > "$probe_root/status"

[ "$(trap -p HUP)" = "$before_hup" ]
[ "$(trap -p INT)" = "$before_int" ]
[ "$(trap -p TERM)" = "$before_term" ]
[ "$(trap -p RETURN)" = "$before_return" ]
[ "$(trap -p EXIT)" = "$before_exit" ]
printf 'restored\n' > "$probe_root/traps"
printf '%s %s %s %s %s %s\n' \
  "${#STAGING_CONTEXT_KIND[@]}" \
  "${#STAGING_CONTEXT_PRIMARY[@]}" \
  "${#STAGING_CONTEXT_SECONDARY[@]}" \
  "${#STAGING_CONTEXT_HUP[@]}" \
  "${#STAGING_CONTEXT_INT[@]}" \
  "${#STAGING_CONTEXT_TERM[@]}" > "$probe_root/contexts"
[ "${STAGING_CONTEXT_KIND[900]}" = unrelated ]
[ "${STAGING_CONTEXT_PRIMARY[900]}" = unrelated-primary ]
[ "${STAGING_CONTEXT_SECONDARY[900]}" = unrelated-secondary ]
[ "${STAGING_CONTEXT_HUP[900]}" = unrelated-hup ]
[ "${STAGING_CONTEXT_INT[900]}" = unrelated-int ]
[ "${STAGING_CONTEXT_TERM[900]}" = unrelated-term ]
release_staging_context 900
printf '%s %s %s %s %s %s\n' \
  "${#STAGING_CONTEXT_KIND[@]}" \
  "${#STAGING_CONTEXT_PRIMARY[@]}" \
  "${#STAGING_CONTEXT_SECONDARY[@]}" \
  "${#STAGING_CONTEXT_HUP[@]}" \
  "${#STAGING_CONTEXT_INT[@]}" \
  "${#STAGING_CONTEXT_TERM[@]}" > "$probe_root/contexts-after-release"
trap - HUP INT TERM RETURN EXIT
exit "$status"
EOF
chmod +x "$SETUP_BOUNDARY_PROBE"

for operation in service download; do
  for signal in HUP INT TERM; do
    for boundary in allocation install-HUP install-INT install-TERM; do
      for handler_mode in default prior-exits prior-returns; do
        probe_root="$TMP_DIR/setup-$operation-$boundary-$signal-$handler_mode"
        set +e
        bash "$SETUP_BOUNDARY_PROBE" "$operation" "$boundary" "$signal" "$handler_mode" \
          "$TMP_DIR/source/install-functions.sh" "$TEMPLATE" "$probe_root" \
          >/dev/null 2>&1
        status=$?
        set -e
        [ "$status" -eq 1 ] ||
          fail "$operation $boundary setup failure under $signal/$handler_mode exited $status, expected 1"
        require_text "$probe_root/status" '1'
        require_text "$probe_root/traps" 'restored'
        require_text "$probe_root/contexts" '1 1 1 1 1 1'
        require_text "$probe_root/contexts-after-release" '0 0 0 0 0 0'
        require_text "$probe_root/systemd/sub2api.service" 'old destination bytes'
        require_text "$probe_root/install/sub2api" 'old binary bytes'
        marker_count=$(grep -Ec '^(outer-|unsafe-)' "$probe_root/events" 2>/dev/null || true)
        marker_count=${marker_count:-0}
        [ "$marker_count" -eq 0 ] ||
          fail "$operation $boundary setup signal $signal/$handler_mode invoked a caller or operation marker"
        require_no_unit_temps "$probe_root/systemd/sub2api.service"
        for staged in "$probe_root/install"/.sub2api.tmp.* "$probe_root/tmp"/*; do
          [ ! -e "$staged" ] && [ ! -L "$staged" ] ||
            fail "temporary resource remains after $operation $boundary setup failure under $signal/$handler_mode: $staged"
        done
      done
    done
  done
done

SIGNAL_PROBE="$TMP_DIR/signal-probe.sh"
cat > "$SIGNAL_PROBE" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
operation=$1
signal=$2
handler_mode=$3
functions_file=$4
template=$5
archive=$6
probe_root=$7
mkdir -p "$probe_root/install" "$probe_root/systemd" "$probe_root/tmp"
export TMPDIR="$probe_root/tmp"
# shellcheck disable=SC1090
source "$functions_file"
INSTALL_DIR="$probe_root/install"
SYSTEMD_UNIT_PATH="$probe_root/systemd/sub2api.service"
SERVICE_USER=fixture-user
SERVER_HOST=127.0.0.1
SERVER_PORT=18080
OS=linux
ARCH=amd64
LATEST_VERSION=v2.0.0
cp "$template" "$INSTALL_DIR/sub2api.service"
printf 'old destination bytes\n' > "$SYSTEMD_UNIT_PATH"
printf 'old binary bytes\n' > "$INSTALL_DIR/sub2api"
systemctl() { :; }
id() { [ "${1:-}" != -u ] || printf '1000\n'; }
chown() { :; }

case "$handler_mode:$signal" in
  prior:HUP) trap 'printf "outer-HUP\n" >> "$probe_root/events"; exit 71' HUP ;;
  prior:INT) trap 'printf "outer-INT\n" >> "$probe_root/events"; exit 72' INT ;;
  prior:TERM) trap 'printf "outer-TERM\n" >> "$probe_root/events"; exit 73' TERM ;;
  prior-return:HUP) trap 'printf "outer-HUP\n" >> "$probe_root/events"' HUP ;;
  prior-return:INT) trap 'printf "outer-INT\n" >> "$probe_root/events"' INT ;;
  prior-return:TERM) trap 'printf "outer-TERM\n" >> "$probe_root/events"' TERM ;;
esac

if [ "$operation" = service ]; then
  render_service() {
    printf 'partial staged unit\n'
    kill -s "$signal" "$BASHPID"
    printf 'continued-service\n' >> "$probe_root/events"
    sleep 1
  }
  install_service
else
  curl() {
    local output='' previous='' arg
    for arg in "$@"; do
      if [ "$previous" = -o ]; then output=$arg; previous=''; continue; fi
      [ "$arg" != -o ] || previous=-o
    done
    if [[ "$output" == *.tar.gz ]]; then
      cp "$archive" "$output"
      kill -s "$signal" "$BASHPID"
      printf 'continued-download\n' >> "$probe_root/events"
      sleep 1
    else
      return 1
    fi
  }
  download_and_extract
fi
printf 'continued-operation\n' >> "$probe_root/events"
EOF
chmod +x "$SIGNAL_PROBE"

signal_number() {
  case "$1" in
    HUP) printf '1\n' ;;
    INT) printf '2\n' ;;
    TERM) printf '15\n' ;;
  esac
}

for operation in service download; do
  for signal in HUP INT TERM; do
    for handler_mode in prior prior-return default; do
      probe_root="$TMP_DIR/signal-$operation-$signal-$handler_mode"
      expected_status=$((128 + $(signal_number "$signal")))
      if [ "$handler_mode" = prior ]; then
        case "$signal" in HUP) expected_status=71 ;; INT) expected_status=72 ;; TERM) expected_status=73 ;; esac
      fi
      set +e
      bash "$SIGNAL_PROBE" "$operation" "$signal" "$handler_mode" \
        "$TMP_DIR/source/install-functions.sh" "$TEMPLATE" \
        "$RELEASE_DIR/sub2api_2.0.0_linux_amd64.tar.gz" "$probe_root" \
        >/dev/null 2>&1
      status=$?
      set -e
      [ "$status" -eq "$expected_status" ] ||
        fail "$operation staging $signal/$handler_mode exited $status, expected $expected_status"
      if [ "$operation" = service ]; then
        require_text "$probe_root/systemd/sub2api.service" 'old destination bytes'
        require_no_unit_temps "$probe_root/systemd/sub2api.service"
      else
        require_text "$probe_root/install/sub2api" 'old binary bytes'
        for staged in "$probe_root/install"/.sub2api.tmp.* "$probe_root/tmp"/*; do
          [ ! -e "$staged" ] && [ ! -L "$staged" ] ||
            fail "temporary download resource remains after $signal: $staged"
        done
      fi
      if [ "$handler_mode" != default ]; then
        require_text "$probe_root/events" "outer-$signal"
      fi
      if [ -e "$probe_root/events" ]; then
        reject_pattern "$probe_root/events" '^continued-' \
          "$operation continued after $signal interruption"
      fi
    done
  done
done

# Nested staging must bind every cleanup and saved trap to the frame that installed
# it. Exercise both nesting directions in subprocesses so a dynamically scoped
# inner snapshot cannot make the restored outer handler recurse into itself.
NESTED_SIGNAL_PROBE="$TMP_DIR/nested-signal-probe.sh"
cat > "$NESTED_SIGNAL_PROBE" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
direction=$1
signal=$2
handler_mode=$3
functions_file=$4
template=$5
archive=$6
probe_root=$7
mkdir -p "$probe_root/install" "$probe_root/systemd" "$probe_root/tmp"
export TMPDIR="$probe_root/tmp"
# shellcheck disable=SC1090
source "$functions_file"
INSTALL_DIR="$probe_root/install"
SYSTEMD_UNIT_PATH="$probe_root/systemd/sub2api.service"
SERVICE_USER=fixture-user
SERVER_HOST=127.0.0.1
SERVER_PORT=18080
OS=linux
ARCH=amd64
LATEST_VERSION=v2.0.0
cp "$template" "$INSTALL_DIR/sub2api.service"
printf 'old destination bytes\n' > "$SYSTEMD_UNIT_PATH"
printf 'old binary bytes\n' > "$INSTALL_DIR/sub2api"
systemctl() { :; }
id() { [ "${1:-}" != -u ] || printf '1000\n'; }
chown() { :; }
trap 'printf "%s %s %s %s %s %s\n" "${#STAGING_CONTEXT_KIND[@]}" "${#STAGING_CONTEXT_PRIMARY[@]}" "${#STAGING_CONTEXT_SECONDARY[@]}" "${#STAGING_CONTEXT_HUP[@]}" "${#STAGING_CONTEXT_INT[@]}" "${#STAGING_CONTEXT_TERM[@]}" > "$probe_root/contexts"' EXIT

case "$handler_mode:$signal" in
  prior:HUP) trap 'printf "outer-HUP\n" >> "$probe_root/events"; exit 71' HUP ;;
  prior:INT) trap 'printf "outer-INT\n" >> "$probe_root/events"; exit 72' INT ;;
  prior:TERM) trap 'printf "outer-TERM\n" >> "$probe_root/events"; exit 73' TERM ;;
  prior-return:HUP) trap 'printf "outer-HUP\n" >> "$probe_root/events"' HUP ;;
  prior-return:INT) trap 'printf "outer-INT\n" >> "$probe_root/events"' INT ;;
  prior-return:TERM) trap 'printf "outer-TERM\n" >> "$probe_root/events"' TERM ;;
esac

if [ "$direction" = service-download ]; then
  curl() {
    local output='' previous='' arg
    for arg in "$@"; do
      if [ "$previous" = -o ]; then output=$arg; previous=''; continue; fi
      [ "$arg" != -o ] || previous=-o
    done
    if [[ "$output" == *.tar.gz ]]; then
      cp "$archive" "$output"
      kill -s "$signal" "$BASHPID"
      printf 'continued-inner-download\n' >> "$probe_root/events"
    else
      return 1
    fi
  }
  render_service() {
    download_and_extract
    printf 'continued-outer-service\n' >> "$probe_root/events"
  }
  install_service
else
  render_service() {
    printf 'partial staged unit\n'
    kill -s "$signal" "$BASHPID"
    printf 'continued-inner-service\n' >> "$probe_root/events"
  }
  curl() {
    local output='' previous='' arg
    for arg in "$@"; do
      if [ "$previous" = -o ]; then output=$arg; previous=''; continue; fi
      [ "$arg" != -o ] || previous=-o
    done
    if [[ "$output" == *.tar.gz ]]; then
      install_service
      printf 'continued-outer-download\n' >> "$probe_root/events"
      cp "$archive" "$output"
    else
      return 1
    fi
  }
  download_and_extract
fi
printf 'continued-operation\n' >> "$probe_root/events"
EOF
chmod +x "$NESTED_SIGNAL_PROBE"

for direction in service-download download-service; do
  for signal in TERM HUP INT; do
    for handler_mode in prior prior-return default; do
      probe_root="$TMP_DIR/nested-signal-$direction-$signal-$handler_mode"
      expected_status=$((128 + $(signal_number "$signal")))
      if [ "$handler_mode" = prior ]; then
        case "$signal" in HUP) expected_status=71 ;; INT) expected_status=72 ;; TERM) expected_status=73 ;; esac
      fi
      set +e
      bash "$NESTED_SIGNAL_PROBE" "$direction" "$signal" "$handler_mode" \
        "$TMP_DIR/source/install-functions.sh" "$TEMPLATE" \
        "$RELEASE_DIR/sub2api_2.0.0_linux_amd64.tar.gz" "$probe_root" \
        >/dev/null 2>&1
      status=$?
      set -e
      [ "$status" -eq "$expected_status" ] ||
        fail "nested $direction staging $signal/$handler_mode exited $status, expected $expected_status"
      require_text "$probe_root/systemd/sub2api.service" 'old destination bytes'
      require_text "$probe_root/install/sub2api" 'old binary bytes'
      require_text "$probe_root/contexts" '0 0 0 0 0 0'
      require_no_unit_temps "$probe_root/systemd/sub2api.service"
      for staged in "$probe_root/install"/.sub2api.tmp.* "$probe_root/tmp"/*; do
        [ ! -e "$staged" ] && [ ! -L "$staged" ] ||
          fail "nested temporary staging resource remains after $direction $signal: $staged"
      done
      if [ "$handler_mode" != default ]; then
        marker_count=$(grep -Fxc -- "outer-$signal" "$probe_root/events" 2>/dev/null || true)
        [ "$marker_count" -eq 1 ] ||
          fail "nested $direction $signal/$handler_mode invoked prior handler $marker_count times"
      fi
      if [ -e "$probe_root/events" ]; then
        reject_pattern "$probe_root/events" '^continued-' \
          "nested $direction continued after $signal interruption"
      fi
    done
  done
done

# The intentionally conservative IPv6 grammar is pure-hex only; document and pin
# rejection of IPv4-embedded forms instead of silently implying broader support.
require_text "$ROOT/deploy/README.md" 'unbracketed pure-hex IPv6 literals'
for host in '::ffff:192.0.2.128' '::192.0.2.1' '2001:db8::192.0.2.1'; do
  if validate_server_host "$host"; then
    fail "IPv4-embedded IPv6 unexpectedly accepted: $host"
  fi
done

EVENT_LOG="$TMP_DIR/lifecycle-events.log"
FAIL_ARCHIVE=''
LATEST_LOOKUP_MODE=success
SERVICE_ACTIVE=false
curl() {
  local output=''
  local url=''
  local previous=''
  local arg
  for arg in "$@"; do
    if [ "$previous" = '-o' ]; then
      output=$arg
      previous=''
      continue
    fi
    case "$arg" in
      -o) previous='-o' ;;
      http*) url=$arg ;;
    esac
  done
  case "$url" in
    */releases/latest)
      case "$LATEST_LOOKUP_MODE" in
        success) printf '{"tag_name":"v2.0.0"}\n' ;;
        request-failure) return 1 ;;
        parse-failure) printf '{"tag_name":\n' ;;
        missing-tag) printf '{"name":"fixture release"}\n' ;;
        *) fail "unknown latest lookup fixture mode: $LATEST_LOOKUP_MODE" ;;
      esac
      ;;
    */releases/tags/*)
      printf '200'
      ;;
    */checksums.txt)
      return 1
      ;;
    *.tar.gz)
      local archive=${url##*/}
      printf 'download:%s\n' "$archive" >> "$EVENT_LOG"
      [ "$archive" != "$FAIL_ARCHIVE" ] || return 1
      cp "$RELEASE_DIR/$archive" "$output"
      ;;
    *)
      fail "fixture curl received unexpected URL: $url"
      ;;
  esac
}
tar() {
  case "${1:-}" in
    -xzf) printf 'extract:%s\n' "${2:-}" >> "$EVENT_LOG" ;;
  esac
  command tar "$@"
}
systemctl() {
  case "${1:-}" in
    is-active)
      [ "$SERVICE_ACTIVE" = true ]
      ;;
    stop)
      printf 'stop:%s\n' "$("$INSTALL_DIR/sub2api" --version)" >> "$EVENT_LOG"
      SERVICE_ACTIVE=false
      ;;
    daemon-reload)
      printf 'daemon-reload\n' >> "$EVENT_LOG"
      ;;
    start)
      printf 'start:%s\n' "$("$INSTALL_DIR/sub2api" --version)" >> "$EVENT_LOG"
      SERVICE_ACTIVE=true
      ;;
    enable)
      printf 'enable:%s\n' "${2:-}" >> "$EVENT_LOG"
      ;;
    *)
      fail "fixture systemctl received unexpected command: $*"
      ;;
  esac
}
get_public_ip() {
  PUBLIC_IP='127.0.0.1'
}
create_user() {
  printf 'create-user:%s\n' "$SERVICE_USER" >> "$EVENT_LOG"
}
setup_directories() {
  mkdir -p "$INSTALL_DIR/data" "$CONFIG_DIR"
  printf 'setup-directories\n' >> "$EVENT_LOG"
}
is_interactive() {
  return 1
}

setup_lifecycle_fixture() {
  local name=$1
  TEST_ROOT="$TMP_DIR/lifecycle-$name"
  INSTALL_DIR="$TEST_ROOT/opt/sub2api"
  CONFIG_DIR="$TEST_ROOT/etc/sub2api"
  SYSTEMD_UNIT_PATH="$TEST_ROOT/systemd/sub2api.service"
  SERVICE_USER='fixture-user'
  SERVER_HOST='127.0.0.7'
  SERVER_PORT='18080'
  OS=linux
  ARCH=amd64
  LATEST_VERSION=''
  SYSTEMCTL_LOG="$TEST_ROOT/systemctl.log"
  OWNERSHIP_LOG="$TEST_ROOT/ownership.log"
  mkdir -p "$INSTALL_DIR" "$CONFIG_DIR" "$(dirname -- "$SYSTEMD_UNIT_PATH")" "$TEST_ROOT/tmp"
  : > "$EVENT_LOG"
  : > "$SYSTEMCTL_LOG"
  : > "$OWNERSHIP_LOG"
  FAIL_ARCHIVE=''
  LATEST_LOOKUP_MODE=success
  FAIL_CHOWN=false
  TEST_UID=1000
  SERVICE_ACTIVE=false
}

setup_lifecycle_fixture invalid-environment
INSTALL_DIR="$TEST_ROOT/opt/sub2api%N"
if fresh_install '' >/dev/null 2>&1; then
  fail 'fresh install accepted an unsafe environment-supplied INSTALL_DIR'
fi
[ ! -s "$EVENT_LOG" ] || fail 'invalid environment reached lifecycle commands before rejection'

setup_lifecycle_fixture fresh
fresh_install ''
require_text "$EVENT_LOG" 'download:sub2api_2.0.0_linux_amd64.tar.gz'
require_text "$EVENT_LOG" 'start:sub2api v2.0.0'
require_text "$EVENT_LOG" 'enable:sub2api'
require_order "$EVENT_LOG" 'download:sub2api_2.0.0_linux_amd64.tar.gz' 'start:sub2api v2.0.0'
require_order "$EVENT_LOG" 'start:sub2api v2.0.0' 'enable:sub2api'
require_text "$INSTALL_DIR/sub2api.service" '# fixture-template-v2.0.0'
require_text "$SYSTEMD_UNIT_PATH" 'Environment="SERVER_HOST=127.0.0.7"'

setup_lifecycle_fixture fresh-version
fresh_install 'v1.5.0'
require_text "$EVENT_LOG" 'download:sub2api_1.5.0_linux_amd64.tar.gz'
require_text "$EVENT_LOG" 'start:sub2api v1.5.0'
require_text "$EVENT_LOG" 'enable:sub2api'

setup_lifecycle_fixture upgrade-active
cp "$TMP_DIR/build-1.0.0/sub2api" "$INSTALL_DIR/sub2api"
SERVICE_ACTIVE=true
upgrade
cmp "$INSTALL_DIR/sub2api.backup" "$TMP_DIR/build-1.0.0/sub2api" >/dev/null || fail 'upgrade backup bytes differ from previous binary'
require_order "$EVENT_LOG" 'stop:sub2api v1.0.0' 'download:sub2api_2.0.0_linux_amd64.tar.gz'
require_order "$EVENT_LOG" 'download:sub2api_2.0.0_linux_amd64.tar.gz' 'start:sub2api v2.0.0'
reject_pattern "$EVENT_LOG" '^enable:' 'upgrade must not alter enablement'

setup_lifecycle_fixture upgrade-inactive
cp "$TMP_DIR/build-1.0.0/sub2api" "$INSTALL_DIR/sub2api"
SERVICE_ACTIVE=false
upgrade
reject_pattern "$EVENT_LOG" '^(stop|start|enable):' 'upgrade must preserve an inactive service state'
require_text "$EVENT_LOG" 'download:sub2api_2.0.0_linux_amd64.tar.gz'

setup_lifecycle_fixture rollback
cp "$TMP_DIR/build-2.0.0/sub2api" "$INSTALL_DIR/sub2api"
SERVICE_ACTIVE=true
install_version 'v1.5.0'
cmp "$INSTALL_DIR/sub2api.backup.v2.0.0" "$TMP_DIR/build-2.0.0/sub2api" >/dev/null || fail 'rollback backup bytes or versioned name are wrong'
require_order "$EVENT_LOG" 'stop:sub2api v2.0.0' 'download:sub2api_1.5.0_linux_amd64.tar.gz'
require_order "$EVENT_LOG" 'download:sub2api_1.5.0_linux_amd64.tar.gz' 'start:sub2api v1.5.0'
reject_pattern "$EVENT_LOG" '^enable:' 'rollback must not alter enablement'

setup_lifecycle_fixture rollback-inactive
cp "$TMP_DIR/build-2.0.0/sub2api" "$INSTALL_DIR/sub2api"
SERVICE_ACTIVE=false
install_version 'v1.5.0'
reject_pattern "$EVENT_LOG" '^(stop|start|enable):' 'rollback must preserve an inactive service state'
require_text "$EVENT_LOG" 'download:sub2api_1.5.0_linux_amd64.tar.gz'

# Latest-version discovery is fallible. An active upgrade must restart the old
# service after its stop/backup phase, while an inactive upgrade must stay inactive.
setup_lifecycle_fixture upgrade-latest-failure-active
cp "$TMP_DIR/build-1.0.0/sub2api" "$INSTALL_DIR/sub2api"
SERVICE_ACTIVE=true
LATEST_LOOKUP_MODE=request-failure
if upgrade >/dev/null 2>&1; then
  fail 'active upgrade with failed latest lookup unexpectedly succeeded'
fi
cmp "$INSTALL_DIR/sub2api" "$TMP_DIR/build-1.0.0/sub2api" >/dev/null || fail 'latest lookup failure changed the active upgrade binary'
cmp "$INSTALL_DIR/sub2api.backup" "$TMP_DIR/build-1.0.0/sub2api" >/dev/null || fail 'latest lookup failure changed active upgrade backup semantics'
require_text "$EVENT_LOG" 'stop:sub2api v1.0.0'
require_text "$EVENT_LOG" 'start:sub2api v1.0.0'
require_order "$EVENT_LOG" 'stop:sub2api v1.0.0' 'start:sub2api v1.0.0'
stop_count=$(grep -Fxc -- 'stop:sub2api v1.0.0' "$EVENT_LOG" || true)
start_count=$(grep -Fxc -- 'start:sub2api v1.0.0' "$EVENT_LOG" || true)
[ "$stop_count" -eq 1 ] || fail "active latest lookup failure stopped the service $stop_count times"
[ "$start_count" -eq 1 ] || fail "active latest lookup failure restarted the service $start_count times"
reject_pattern "$EVENT_LOG" '^(download|extract|enable):' 'active latest lookup failure reached archive download, extraction, or enablement'
[ "$SERVICE_ACTIVE" = true ] || fail 'latest lookup failure did not recover prior active state'
for staged in "$INSTALL_DIR"/.sub2api.tmp.* "$TEST_ROOT"/tmp/*; do
  [ ! -e "$staged" ] && [ ! -L "$staged" ] || fail "stage residue remains after active latest lookup failure: $staged"
done

setup_lifecycle_fixture upgrade-latest-failure-inactive
cp "$TMP_DIR/build-1.0.0/sub2api" "$INSTALL_DIR/sub2api"
SERVICE_ACTIVE=false
LATEST_LOOKUP_MODE=request-failure
if upgrade >/dev/null 2>&1; then
  fail 'inactive upgrade with failed latest lookup unexpectedly succeeded'
fi
cmp "$INSTALL_DIR/sub2api" "$TMP_DIR/build-1.0.0/sub2api" >/dev/null || fail 'latest lookup failure changed the inactive upgrade binary'
cmp "$INSTALL_DIR/sub2api.backup" "$TMP_DIR/build-1.0.0/sub2api" >/dev/null || fail 'latest lookup failure changed inactive upgrade backup semantics'
reject_pattern "$EVENT_LOG" '^(stop|start|download|extract|enable):' 'inactive latest lookup failure changed service state or reached archive work'
[ "$SERVICE_ACTIVE" = false ] || fail 'latest lookup failure activated an inactive service'
for staged in "$INSTALL_DIR"/.sub2api.tmp.* "$TEST_ROOT"/tmp/*; do
  [ ! -e "$staged" ] && [ ! -L "$staged" ] || fail "stage residue remains after inactive latest lookup failure: $staged"
done

# A fresh latest install must stop at discovery, before archive/account/directory,
# unit, start, or enable mutations. Specific-version fresh install is covered above.
setup_lifecycle_fixture fresh-latest-failure
LATEST_LOOKUP_MODE=request-failure
if fresh_install '' >/dev/null 2>&1; then
  fail 'fresh install with failed latest lookup unexpectedly succeeded'
fi
[ ! -e "$INSTALL_DIR/sub2api" ] || fail 'fresh latest lookup failure installed a binary'
[ ! -e "$SYSTEMD_UNIT_PATH" ] || fail 'fresh latest lookup failure installed a systemd unit'
[ ! -s "$EVENT_LOG" ] || fail 'fresh latest lookup failure reached a later lifecycle mutation'
for staged in "$INSTALL_DIR"/.sub2api.tmp.* "$TEST_ROOT"/tmp/*; do
  [ ! -e "$staged" ] && [ ! -L "$staged" ] || fail "stage residue remains after fresh latest lookup failure: $staged"
done

# Malformed and valid-but-tagless responses are equivalent discovery failures and
# must also stop fresh installation before any later lifecycle boundary.
for lookup_mode in parse-failure missing-tag; do
  setup_lifecycle_fixture "fresh-latest-$lookup_mode"
  LATEST_LOOKUP_MODE=$lookup_mode
  if fresh_install '' >/dev/null 2>&1; then
    fail "fresh install with $lookup_mode latest response unexpectedly succeeded"
  fi
  [ ! -e "$INSTALL_DIR/sub2api" ] || fail "$lookup_mode latest response installed a binary"
  [ ! -e "$SYSTEMD_UNIT_PATH" ] || fail "$lookup_mode latest response installed a systemd unit"
  [ ! -s "$EVENT_LOG" ] || fail "$lookup_mode latest response reached a later lifecycle mutation"
done

# Failed active upgrades retain old bytes, leave no staged binary, and recover activity.
setup_lifecycle_fixture upgrade-failure
cp "$TMP_DIR/build-1.0.0/sub2api" "$INSTALL_DIR/sub2api"
SERVICE_ACTIVE=true
FAIL_ARCHIVE='sub2api_2.0.0_linux_amd64.tar.gz'
if upgrade >/dev/null 2>&1; then
  fail 'failed upgrade unexpectedly succeeded'
fi
cmp "$INSTALL_DIR/sub2api" "$TMP_DIR/build-1.0.0/sub2api" >/dev/null || fail 'failed upgrade did not preserve previous binary'
require_order "$EVENT_LOG" 'stop:sub2api v1.0.0' 'download:sub2api_2.0.0_linux_amd64.tar.gz'
require_text "$EVENT_LOG" 'start:sub2api v1.0.0'
[ "$SERVICE_ACTIVE" = true ] || fail 'failed upgrade did not recover prior active state'
for staged in "$INSTALL_DIR"/.sub2api.tmp.*; do
  [ ! -e "$staged" ] || fail "staged binary remains after failed upgrade: $staged"
done

setup_lifecycle_fixture rollback-failure
cp "$TMP_DIR/build-2.0.0/sub2api" "$INSTALL_DIR/sub2api"
SERVICE_ACTIVE=true
FAIL_ARCHIVE='sub2api_1.5.0_linux_amd64.tar.gz'
if install_version 'v1.5.0' >/dev/null 2>&1; then
  fail 'failed rollback unexpectedly succeeded'
fi
cmp "$INSTALL_DIR/sub2api" "$TMP_DIR/build-2.0.0/sub2api" >/dev/null || fail 'failed rollback did not preserve previous binary'
cmp "$INSTALL_DIR/sub2api.backup.v2.0.0" "$TMP_DIR/build-2.0.0/sub2api" >/dev/null || fail 'failed rollback backup is incorrect'
require_text "$EVENT_LOG" 'start:sub2api v2.0.0'
[ "$SERVICE_ACTIVE" = true ] || fail 'failed rollback did not recover prior active state'

printf 'PASS: installer lifecycle, release coupling, validation, and atomic systemd contract\n'
