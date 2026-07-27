#!/bin/sh
set -eu

DEPLOY_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
SCRIPT="$DEPLOY_DIR/docker-deploy.sh"
TMP_DIR=$(mktemp -d)
FIXTURE_ROOT="$TMP_DIR/fixture-repository"
BASE_BIN="$TMP_DIR/base-bin"
CURL_BIN="$TMP_DIR/curl-bin"
WGET_BIN="$TMP_DIR/wget-bin"
DOWNLOAD_LOG="$TMP_DIR/download.log"
DOCKER_LOG="$TMP_DIR/docker.log"
OPENSSL_COUNT="$TMP_DIR/openssl.count"
trap 'rm -rf "$TMP_DIR"' EXIT HUP INT TERM

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

assert_file() {
  [ -f "$1" ] || fail "expected file $1"
}

assert_absent() {
  [ ! -e "$1" ] && [ ! -L "$1" ] || fail "expected $1 to remain absent"
}

assert_contains() {
  grep -F -- "$2" "$1" >/dev/null || fail "$1 does not contain: $2"
}

assert_not_contains() {
  if grep -F -- "$2" "$1" >/dev/null; then
    fail "$1 unexpectedly contains: $2"
  fi
}

assert_secret_values_absent() {
  output=$1
  for value in \
    0000000000000000000000000000000000000000000000000000000000000001 \
    0000000000000000000000000000000000000000000000000000000000000002 \
    0000000000000000000000000000000000000000000000000000000000000003
  do
    assert_not_contains "$output" "$value"
  done
}

assert_only_canonical_files() {
  target=$1
  [ "$(ls -A "$target" | LC_ALL=C sort | tr '\n' ' ')" = ".env .env.example compose.yaml " ] || \
    fail "$target must contain exactly the three canonical files"
}

mkdir -p "$FIXTURE_ROOT/release-test/deploy" "$BASE_BIN" "$CURL_BIN" "$WGET_BIN"
cp "$DEPLOY_DIR/compose.yaml" "$FIXTURE_ROOT/release-test/deploy/compose.yaml"
cp "$DEPLOY_DIR/.env.example" "$FIXTURE_ROOT/release-test/deploy/.env.example"

# Keep the test PATH isolated so curl can truly be absent in wget cases.
for command_path in \
  /bin/awk /bin/cat /bin/chmod /bin/cp /bin/diff /bin/dirname /bin/env /bin/grep \
  /bin/ln /bin/ls /bin/mkdir /bin/mktemp /bin/mv /bin/pwd /bin/rm /bin/rmdir \
  /bin/sleep /bin/sort /bin/stat /bin/tr /bin/uname /bin/wc /usr/bin/awk /usr/bin/cat /usr/bin/chmod \
  /usr/bin/cp /usr/bin/diff /usr/bin/dirname /usr/bin/env /usr/bin/grep /usr/bin/ln \
  /usr/bin/ls /usr/bin/mkdir /usr/bin/mktemp /usr/bin/mv /usr/bin/pwd /usr/bin/rm /usr/bin/rmdir \
  /usr/bin/sleep /usr/bin/sort /usr/bin/stat /usr/bin/tr /usr/bin/uname /usr/bin/wc
do
  if [ -x "$command_path" ]; then
    command_name=${command_path##*/}
    if [ ! -e "$BASE_BIN/$command_name" ]; then
      cat >"$BASE_BIN/$command_name" <<EOF
#!/bin/sh
environment=\$(export -p)
case "\$environment" in
  *000000000000000000000000000000000000000000000000000000000000000[123]*)
    unset environment
    printf 'secret-bearing $command_name environment\n' >&2
    exit 97
    ;;
esac
unset environment
exec $command_path "\$@"
EOF
      chmod +x "$BASE_BIN/$command_name"
    fi
  fi
done

# Publication faults are injected by the fixture's ln implementation, not by a
# production-script hook. This exercises the real filesystem/tool boundary.
cat >"$BASE_BIN/ln" <<'EOF'
#!/bin/sh
set -eu
environment=$(export -p)
case "$environment" in
  *000000000000000000000000000000000000000000000000000000000000000[123]*)
    unset environment
    printf 'secret-bearing ln environment\n' >&2
    exit 97
    ;;
esac
unset environment
/bin/ln "$@"
source_path=$1
destination_path=$2
case "${BOOTSTRAP_LN_ACTION:-}" in
  collision)
    case "$source_path" in
      */compose.yaml) printf 'foreign-collision-content\n' >"${destination_path%/*}/.env.example" ;;
    esac
    ;;
  signal-after-first-publish)
    case "$source_path" in
      */compose.yaml)
        kill -TERM "$PPID"
        ;;
    esac
    ;;
esac
EOF
chmod +x "$BASE_BIN/ln"

cat >"$CURL_BIN/curl" <<'EOF'
#!/bin/sh
set -eu
environment=$(export -p)
case "$environment" in
  *000000000000000000000000000000000000000000000000000000000000000[123]*)
    unset environment
    printf 'secret-bearing curl environment\n' >&2
    exit 97
    ;;
esac
unset environment
output=
url=
for arg in "$@"; do
  case "$arg" in
    *000000000000000000000000000000000000000000000000000000000000000[123]*)
      printf 'secret-bearing curl argv\n' >&2
      exit 97
      ;;
  esac
done
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o|--output)
      [ "$#" -ge 2 ] || exit 2
      output=$2
      shift 2
      ;;
    --output=*) output=${1#*=}; shift ;;
    -*) shift ;;
    *) url=$1; shift ;;
  esac
done
[ -n "$output" ] && [ -n "$url" ] || exit 2
printf 'curl %s %s\n' "$url" "$output" >>"$BOOTSTRAP_DOWNLOAD_LOG"
relative=${url#"$BOOTSTRAP_RAW_BASE_URL"/}
[ "$relative" != "$url" ] || exit 22
source_file="$BOOTSTRAP_FIXTURE_ROOT/$relative"
[ -f "$source_file" ] || exit 22
cp "$source_file" "$output"
EOF

cat >"$WGET_BIN/wget" <<'EOF'
#!/bin/sh
set -eu
environment=$(export -p)
case "$environment" in
  *000000000000000000000000000000000000000000000000000000000000000[123]*)
    unset environment
    printf 'secret-bearing wget environment\n' >&2
    exit 97
    ;;
esac
unset environment
output=
url=
for arg in "$@"; do
  case "$arg" in
    *000000000000000000000000000000000000000000000000000000000000000[123]*)
      printf 'secret-bearing wget argv\n' >&2
      exit 97
      ;;
  esac
done
while [ "$#" -gt 0 ]; do
  case "$1" in
    --output-document=*) output=${1#*=}; shift ;;
    --output-document)
      [ "$#" -ge 2 ] || exit 2
      output=$2
      shift 2
      ;;
    --quiet) shift ;;
    -*) exit 2 ;;
    *) [ -z "$url" ] || exit 2; url=$1; shift ;;
  esac
done
[ -n "$output" ] && [ -n "$url" ] || exit 2
printf 'wget %s %s\n' "$url" "$output" >>"$BOOTSTRAP_DOWNLOAD_LOG"
relative=${url#"$BOOTSTRAP_RAW_BASE_URL"/}
[ "$relative" != "$url" ] || exit 8
source_file="$BOOTSTRAP_FIXTURE_ROOT/$relative"
[ -f "$source_file" ] || exit 8
cp "$source_file" "$output"
EOF

cat >"$BASE_BIN/openssl" <<'EOF'
#!/bin/sh
set -eu
[ "${1-}" = rand ] && [ "${2-}" = -hex ] && [ "${3-}" = 32 ] || exit 2
if [ -n "${BOOTSTRAP_RETARGET_COUNT:-}" ] && [ -n "${BOOTSTRAP_PARENT_LINK:-}" ] && \
   [ -n "${BOOTSTRAP_RETARGET_PARENT:-}" ]; then
  retarget_count=0
  [ ! -f "$BOOTSTRAP_RETARGET_COUNT" ] || retarget_count=$(awk 'NR == 1 { print; exit }' "$BOOTSTRAP_RETARGET_COUNT")
  retarget_count=$((retarget_count + 1))
  printf '%s\n' "$retarget_count" >"$BOOTSTRAP_RETARGET_COUNT"
  if [ "$retarget_count" = 3 ]; then
    rm -f -- "$BOOTSTRAP_PARENT_LINK"
    ln -s "$BOOTSTRAP_RETARGET_PARENT" "$BOOTSTRAP_PARENT_LINK"
  fi
fi
environment=$(export -p)
case "$environment" in
  *000000000000000000000000000000000000000000000000000000000000000[123]*)
    unset environment
    printf 'secret-bearing openssl environment\n' >&2
    exit 97
    ;;
esac
unset environment
count=0
[ ! -f "$BOOTSTRAP_OPENSSL_COUNT" ] || count=$(awk 'NR == 1 { print; exit }' "$BOOTSTRAP_OPENSSL_COUNT")
count=$((count + 1))
printf '%s\n' "$count" >"$BOOTSTRAP_OPENSSL_COUNT"
printf '%064x\n' "$count"
EOF

cat >"$BASE_BIN/docker" <<'EOF'
#!/bin/sh
set -eu
environment=$(export -p)
case "$environment" in
  *000000000000000000000000000000000000000000000000000000000000000[123]*)
    unset environment
    printf 'secret-bearing docker environment\n' >&2
    exit 97
    ;;
esac
unset environment
printf '%s\n' "$*" >>"$BOOTSTRAP_DOCKER_LOG"
[ "${BOOTSTRAP_DOCKER_V2:-yes}" = yes ] || exit 1
[ "${1-}" = compose ] && [ "${2-}" = version ] || exit 3
printf 'Docker Compose version v2.24.7\n'
EOF
chmod +x "$CURL_BIN/curl" "$WGET_BIN/wget" "$BASE_BIN/openssl" "$BASE_BIN/docker"

run_bootstrap_with_path() {
  tool_path=$1
  destination=$2
  output=$3
  trace=$4
  shift 4
  env \
    PATH="$tool_path:$BASE_BIN" \
    SYSTEMROOT="${SYSTEMROOT-}" \
    WINDIR="${WINDIR-}" \
    TMP="${TMP-}" \
    TEMP="${TEMP-}" \
    BOOTSTRAP_FIXTURE_ROOT="$FIXTURE_ROOT" \
    BOOTSTRAP_RAW_BASE_URL="https://fixture.invalid/sub2api" \
    BOOTSTRAP_DOWNLOAD_LOG="$DOWNLOAD_LOG" \
    BOOTSTRAP_DOCKER_LOG="$DOCKER_LOG" \
    BOOTSTRAP_OPENSSL_COUNT="$OPENSSL_COUNT" \
    BOOTSTRAP_PARENT_LINK="${BOOTSTRAP_PARENT_LINK-}" \
    BOOTSTRAP_RETARGET_PARENT="${BOOTSTRAP_RETARGET_PARENT-}" \
    BOOTSTRAP_RETARGET_COUNT="${BOOTSTRAP_RETARGET_COUNT-}" \
    SUB2API_RAW_BASE_URL="https://fixture.invalid/sub2api" \
    /bin/sh $trace "$SCRIPT" --destination "$destination" --ref release-test "$@" >"$output" 2>&1
}

run_bootstrap() {
  destination=$1
  output=$2
  shift 2
  run_bootstrap_with_path "$CURL_BIN" "$destination" "$output" "" "$@"
}

run_hostile_export_bootstrap() (
  destination=$1
  output=$2
  trace=$3
  rm -f "$OPENSSL_COUNT"
  value=hostile-caller-value
  key=hostile-caller-key
  generated_secret=hostile-caller-generated-secret
  secret=hostile-caller-secret
  secrets=hostile-caller-secrets
  export value key generated_secret secret secrets
  run_bootstrap_with_path "$CURL_BIN" "$destination" "$output" "$trace"
)

verify_template_preservation() {
  example=$1
  generated=$2
  awk '
    BEGIN {
      expected["DATABASE_PASSWORD"] = 1
      expected["JWT_SECRET"] = 1
      expected["TOTP_ENCRYPTION_KEY"] = 1
    }
    NR == FNR {
      source[FNR] = $0
      source_count = FNR
      next
    }
    {
      if (FNR > source_count) exit 10
      original = source[FNR]
      generated = $0
      changed = 0
      for (key in expected) {
        prefix = key "="
        if (index(original, prefix) == 1) {
          source_key_count[key]++
          if (index(generated, prefix) != 1 || generated == original) exit 11
          generated_key_count[key]++
          changed = 1
          break
        }
        if (index(generated, prefix) == 1) exit 12
      }
      if (!changed && generated != original) exit 13
    }
    END {
      if (FNR != source_count) exit 14
      for (key in expected) {
        if (source_key_count[key] != 1 || generated_key_count[key] != 1) exit 15
      }
    }
  ' "$example" "$generated" || fail ".env must differ from .env.example only at exactly three active secret values"
}

# First install runs with caller-enabled xtrace. Deterministic generated values
# must be absent from combined stdout/stderr and from all fake-tool argv/env.
TARGET="$TMP_DIR/first-install"
OUTPUT="$TMP_DIR/first-install.out"
run_bootstrap_with_path "$CURL_BIN" "$TARGET" "$OUTPUT" -x || { grep -v '000000000000000000000000000000000000000000000000000000000000000[123]' "$OUTPUT" >&2 || true; fail "xtrace first install failed"; }
assert_only_canonical_files "$TARGET"
cmp -s "$DEPLOY_DIR/compose.yaml" "$TARGET/compose.yaml" || fail "downloaded compose.yaml differs from fixture"
cmp -s "$DEPLOY_DIR/.env.example" "$TARGET/.env.example" || fail "downloaded .env.example differs from fixture"
verify_template_preservation "$TARGET/.env.example" "$TARGET/.env"
assert_absent "$TARGET/docker-compose.yml"
assert_absent "$TARGET/docker-compose.local.yml"
assert_absent "$TARGET/data"
assert_absent "$TARGET/postgres_data"
assert_absent "$TARGET/redis_data"
assert_contains "$DOWNLOAD_LOG" "curl https://fixture.invalid/sub2api/release-test/deploy/compose.yaml"
assert_contains "$DOWNLOAD_LOG" "curl https://fixture.invalid/sub2api/release-test/deploy/.env.example"
[ "$(wc -l <"$DOWNLOAD_LOG" | tr -d ' ')" = 2 ] || fail "bootstrap must download exactly two canonical files"
assert_contains "$OUTPUT" "docker compose up -d"
assert_not_contains "$OUTPUT" "docker-compose"
assert_not_contains "$OUTPUT" "docker compose config"
assert_secret_values_absent "$OUTPUT"
[ "$(wc -l <"$DOCKER_LOG" | tr -d ' ')" = 1 ] || fail "bootstrap must only probe Docker Compose once"
assert_contains "$DOCKER_LOG" "compose version"
assert_not_contains "$DOCKER_LOG" " up "

# Hostile caller exports of every potentially secret-bearing internal name must
# not cause deterministic generated bytes to enter any child environment.
for trace_name in normal xtrace; do
  HOSTILE_TARGET="$TMP_DIR/hostile-export-$trace_name"
  HOSTILE_OUTPUT="$TMP_DIR/hostile-export-$trace_name.out"
  trace=
  [ "$trace_name" = normal ] || trace=-x
  run_hostile_export_bootstrap "$HOSTILE_TARGET" "$HOSTILE_OUTPUT" "$trace" || \
    fail "hostile exported-variable $trace_name install failed"
  assert_only_canonical_files "$HOSTILE_TARGET"
  assert_secret_values_absent "$HOSTILE_OUTPUT"
done

# Ambient caller exports must not activate any post-generation production hook.
# A hostile legacy hook would receive the staging path and print every secret.
cat >"$BASE_BIN/hostile-publish-hook" <<'EOF'
#!/bin/sh
set -eu
printf 'legacy publish hook was called\n' >>"$BOOTSTRAP_HOSTILE_HOOK_LOG"
awk '/^(DATABASE_PASSWORD|JWT_SECRET|TOTP_ENCRYPTION_KEY)=/' "$2/.env"
EOF
chmod +x "$BASE_BIN/hostile-publish-hook"
AMBIENT_HOOK_TARGET="$TMP_DIR/ambient-publish-hook"
AMBIENT_HOOK_OUTPUT="$TMP_DIR/ambient-publish-hook.out"
AMBIENT_HOOK_LOG="$TMP_DIR/ambient-publish-hook.log"
if ! SUB2API_BOOTSTRAP_TEST_PUBLISH_HOOK="$BASE_BIN/hostile-publish-hook" \
  BOOTSTRAP_HOSTILE_HOOK_LOG="$AMBIENT_HOOK_LOG" \
  run_bootstrap "$AMBIENT_HOOK_TARGET" "$AMBIENT_HOOK_OUTPUT"; then
  fail "ambient publish-hook export must not affect bootstrap"
fi
assert_only_canonical_files "$AMBIENT_HOOK_TARGET"
assert_absent "$AMBIENT_HOOK_LOG"
assert_secret_values_absent "$AMBIENT_HOOK_OUTPUT"

# A valid-shape but constant generator must be rejected before publication.
CONSTANT_SECRET=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
cat >"$BASE_BIN/openssl-constant" <<EOF
#!/bin/sh
set -eu
[ "\${1-}" = rand ] && [ "\${2-}" = -hex ] && [ "\${3-}" = 32 ] || exit 2
printf '%s\\n' '$CONSTANT_SECRET'
EOF
chmod +x "$BASE_BIN/openssl-constant"
CONSTANT_BIN="$TMP_DIR/constant-bin"
mkdir "$CONSTANT_BIN"
cp "$BASE_BIN/openssl-constant" "$CONSTANT_BIN/openssl"
CONSTANT_TARGET="$TMP_DIR/constant-generator"
CONSTANT_OUTPUT="$TMP_DIR/constant-generator.out"
if run_bootstrap_with_path "$CURL_BIN:$CONSTANT_BIN" "$CONSTANT_TARGET" "$CONSTANT_OUTPUT" ""; then
  fail "constant valid-shape generator must be rejected"
fi
assert_absent "$CONSTANT_TARGET"
assert_not_contains "$CONSTANT_OUTPUT" "$CONSTANT_SECRET"
[ -z "$(ls -A "$TMP_DIR" | grep '^\.constant-generator\.bootstrap\.' || true)" ] || \
  fail "constant-generator failure left private staging data"

secrets=
for key in DATABASE_PASSWORD JWT_SECRET TOTP_ENCRYPTION_KEY; do
  value=$(awk -F= -v key="$key" '$1 == key { print substr($0, index($0, "=") + 1); found = 1 } END { if (!found) exit 1 }' "$TARGET/.env") || fail "$key is missing"
  [ "${#value}" = 64 ] || fail "$key must contain a 32-byte hexadecimal secret"
  case "$value" in *[!0-9a-f]*) fail "$key must be hexadecimal" ;; esac
  assert_not_contains "$OUTPUT" "$value"
  case " $secrets " in *" $value "*) fail "generated secrets must be distinct" ;; esac
  secrets="$secrets $value"
done

# Check 0600 only when this filesystem reports POSIX permission changes.
PERMISSION_PROBE="$TMP_DIR/permission-probe"
: >"$PERMISSION_PROBE"
chmod 600 "$PERMISSION_PROBE" 2>/dev/null || true
probe_mode=
env_mode=
if probe_mode=$(stat -c '%a' "$PERMISSION_PROBE" 2>/dev/null); then
  env_mode=$(stat -c '%a' "$TARGET/.env" 2>/dev/null || true)
elif probe_mode=$(stat -f '%Lp' "$PERMISSION_PROBE" 2>/dev/null); then
  env_mode=$(stat -f '%Lp' "$TARGET/.env" 2>/dev/null || true)
fi
if [ "$probe_mode" = 600 ]; then
  [ "$env_mode" = 600 ] || fail ".env mode is $env_mode, expected 600"
fi

# The default convention is a dedicated sub2api-deploy directory.
DEFAULT_PARENT="$TMP_DIR/default-parent"
mkdir "$DEFAULT_PARENT"
DEFAULT_OUTPUT="$TMP_DIR/default.out"
(
  cd "$DEFAULT_PARENT"
  env \
    PATH="$CURL_BIN:$BASE_BIN" \
    SYSTEMROOT="${SYSTEMROOT-}" \
    WINDIR="${WINDIR-}" \
    TMP="${TMP-}" \
    TEMP="${TEMP-}" \
    BOOTSTRAP_FIXTURE_ROOT="$FIXTURE_ROOT" \
    BOOTSTRAP_RAW_BASE_URL="https://fixture.invalid/sub2api" \
    BOOTSTRAP_DOWNLOAD_LOG="$DOWNLOAD_LOG" \
    BOOTSTRAP_DOCKER_LOG="$DOCKER_LOG" \
    BOOTSTRAP_OPENSSL_COUNT="$OPENSSL_COUNT" \
    SUB2API_RAW_BASE_URL="https://fixture.invalid/sub2api" \
    SUB2API_REF=release-test \
    /bin/sh "$SCRIPT" >"$DEFAULT_OUTPUT" 2>&1
) || fail "default destination install failed"
assert_only_canonical_files "$DEFAULT_PARENT/sub2api-deploy"

# Every pre-existing destination is rejected. The atomic publication contract
# requires absence, including for an empty directory.
EMPTY_TARGET="$TMP_DIR/empty-directory"
mkdir "$EMPTY_TARGET"
EMPTY_OUTPUT="$TMP_DIR/empty-directory.out"
if run_bootstrap "$EMPTY_TARGET" "$EMPTY_OUTPUT"; then fail "empty destination must be rejected"; fi
[ -d "$EMPTY_TARGET" ] && [ -z "$(ls -A "$EMPTY_TARGET")" ] || fail "empty destination was altered"
assert_contains "$EMPTY_OUTPUT" "destination already exists"

# Existing canonical entries and runtime data are rejected and preserved
# byte-for-byte; none can be replaced by publication.
for entry_name in .env .env.example compose.yaml; do
  existing="$TMP_DIR/existing-${entry_name#.}"
  mkdir "$existing"
  printf 'foreign-%s\n' "$entry_name" >"$existing/$entry_name"
  printf 'foreign\n' >"$existing/operator-file"
  before="$TMP_DIR/existing-${entry_name#.}.before"
  cp -R "$existing" "$before"
  output="$TMP_DIR/existing-${entry_name#.}.out"
  if run_bootstrap "$existing" "$output"; then fail "destination with $entry_name must be rejected"; fi
  diff -r "$before" "$existing" >/dev/null || fail "destination with $entry_name was altered"
done

# Linux/BSD CI must additionally prove dangling destination and canonical-name
# symlinks are treated as existing entries. Git Bash on Windows cannot create
# symlinks without host policy support, so probe and skip only on that platform.
SYMLINK_PROBE="$TMP_DIR/symlink-probe"
if ln -s "$TMP_DIR/missing-probe-target" "$SYMLINK_PROBE" 2>/dev/null; then
  DANGLING_TARGET="$TMP_DIR/dangling-destination"
  ln -s "$TMP_DIR/missing-destination-target" "$DANGLING_TARGET"
  if run_bootstrap "$DANGLING_TARGET" "$TMP_DIR/dangling-destination.out"; then
    fail "dangling destination symlink must be rejected"
  fi
  [ -L "$DANGLING_TARGET" ] || fail "dangling destination symlink was altered"
  for entry_name in .env .env.example compose.yaml; do
    existing="$TMP_DIR/symlink-${entry_name#.}"
    mkdir "$existing"
    ln -s "$TMP_DIR/missing-${entry_name#.}-target" "$existing/$entry_name"
    if run_bootstrap "$existing" "$TMP_DIR/symlink-${entry_name#.}.out"; then
      fail "destination with dangling $entry_name symlink must be rejected"
    fi
    [ -L "$existing/$entry_name" ] || fail "dangling $entry_name symlink was altered"
  done

  # Resolve a selected symlinked parent once to its physical directory. Retargeting
  # the caller's symlink during generation must neither redirect publication nor
  # strand private staging/secret material.
  PHYSICAL_PARENT_A="$TMP_DIR/physical-parent-a"
  PHYSICAL_PARENT_B="$TMP_DIR/physical-parent-b"
  PARENT_LINK="$TMP_DIR/mutable-parent"
  mkdir "$PHYSICAL_PARENT_A" "$PHYSICAL_PARENT_B"
  if ln -s "$PHYSICAL_PARENT_A" "$PARENT_LINK" 2>/dev/null && \
     rm -f "$PARENT_LINK" && ln -s "$PHYSICAL_PARENT_B" "$PARENT_LINK" 2>/dev/null && \
     rm -f "$PARENT_LINK" && ln -s "$PHYSICAL_PARENT_A" "$PARENT_LINK" 2>/dev/null; then
    RETARGET_COUNT="$TMP_DIR/retarget.count"
    RETARGET_OUTPUT="$TMP_DIR/retarget.out"
    BOOTSTRAP_PARENT_LINK="$PARENT_LINK" \
      BOOTSTRAP_RETARGET_PARENT="$PHYSICAL_PARENT_B" \
      BOOTSTRAP_RETARGET_COUNT="$RETARGET_COUNT" \
      run_bootstrap "$PARENT_LINK/retarget-deployment" "$RETARGET_OUTPUT" || \
      fail "parent-symlink retarget must not strand staging or redirect publication"
    assert_only_canonical_files "$PHYSICAL_PARENT_A/retarget-deployment"
    assert_absent "$PHYSICAL_PARENT_B/retarget-deployment"
    [ -z "$(ls -A "$PHYSICAL_PARENT_A" | grep '^\.retarget-deployment\.bootstrap\.' || true)" ] || \
      fail "parent retarget left private staging data in the selected physical parent"
    [ -z "$(ls -A "$PHYSICAL_PARENT_B" | grep '^\.retarget-deployment\.bootstrap\.' || true)" ] || \
      fail "parent retarget redirected private staging data"
    assert_secret_values_absent "$RETARGET_OUTPUT"
  else
    printf 'SKIP: host cannot create and retarget parent symlinks; POSIX CI runs parent-retarget regression\n'
  fi
else
  printf 'SKIP: host cannot create symlinks; POSIX CI runs symlink regressions\n'
fi

# The physical-parent trust boundary is also a static POSIX contract on hosts
# where symlink behavior cannot be exercised (notably default Git Bash setups).
assert_contains "$SCRIPT" 'PARENT=$(CDPATH= cd -- "$PARENT" && pwd -P)'

EXISTING_TARGET="$TMP_DIR/existing-deployment"
mkdir -p "$EXISTING_TARGET/data" "$EXISTING_TARGET/postgres_data" "$EXISTING_TARGET/redis_data"
printf 'preserve-existing-env\n' >"$EXISTING_TARGET/.env"
printf 'preserve-app-data\n' >"$EXISTING_TARGET/data/sentinel"
printf 'preserve-postgres-data\n' >"$EXISTING_TARGET/postgres_data/sentinel"
printf 'preserve-redis-data\n' >"$EXISTING_TARGET/redis_data/sentinel"
cp -R "$EXISTING_TARGET" "$TMP_DIR/existing-deployment.before"
EXISTING_OUTPUT="$TMP_DIR/existing.out"
if run_bootstrap "$EXISTING_TARGET" "$EXISTING_OUTPUT"; then fail "existing deployment must be rejected"; fi
diff -r "$TMP_DIR/existing-deployment.before" "$EXISTING_TARGET" >/dev/null || fail "existing deployment was altered"

# A canonical-name collision injected after the first published link cannot be
# overwritten. Rollback removes only bootstrap-owned links and preserves the
# foreign file byte-for-byte, proving publication is not check-then-overwrite.
COLLISION_TARGET="$TMP_DIR/canonical-collision"
COLLISION_OUTPUT="$TMP_DIR/canonical-collision.out"
if BOOTSTRAP_LN_ACTION=collision run_bootstrap "$COLLISION_TARGET" "$COLLISION_OUTPUT"; then
  fail "canonical publication collision must fail"
fi
[ "$(cat "$COLLISION_TARGET/.env.example")" = foreign-collision-content ] || fail "foreign collision was altered"
assert_absent "$COLLISION_TARGET/compose.yaml"
assert_absent "$COLLISION_TARGET/.env"
rm -rf "$COLLISION_TARGET"
run_bootstrap "$COLLISION_TARGET" "$TMP_DIR/canonical-collision-rerun.out" || fail "rerun after collision cleanup failed"
assert_only_canonical_files "$COLLISION_TARGET"

# A handled signal after the directory claim runs the same ownership-checked
# rollback and leaves no partial destination. Git Bash on Windows cannot reliably
# deliver POSIX signals through its process shim, so run this case on POSIX hosts.
case "$(uname -s 2>/dev/null || printf unknown)" in
  MINGW*|MSYS*|CYGWIN*) ;;
  *)
    SIGNAL_TARGET="$TMP_DIR/signal-interruption"
    SIGNAL_OUTPUT="$TMP_DIR/signal-interruption.out"
    if BOOTSTRAP_LN_ACTION=signal-after-first-publish \
      run_bootstrap "$SIGNAL_TARGET" "$SIGNAL_OUTPUT"; then
      fail "handled signal must fail bootstrap"
    fi
    assert_absent "$SIGNAL_TARGET"
    run_bootstrap "$SIGNAL_TARGET" "$TMP_DIR/signal-interruption-rerun.out" || fail "rerun after handled signal failed"
    assert_only_canonical_files "$SIGNAL_TARGET"
    ;;
esac

# Failed curl downloads and missing Compose v2 must not leave a destination.
MISSING_TARGET="$TMP_DIR/missing-download"
MISSING_OUTPUT="$TMP_DIR/missing-download.out"
if run_bootstrap_with_path "$CURL_BIN" "$MISSING_TARGET" "$MISSING_OUTPUT" "" --ref missing-ref; then
  fail "missing canonical curl download must fail"
fi
assert_absent "$MISSING_TARGET"
assert_secret_values_absent "$MISSING_OUTPUT"

NO_COMPOSE_TARGET="$TMP_DIR/no-compose-v2"
NO_COMPOSE_OUTPUT="$TMP_DIR/no-compose-v2.out"
if BOOTSTRAP_DOCKER_V2=no run_bootstrap "$NO_COMPOSE_TARGET" "$NO_COMPOSE_OUTPUT"; then
  fail "missing Compose v2 must fail"
fi
assert_absent "$NO_COMPOSE_TARGET"
assert_contains "$NO_COMPOSE_OUTPUT" "Docker Compose v2"

# Network-free wget fallback with curl absent: exact URL/ref/output behavior,
# success, missing-download failure, cleanup, and secret non-disclosure.
WGET_TARGET="$TMP_DIR/wget-success"
WGET_OUTPUT="$TMP_DIR/wget-success.out"
lines_before=$(wc -l <"$DOWNLOAD_LOG" | tr -d ' ')
run_bootstrap_with_path "$WGET_BIN" "$WGET_TARGET" "$WGET_OUTPUT" -x || fail "wget fallback install failed"
assert_only_canonical_files "$WGET_TARGET"
assert_contains "$DOWNLOAD_LOG" "wget https://fixture.invalid/sub2api/release-test/deploy/compose.yaml"
assert_contains "$DOWNLOAD_LOG" "wget https://fixture.invalid/sub2api/release-test/deploy/.env.example"
[ "$(wc -l <"$DOWNLOAD_LOG" | tr -d ' ')" = "$((lines_before + 2))" ] || fail "wget must download exactly two files"
assert_secret_values_absent "$WGET_OUTPUT"

WGET_MISSING_TARGET="$TMP_DIR/wget-missing"
WGET_MISSING_OUTPUT="$TMP_DIR/wget-missing.out"
if run_bootstrap_with_path "$WGET_BIN" "$WGET_MISSING_TARGET" "$WGET_MISSING_OUTPUT" "" --ref missing-ref; then
  fail "missing canonical wget download must fail"
fi
assert_absent "$WGET_MISSING_TARGET"
assert_secret_values_absent "$WGET_MISSING_OUTPUT"

sh -n "$SCRIPT"
sh -n "$0"
printf 'PASS: Docker bootstrap safely publishes a preserved template without secret disclosure or clobbering\n'
