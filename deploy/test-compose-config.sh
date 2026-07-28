#!/bin/sh
set -eu

DEPLOY_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPOSITORY_ROOT=$(CDPATH= cd -- "$DEPLOY_DIR/.." && pwd)
# The override supports the missing-source regression only. It is used solely
# to locate <root>/Dockerfile; deploy inputs always come from DEPLOY_DIR.
DOCKERFILE_SOURCE_ROOT=${TASK3_DOCKERFILE_SOURCE_ROOT:-$REPOSITORY_ROOT}
SOURCE_DOCKERFILE="$DOCKERFILE_SOURCE_ROOT/Dockerfile"

run_static_postgres_tuning_contract() {
  PYTHON=
  for candidate in python3 python; do
    if "$candidate" -c 'import pathlib' >/dev/null 2>&1; then
      PYTHON=$candidate
      break
    fi
  done
  if [ -z "$PYTHON" ]; then
    printf 'FAIL: python3 or python is required for the static PostgreSQL tuning contract\n' >&2
    exit 1
  fi

  "$PYTHON" - \
    "$DEPLOY_DIR/compose.yaml" \
    "$DEPLOY_DIR/.env.example" \
    "$DEPLOY_DIR/compose.bind.yaml" \
    "$DEPLOY_DIR/compose.dev.yaml" \
    "$DEPLOY_DIR/compose.external.yaml" <<'PY'
import pathlib
import sys

compose = pathlib.Path(sys.argv[1]).read_text(encoding="utf-8")
env_example = pathlib.Path(sys.argv[2]).read_text(encoding="utf-8")
variants = {
    pathlib.Path(path).name: pathlib.Path(path).read_text(encoding="utf-8")
    for path in sys.argv[3:]
}


def require(condition, message):
    if not condition:
        raise AssertionError(message)


settings = {
    "POSTGRES_MAX_CONNECTIONS": "max_connections",
    "POSTGRES_SHARED_BUFFERS": "shared_buffers",
    "POSTGRES_EFFECTIVE_CACHE_SIZE": "effective_cache_size",
    "POSTGRES_MAINTENANCE_WORK_MEM": "maintenance_work_mem",
}

require("entrypoint:" in compose, "PostgreSQL tuning must preserve initialization through an entrypoint wrapper")
require('exec /usr/local/bin/docker-entrypoint.sh "$$@"' in compose, "wrapper must delegate to the image entrypoint")
require('command: ["postgres"]' in compose, "PostgreSQL must retain the image's default server command")
for environment_name, setting_name in settings.items():
    require(
        f"{environment_name}: ${{{environment_name}:-}}" in compose,
        f"{environment_name} must default to blank rather than pinning a PostgreSQL value",
    )
    require(
        f'if [ -n "$${{{environment_name}}}" ]; then' in compose,
        f"{environment_name} must apply only when nonblank",
    )
    require(
        f'set -- "$$@" -c "{setting_name}=$${{{environment_name}}}"' in compose,
        f"{environment_name} must map to the fixed {setting_name} parameter",
    )
    require(
        f"# {environment_name}=" in env_example,
        f"{environment_name} must be documented as an opt-in setting",
    )
    require(
        f"\n{environment_name}=" not in env_example,
        f"{environment_name} must not be enabled by default in .env.example",
    )

require(
    "postgres_data:/var/lib/postgresql/data" in compose
    and "PGDATA: /var/lib/postgresql/data" in compose,
    "PostgreSQL tuning must not change the existing data path",
)
require("image: postgres:18-alpine" in compose, "PostgreSQL tuning must not change the major version")
require(
    "postgres:" not in variants["compose.external.yaml"],
    "external infrastructure variant must not add managed PostgreSQL",
)
for variant_name in ("compose.bind.yaml", "compose.dev.yaml"):
    require(
        "entrypoint:" not in variants[variant_name] and "command:" not in variants[variant_name],
        f"{variant_name} must inherit the canonical PostgreSQL tuning wrapper",
    )

print("PASS: static PostgreSQL Compose tuning contract")
PY
}

if [ "${1-}" = "--static-postgres-tuning" ]; then
  run_static_postgres_tuning_contract
  exit 0
fi

TMP_DIR=$(mktemp -d)
FIXTURE_ROOT="$TMP_DIR/repository"
FIXTURE_DEPLOY="$FIXTURE_ROOT/deploy"
trap 'rm -rf "$TMP_DIR"' EXIT HUP INT TERM

if [ ! -f "$SOURCE_DOCKERFILE" ]; then
  printf 'FAIL: repository root Dockerfile does not exist\n' >&2
  exit 1
fi
if [ ! -s "$SOURCE_DOCKERFILE" ]; then
  printf 'FAIL: repository root Dockerfile is empty\n' >&2
  exit 1
fi

mkdir -p "$FIXTURE_DEPLOY"
cp "$DEPLOY_DIR/.env.example" "$FIXTURE_DEPLOY/.env"
cp "$SOURCE_DOCKERFILE" "$FIXTURE_ROOT/Dockerfile"
if [ ! -s "$FIXTURE_ROOT/Dockerfile" ]; then
  printf 'FAIL: fixture Dockerfile is missing or empty\n' >&2
  exit 1
fi
if ! cmp -s "$SOURCE_DOCKERFILE" "$FIXTURE_ROOT/Dockerfile"; then
  printf 'FAIL: fixture Dockerfile does not match the repository root Dockerfile\n' >&2
  exit 1
fi

set_fixture() {
  key=$1
  value=$2
  found=false
  while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in
      "$key="*)
        printf '%s=%s\n' "$key" "$value"
        found=true
        ;;
      *) printf '%s\n' "$line" ;;
    esac
  done < "$FIXTURE_DEPLOY/.env" > "$FIXTURE_DEPLOY/.env.next"
  if [ "$found" = false ]; then
    printf '%s=%s\n' "$key" "$value" >> "$FIXTURE_DEPLOY/.env.next"
  fi
  mv "$FIXTURE_DEPLOY/.env.next" "$FIXTURE_DEPLOY/.env"
}

remove_fixture() {
  key=$1
  sed "/^${key}=/d" "$FIXTURE_DEPLOY/.env" > "$FIXTURE_DEPLOY/.env.next"
  mv "$FIXTURE_DEPLOY/.env.next" "$FIXTURE_DEPLOY/.env"
}

# Compose interpolation must depend only on the isolated fixture. Preserve the
# platform paths Docker needs, but discard caller and Compose control variables.
clean_env() {
  env -i \
    PATH="${PATH-}" \
    HOME="${HOME-}" \
    USERPROFILE="${USERPROFILE-}" \
    SYSTEMROOT="${SYSTEMROOT-}" \
    WINDIR="${WINDIR-}" \
    COMSPEC="${COMSPEC-}" \
    APPDATA="${APPDATA-}" \
    LOCALAPPDATA="${LOCALAPPDATA-}" \
    TEMP="${TEMP-}" \
    TMP="${TMP-}" \
    TMPDIR="${TMPDIR-}" \
    PROGRAMFILES="${PROGRAMFILES-}" \
    'ProgramFiles(x86)'="$(printenv 'ProgramFiles(x86)' 2>/dev/null || :)" \
    ProgramW6432="${ProgramW6432-}" \
    ProgramData="${ProgramData-}" \
    COMMONPROGRAMFILES="${COMMONPROGRAMFILES-}" \
    'CommonProgramFiles(x86)'="$(printenv 'CommonProgramFiles(x86)' 2>/dev/null || :)" \
    HOMEDRIVE="${HOMEDRIVE-}" \
    HOMEPATH="${HOMEPATH-}" \
    PATHEXT="${PATHEXT-}" \
    "$@"
}

compose_config() {
  clean_env docker compose --project-directory "$FIXTURE_DEPLOY" -f "$FIXTURE_DEPLOY/compose.yaml" config "$@"
}

compose_files_config() {
  output=$1
  shift
  clean_env docker compose --project-directory "$FIXTURE_DEPLOY" "$@" config --format json > "$output"
}

# !override was added in Compose v2.24.4. Probe the parser directly so older
# clients fail with an actionable message rather than an opaque YAML-tag error.
printf '%s\n' 'services:' '  probe:' '    image: scratch' '    ports:' '      - "8080:8080"' > "$TMP_DIR/override-base.yaml"
printf '%s\n' 'services:' '  probe:' '    ports: !override' '      - "127.0.0.1:8080:8080"' > "$TMP_DIR/override.yaml"
if ! clean_env docker compose -f "$TMP_DIR/override-base.yaml" -f "$TMP_DIR/override.yaml" \
  config --quiet >/dev/null 2>&1
then
  compose_version=$(clean_env docker compose version --short 2>/dev/null || printf 'unknown')
  printf 'FAIL: Docker Compose v2.24.4+ with !override support is required (found %s)\n' "$compose_version" >&2
  exit 1
fi

set_fixture DATABASE_PASSWORD test-database-password
set_fixture DATABASE_HOST external-postgres.example
set_fixture REDIS_HOST external-redis.example
set_fixture LOG_LEVEL warn
set_fixture SERVER_H2C_ENABLED true
set_fixture GATEWAY_MAX_CONNS_PER_HOST 321
set_fixture DASHBOARD_AGGREGATION_RETENTION_USAGE_LOGS_DAYS 47
set_fixture OPS_ENABLED false

if [ ! -f "$DEPLOY_DIR/compose.yaml" ]; then
  printf 'FAIL: deploy/compose.yaml does not exist\n' >&2
  exit 1
fi
cp "$DEPLOY_DIR/compose.yaml" "$FIXTURE_DEPLOY/compose.yaml"
for variant in compose.bind.yaml compose.dev.yaml compose.external.yaml; do
  if [ ! -f "$DEPLOY_DIR/$variant" ]; then
    printf 'FAIL: deploy/%s does not exist\n' "$variant" >&2
    exit 1
  fi
  cp "$DEPLOY_DIR/$variant" "$FIXTURE_DEPLOY/$variant"
done

# These values deliberately conflict with the fixture. Every ordinary render
# below must ignore them, including Compose and Docker control variables.
export AUTO_SETUP=ambient-auto-setup
export SERVER_HOST=ambient-server-host
export SERVER_PORT=19090
export DATABASE_HOST=ambient-database-host
export DATABASE_PORT=15432
export DATABASE_USER=ambient-database-user
export DATABASE_PASSWORD=ambient-database-password
export DATABASE_DBNAME=ambient-database-name
export REDIS_HOST=ambient-redis-host
export REDIS_PORT=16379
export REDIS_PASSWORD=ambient-redis-password
export REDIS_MAXCLIENTS=19999
export TOTP_ENCRYPTION_KEY=ambient-totp-key
export SUB2API_IMAGE=ambient.example/sub2api
export SUB2API_VERSION=ambient-version
export PUBLISH_HOST=127.9.9.9
export PUBLISH_PORT=19080
export TZ=Ambient/Timezone
export COMPOSE_FILE=ambient-compose.yaml
export COMPOSE_PROJECT_NAME=ambient-project
export COMPOSE_PROFILES=ambient-profile
export COMPOSE_ENV_FILES=ambient.env
export COMPOSE_DISABLE_ENV_FILE=1
export COMPOSE_PATH_SEPARATOR=';'
export COMPOSE_CONVERT_WINDOWS_PATHS=1
export COMPOSE_IGNORE_ORPHANS=1
export COMPOSE_REMOVE_ORPHANS=1
export COMPOSE_PARALLEL_LIMIT=1
export COMPOSE_ANSI=always
export COMPOSE_PROGRESS=plain
export COMPOSE_STATUS_STDOUT=1
export COMPOSE_MENU=1
export COMPOSE_EXPERIMENTAL=1
export COMPOSE_BAKE=1
export DOCKER_HOST=tcp://127.0.0.1:1
export DOCKER_CONTEXT=ambient-context
export DOCKER_CONFIG="$TMP_DIR/ambient-docker-config"
export DOCKER_DEFAULT_PLATFORM=ambient/platform

if compose_config --quiet >/dev/null 2>&1; then
  printf 'FAIL: blank TOTP_ENCRYPTION_KEY must reject the deployment render\n' >&2
  exit 1
fi
set_fixture TOTP_ENCRYPTION_KEY test-totp-encryption-key

# External infrastructure and identity inputs are mandatory independently; Redis
# authentication and conventional service ports intentionally remain optional.
for required in \
  DATABASE_HOST=external-postgres.example \
  DATABASE_USER=sub2api \
  DATABASE_PASSWORD=test-database-password \
  DATABASE_DBNAME=sub2api \
  REDIS_HOST=external-redis.example \
  TOTP_ENCRYPTION_KEY=test-totp-encryption-key
do
  key=${required%%=*}
  value=${required#*=}
  remove_fixture "$key"
  if clean_env docker compose --project-directory "$FIXTURE_DEPLOY" \
    -f "$FIXTURE_DEPLOY/compose.external.yaml" config --quiet >/dev/null 2>&1
  then
    printf 'FAIL: external render must require %s\n' "$key" >&2
    exit 1
  fi
  set_fixture "$key" "$value"
done

compose_config --format json > "$TMP_DIR/config-redis-blank.json"
compose_config --format json --no-env-resolution > "$TMP_DIR/config-unresolved.json"
clean_env env \
  POSTGRES_MAX_CONNECTIONS=240 \
  POSTGRES_SHARED_BUFFERS=512MB \
  POSTGRES_EFFECTIVE_CACHE_SIZE=1536MB \
  POSTGRES_MAINTENANCE_WORK_MEM=96MB \
  docker compose --project-directory "$FIXTURE_DEPLOY" -f "$FIXTURE_DEPLOY/compose.yaml" \
  config --format json > "$TMP_DIR/config-postgres-tuned.json"

compose_files_config "$TMP_DIR/config-prod-bind.json" \
  -f "$FIXTURE_DEPLOY/compose.yaml" -f "$FIXTURE_DEPLOY/compose.bind.yaml"
compose_files_config "$TMP_DIR/config-external-redis-blank.json" \
  -f "$FIXTURE_DEPLOY/compose.external.yaml"

compose_files_config "$TMP_DIR/config-dev-named.json" \
  -f "$FIXTURE_DEPLOY/compose.yaml" -f "$FIXTURE_DEPLOY/compose.dev.yaml"
compose_files_config "$TMP_DIR/config-dev-bind.json" \
  -f "$FIXTURE_DEPLOY/compose.yaml" -f "$FIXTURE_DEPLOY/compose.bind.yaml" -f "$FIXTURE_DEPLOY/compose.dev.yaml"

# Double dollars remain literal in Compose .env values, exercising credential
# propagation without allowing host interpolation or shell evaluation.
ADVERSARIAL_REDIS_PASSWORD='fixture redis $$dollar;semi&and|pipe`tick`$$(literal)'
set_fixture REDIS_PASSWORD "$ADVERSARIAL_REDIS_PASSWORD"
compose_config --format json > "$TMP_DIR/config-redis-auth.json"
compose_files_config "$TMP_DIR/config-prod-bind-redis-auth.json" \
  -f "$FIXTURE_DEPLOY/compose.yaml" -f "$FIXTURE_DEPLOY/compose.bind.yaml"
compose_files_config "$TMP_DIR/config-external-redis-auth.json" \
  -f "$FIXTURE_DEPLOY/compose.external.yaml"
compose_files_config "$TMP_DIR/config-dev-named-redis-auth.json" \
  -f "$FIXTURE_DEPLOY/compose.yaml" -f "$FIXTURE_DEPLOY/compose.dev.yaml"
compose_files_config "$TMP_DIR/config-dev-bind-redis-auth.json" \
  -f "$FIXTURE_DEPLOY/compose.yaml" -f "$FIXTURE_DEPLOY/compose.bind.yaml" -f "$FIXTURE_DEPLOY/compose.dev.yaml"

# This render intentionally adds known shell-over-.env values after sanitizing
# the environment. The app and managed dependencies must receive the same ones.
clean_env env \
  DATABASE_USER=override-database-user \
  DATABASE_PASSWORD=override-database-password \
  DATABASE_DBNAME=override-database-name \
  REDIS_PASSWORD=override-redis-password \
  TOTP_ENCRYPTION_KEY=override-totp-key \
  SUB2API_IMAGE=registry.example/sub2api \
  SUB2API_VERSION=fixture-version \
  PUBLISH_HOST=127.0.0.2 \
  PUBLISH_PORT=18080 \
  docker compose --project-directory "$FIXTURE_DEPLOY" -f "$FIXTURE_DEPLOY/compose.yaml" \
  config --format json > "$TMP_DIR/config-shell-overrides.json"

clean_env env \
  DATABASE_HOST=override-external-database-host \
  DATABASE_USER=override-external-database-user \
  DATABASE_PASSWORD=override-external-database-password \
  DATABASE_DBNAME=override-external-database-name \
  REDIS_HOST=override-external-redis-host \
  REDIS_PASSWORD=override-external-redis-password \
  TOTP_ENCRYPTION_KEY=override-external-totp-key \
  docker compose --project-directory "$FIXTURE_DEPLOY" -f "$FIXTURE_DEPLOY/compose.external.yaml" \
  config --format json > "$TMP_DIR/config-external-shell-overrides.json"

clean_env docker compose --project-directory "$FIXTURE_DEPLOY" \
  -f "$FIXTURE_DEPLOY/compose.external.yaml" config --format json --no-env-resolution \
  > "$TMP_DIR/config-external-unresolved.json"

PYTHON=
for candidate in python3 python; do
  if "$candidate" -c 'import json' >/dev/null 2>&1; then
    PYTHON=$candidate
    break
  fi
done
if [ -z "$PYTHON" ]; then
  printf 'FAIL: python3 or python is required to inspect Compose JSON\n' >&2
  exit 1
fi

"$PYTHON" - "$TMP_DIR/config-redis-blank.json" "$TMP_DIR/config-redis-auth.json" \
  "$TMP_DIR/config-unresolved.json" "$TMP_DIR/config-shell-overrides.json" \
  "$TMP_DIR/config-prod-bind.json" "$TMP_DIR/config-prod-bind-redis-auth.json" \
  "$TMP_DIR/config-dev-named.json" "$TMP_DIR/config-dev-named-redis-auth.json" \
  "$TMP_DIR/config-dev-bind.json" "$TMP_DIR/config-dev-bind-redis-auth.json" \
  "$TMP_DIR/config-external-redis-blank.json" "$TMP_DIR/config-external-redis-auth.json" \
  "$TMP_DIR/config-external-shell-overrides.json" "$TMP_DIR/config-external-unresolved.json" \
  "$TMP_DIR/config-postgres-tuned.json" "$FIXTURE_ROOT" <<'PY'
import json
import os
import sys

with open(sys.argv[1], encoding="utf-8") as handle:
    config = json.load(handle)
with open(sys.argv[2], encoding="utf-8") as handle:
    config_redis_auth = json.load(handle)
with open(sys.argv[3], encoding="utf-8") as handle:
    unresolved = json.load(handle)
with open(sys.argv[4], encoding="utf-8") as handle:
    shell_overrides = json.load(handle)
with open(sys.argv[5], encoding="utf-8") as handle:
    prod_bind = json.load(handle)
with open(sys.argv[6], encoding="utf-8") as handle:
    prod_bind_redis_auth = json.load(handle)
with open(sys.argv[7], encoding="utf-8") as handle:
    dev_named = json.load(handle)
with open(sys.argv[8], encoding="utf-8") as handle:
    dev_named_redis_auth = json.load(handle)
with open(sys.argv[9], encoding="utf-8") as handle:
    dev_bind = json.load(handle)
with open(sys.argv[10], encoding="utf-8") as handle:
    dev_bind_redis_auth = json.load(handle)
with open(sys.argv[11], encoding="utf-8") as handle:
    external_blank = json.load(handle)
with open(sys.argv[12], encoding="utf-8") as handle:
    external_auth = json.load(handle)
with open(sys.argv[13], encoding="utf-8") as handle:
    external_shell_overrides = json.load(handle)
with open(sys.argv[14], encoding="utf-8") as handle:
    external_unresolved = json.load(handle)
with open(sys.argv[15], encoding="utf-8") as handle:
    postgres_tuned = json.load(handle)
fixture_root = os.path.normcase(os.path.realpath(sys.argv[16]))

services = config["services"]
sub2api = services["sub2api"]
sub2api_unresolved = unresolved["services"]["sub2api"]


def require(condition, message):
    if not condition:
        raise AssertionError(message)


def mounts_at(rendered, service_name, target):
    return [
        volume
        for volume in rendered["services"][service_name].get("volumes", [])
        if volume.get("target") == target
    ]


def require_named_mount(service_name, source, target):
    mounts = mounts_at(config, service_name, target)
    require(len(mounts) == 1, f"{service_name} must mount exactly one volume at {target}")
    mount = mounts[0]
    require(mount.get("type") == "volume", f"{service_name} {target} mount must be a named volume")
    require(mount.get("source") == source, f"{service_name} {target} mount must use {source}")
    require(source in config.get("volumes", {}), f"top-level named volume {source} must exist")


def require_bind_mount(rendered, service_name, source_basename, target):
    mounts = mounts_at(rendered, service_name, target)
    require(len(mounts) == 1, f"{service_name} must mount exactly one bind at {target}")
    mount = mounts[0]
    require(mount.get("type") == "bind", f"{service_name} {target} mount must be a bind")
    require(
        os.path.basename(os.path.normpath(mount.get("source", ""))) == source_basename,
        f"{service_name} {target} bind must use ./{source_basename}",
    )
    require("bind" not in mount or not mount["bind"].get("selinux"), "bind mounts must not force SELinux relabeling")


def require_single_publication(rendered, host_ip):
    ports = rendered["services"]["sub2api"].get("ports", [])
    require(len(ports) == 1, "sub2api must publish exactly one HTTP port")
    require(ports[0].get("host_ip") == host_ip, f"sub2api publication host must be {host_ip}")
    require(str(ports[0].get("published")) == "8080", "sub2api published port must be 8080")
    require(ports[0].get("target") == 8080, "sub2api published target must be 8080")


def require_app_env_file(rendered):
    env_files = rendered["services"]["sub2api"].get("env_file", [])
    require(
        any(
            os.path.basename(item.get("path", "") if isinstance(item, dict) else item) == ".env"
            for item in env_files
        ),
        "sub2api must declare env_file .env",
    )


def require_no_fixed_names_or_dependency_ports(rendered):
    for service_name, service in rendered["services"].items():
        require("container_name" not in service, f"{service_name} must not declare container_name")
    for service_name in ("postgres", "redis"):
        if service_name in rendered["services"]:
            require("ports" not in rendered["services"][service_name], f"{service_name} must not publish a host port")


require_app_env_file(unresolved)

require(
    sub2api.get("image") == "ghcr.io/is7qin/sub2api:latest",
    "sub2api must render the canonical default image",
)
app_env = sub2api.get("environment", {})
require(app_env.get("LOG_LEVEL") == "warn", "sub2api must receive LOG_LEVEL from .env")
require(app_env.get("SERVER_H2C_ENABLED") == "true", "sub2api must receive SERVER_H2C_ENABLED from .env")
require(
    app_env.get("GATEWAY_MAX_CONNS_PER_HOST") == "321",
    "sub2api must receive GATEWAY_MAX_CONNS_PER_HOST from .env",
)
require(
    app_env.get("DASHBOARD_AGGREGATION_RETENTION_USAGE_LOGS_DAYS") == "47",
    "sub2api must receive usage-log retention from .env",
)
require(app_env.get("OPS_ENABLED") == "false", "sub2api must receive OPS_ENABLED from .env")
require(
    app_env.get("DASHBOARD_AGGREGATION_RETENTION_USAGE_BILLING_DEDUP_DAYS") == "365",
    "sub2api must receive usage-billing dedup retention from .env",
)
require(
    "DASHBOARD_AGGREGATION_RETENTION_HOURLY_DAYS" not in app_env
    and "DASHBOARD_AGGREGATION_RETENTION_DAILY_DAYS" not in app_env,
    "optional backend-default retention windows must remain unpinned",
)
require(app_env.get("AUTO_SETUP") == "true", "sub2api AUTO_SETUP must be true")
require(app_env.get("SERVER_HOST") == "0.0.0.0", "sub2api SERVER_HOST must be 0.0.0.0")
require(app_env.get("SERVER_PORT") == "8080", "sub2api SERVER_PORT must be 8080")
require(app_env.get("DATABASE_HOST") == "postgres", "sub2api DATABASE_HOST must be postgres")
require(app_env.get("DATABASE_PORT") == "5432", "sub2api DATABASE_PORT must be 5432")
require(app_env.get("REDIS_HOST") == "redis", "sub2api REDIS_HOST must be redis")
require(app_env.get("REDIS_PORT") == "6379", "sub2api REDIS_PORT must be 6379")
require(
    app_env.get("TOTP_ENCRYPTION_KEY") == "test-totp-encryption-key",
    "sub2api must receive the required stable TOTP_ENCRYPTION_KEY",
)
require(app_env.get("DATABASE_USER") == "sub2api", "sub2api must receive fixture DATABASE_USER")
require(
    app_env.get("DATABASE_PASSWORD") == "test-database-password",
    "sub2api must receive fixture DATABASE_PASSWORD",
)
require(app_env.get("DATABASE_DBNAME") == "sub2api", "sub2api must receive fixture DATABASE_DBNAME")
require(app_env.get("REDIS_PASSWORD") == "", "sub2api must receive the blank fixture REDIS_PASSWORD")

explicit_app_env = sub2api_unresolved.get("environment", {})
for key in ("DATABASE_USER", "DATABASE_PASSWORD", "DATABASE_DBNAME", "REDIS_PASSWORD"):
    require(key in explicit_app_env, f"sub2api must explicitly map Compose-interpolated {key}")

ports = sub2api.get("ports", [])
require(len(ports) == 1, "sub2api must publish exactly one HTTP port")
require(ports[0].get("host_ip") == "0.0.0.0", "sub2api default publication host must be 0.0.0.0")
require(str(ports[0].get("published")) == "8080", "sub2api default published port must be 8080")
require(ports[0].get("target") == 8080, "sub2api published target must be 8080")

postgres = services["postgres"]
postgres_env = postgres.get("environment", {})
require(
    postgres_env.get("PGDATA") == "/var/lib/postgresql/data",
    "PostgreSQL PGDATA must be /var/lib/postgresql/data",
)
require(postgres_env.get("POSTGRES_USER") == app_env.get("DATABASE_USER"), "database user mapping must match")
require(
    postgres_env.get("POSTGRES_PASSWORD") == app_env.get("DATABASE_PASSWORD"),
    "database password mapping must match",
)
require(postgres_env.get("POSTGRES_DB") == app_env.get("DATABASE_DBNAME"), "database name mapping must match")

postgres_tuning = {
    "POSTGRES_MAX_CONNECTIONS": ("max_connections", "240"),
    "POSTGRES_SHARED_BUFFERS": ("shared_buffers", "512MB"),
    "POSTGRES_EFFECTIVE_CACHE_SIZE": ("effective_cache_size", "1536MB"),
    "POSTGRES_MAINTENANCE_WORK_MEM": ("maintenance_work_mem", "96MB"),
}
postgres_command = postgres.get("command", [])
postgres_entrypoint = postgres.get("entrypoint", [])
require(postgres_command == ["postgres"], "default PostgreSQL command must remain postgres without pinned tuning")
require(len(postgres_entrypoint) == 4, "PostgreSQL tuning wrapper must have a fixed entrypoint shape")
postgres_entrypoint_source = postgres_entrypoint[2]
require(
    'exec /usr/local/bin/docker-entrypoint.sh "$$@"' in postgres_entrypoint_source,
    "PostgreSQL wrapper must preserve image initialization",
)
for environment_name, (setting_name, expected_value) in postgres_tuning.items():
    require(postgres_env.get(environment_name) == "", f"blank {environment_name} must remain blank")
    require(
        f'-c "{setting_name}=$${{{environment_name}}}"' in postgres_entrypoint_source,
        f"PostgreSQL wrapper must map only {environment_name} to {setting_name}",
    )

postgres_tuned_service = postgres_tuned["services"]["postgres"]
require(
    postgres_tuned_service.get("command") == postgres_command
    and postgres_tuned_service.get("entrypoint") == postgres_entrypoint,
    "explicit PostgreSQL tuning must stay in environment values, not generated shell source",
)
postgres_tuned_env = postgres_tuned_service.get("environment", {})
for environment_name, (_, expected_value) in postgres_tuning.items():
    require(
        postgres_tuned_env.get(environment_name) == expected_value,
        f"explicit {environment_name} must render unchanged",
    )

for rendered in (prod_bind, dev_named, dev_bind):
    variant_postgres = rendered["services"]["postgres"]
    require(variant_postgres.get("command") == postgres_command, "variants must retain PostgreSQL command")
    require(variant_postgres.get("entrypoint") == postgres_entrypoint, "variants must retain PostgreSQL tuning wrapper")

require_named_mount("sub2api", "sub2api_data", "/app/data")
require_named_mount("postgres", "postgres_data", "/var/lib/postgresql/data")
require_named_mount("redis", "redis_data", "/data")

require("ports" not in postgres, "PostgreSQL must not publish a host port")
require("ports" not in services["redis"], "Redis must not publish a host port")
for service_name, service in services.items():
    require("container_name" not in service, f"{service_name} must not declare container_name")

app_dependencies = sub2api.get("depends_on", {})
require(
    app_dependencies.get("postgres", {}).get("condition") == "service_healthy",
    "sub2api must wait for healthy PostgreSQL",
)
require(
    app_dependencies.get("redis", {}).get("condition") == "service_healthy",
    "sub2api must wait for healthy Redis",
)
require(
    "healthcheck" not in sub2api_unresolved,
    "sub2api must inherit the image healthcheck without an override",
)

redis = services["redis"]
redis_auth = config_redis_auth["services"]["redis"]
redis_env = redis.get("environment", {})
redis_auth_env = redis_auth.get("environment", {})
redis_auth_app_env = config_redis_auth["services"]["sub2api"].get("environment", {})
adversarial_redis_password = "fixture redis $$dollar;semi&and|pipe`tick`$$(literal)"
require(redis_env.get("REDIS_PASSWORD") == "", "blank Redis mode must preserve REDIS_PASSWORD")
require(redis_env.get("REDISCLI_AUTH") == "", "blank Redis mode must preserve REDISCLI_AUTH")
require(redis_env.get("REDIS_MAXCLIENTS") == "10000", "Redis must receive fixture REDIS_MAXCLIENTS")
require(
    redis_auth_env.get("REDIS_PASSWORD") == adversarial_redis_password,
    "authenticated Redis mode must preserve spaces and shell metacharacters in REDIS_PASSWORD",
)
require(
    redis_auth_env.get("REDISCLI_AUTH") == adversarial_redis_password,
    "authenticated Redis mode must preserve the adversarial password in REDISCLI_AUTH",
)
require(
    redis_auth_app_env.get("REDIS_PASSWORD") == redis_auth_env.get("REDIS_PASSWORD"),
    "sub2api and authenticated Redis must share REDIS_PASSWORD",
)

redis_command = redis.get("command", [])
redis_auth_command = redis_auth.get("command", [])
require(redis_command == redis_auth_command, "Redis runtime command must not embed the rendered password")
command_source = redis_command[-1] if redis_command else ""
require("${REDIS_MAXCLIENTS}" in command_source, "Redis command must read container-side REDIS_MAXCLIENTS")
require("${REDIS_PASSWORD}" in command_source, "Redis command must read container-side REDIS_PASSWORD")
require(
    'if [ -n "$${REDIS_PASSWORD}" ]; then' in command_source,
    "Redis command must add requirepass only for a nonblank password",
)
require('--requirepass "$${REDIS_PASSWORD}"' in command_source, "Redis command must append requirepass safely")
require('exec "$$@"' in command_source, "Redis command must exec the final argv")
require(adversarial_redis_password not in command_source, "Redis password must not be interpolated into shell source")

unresolved_redis = unresolved["services"]["redis"]
unresolved_command = unresolved_redis.get("command", [])
unresolved_source = unresolved_command[-1] if unresolved_command else ""
require(
    "$${REDIS_MAXCLIENTS}" in unresolved_source,
    "Compose YAML must escape REDIS_MAXCLIENTS for container-side expansion",
)
require(
    "$${REDIS_PASSWORD}" in unresolved_source,
    "Compose YAML must escape REDIS_PASSWORD for container-side expansion",
)
require('exec "$$@"' in unresolved_source, "Compose YAML must escape final argv expansion")

override_services = shell_overrides["services"]
override_app = override_services["sub2api"]
override_app_env = override_app.get("environment", {})
override_postgres_env = override_services["postgres"].get("environment", {})
override_redis_env = override_services["redis"].get("environment", {})
require(override_app.get("image") == "registry.example/sub2api:fixture-version", "image variables must be interpolated")
require(override_app_env.get("DATABASE_USER") == "override-database-user", "app must receive shell DATABASE_USER")
require(
    override_app_env.get("DATABASE_PASSWORD") == "override-database-password",
    "app must receive shell DATABASE_PASSWORD",
)
require(override_app_env.get("DATABASE_DBNAME") == "override-database-name", "app must receive shell DATABASE_DBNAME")
require(
    override_postgres_env.get("POSTGRES_USER") == override_app_env.get("DATABASE_USER"),
    "shell-over-.env database user must remain shared",
)
require(
    override_postgres_env.get("POSTGRES_PASSWORD") == override_app_env.get("DATABASE_PASSWORD"),
    "shell-over-.env database password must remain shared",
)
require(
    override_postgres_env.get("POSTGRES_DB") == override_app_env.get("DATABASE_DBNAME"),
    "shell-over-.env database name must remain shared",
)
require(
    override_app_env.get("REDIS_PASSWORD") == "override-redis-password"
    and override_redis_env.get("REDIS_PASSWORD") == override_app_env.get("REDIS_PASSWORD")
    and override_redis_env.get("REDISCLI_AUTH") == override_app_env.get("REDIS_PASSWORD"),
    "shell-over-.env Redis password must remain shared",
)
override_ports = override_app.get("ports", [])
require(
    len(override_ports) == 1
    and override_ports[0].get("host_ip") == "127.0.0.2"
    and str(override_ports[0].get("published")) == "18080",
    "publication variables must be interpolated only when explicitly supplied",
)

require(config["services"]["sub2api"].get("build") is None, "production must not define an app build")
require_no_fixed_names_or_dependency_ports(config)
for rendered in (prod_bind, prod_bind_redis_auth, dev_bind, dev_bind_redis_auth):
    require_bind_mount(rendered, "sub2api", "data", "/app/data")
    require_bind_mount(rendered, "postgres", "postgres_data", "/var/lib/postgresql/data")
    require_bind_mount(rendered, "redis", "redis_data", "/data")

for rendered in (dev_named, dev_named_redis_auth, dev_bind, dev_bind_redis_auth):
    app = rendered["services"]["sub2api"]
    build = app.get("build", {})
    rendered_context = os.path.normcase(os.path.realpath(build.get("context", "")))
    require(rendered_context == fixture_root, "dev build context must equal the fixture repository root")
    require(build.get("dockerfile") == "Dockerfile", "dev must build the root Dockerfile")
    require(app.get("environment", {}).get("SERVER_MODE") == "debug", "dev app must use debug mode")
    require_single_publication(rendered, "127.0.0.1")
    require_no_fixed_names_or_dependency_ports(rendered)

for rendered in (dev_named, dev_named_redis_auth):
    rendered_services = rendered["services"]
    for service_name, source, target in (
        ("sub2api", "sub2api_data", "/app/data"),
        ("postgres", "postgres_data", "/var/lib/postgresql/data"),
        ("redis", "redis_data", "/data"),
    ):
        mounts = mounts_at(rendered, service_name, target)
        require(len(mounts) == 1, f"dev named {service_name} must have one mount at {target}")
        require(mounts[0].get("type") == "volume", f"dev named {service_name} must retain named storage")
        require(mounts[0].get("source") == source, f"dev named {service_name} must use {source}")

external_explicit_env = external_unresolved["services"]["sub2api"].get("environment", {})
for key in ("DATABASE_HOST", "DATABASE_USER", "DATABASE_PASSWORD", "DATABASE_DBNAME", "REDIS_HOST", "REDIS_PASSWORD"):
    require(key in external_explicit_env, f"external sub2api must explicitly map Compose-interpolated {key}")

external_override_env = external_shell_overrides["services"]["sub2api"].get("environment", {})
for key, value in (
    ("DATABASE_HOST", "override-external-database-host"),
    ("DATABASE_USER", "override-external-database-user"),
    ("DATABASE_PASSWORD", "override-external-database-password"),
    ("DATABASE_DBNAME", "override-external-database-name"),
    ("REDIS_HOST", "override-external-redis-host"),
    ("REDIS_PASSWORD", "override-external-redis-password"),
):
    require(external_override_env.get(key) == value, f"external app must receive shell {key} over .env")

for rendered, expected_password in ((external_blank, ""), (external_auth, adversarial_redis_password)):
    require(set(rendered["services"]) == {"sub2api"}, "external topology must define only sub2api")
    app = rendered["services"]["sub2api"]
    require(app.get("image") == "ghcr.io/is7qin/sub2api:latest", "external must use the released app image")
    require(app.get("build") is None, "external must not define an app build")
    require("healthcheck" not in app, "external app must inherit the image healthcheck")
    require("depends_on" not in app, "external app must not declare managed dependencies")
    require_no_fixed_names_or_dependency_ports(rendered)
    external_env = app.get("environment", {})
    require(external_env.get("AUTO_SETUP") == "true", "external AUTO_SETUP must be true")
    require(external_env.get("SERVER_HOST") == "0.0.0.0", "external SERVER_HOST must be 0.0.0.0")
    require(external_env.get("SERVER_PORT") == "8080", "external SERVER_PORT must be 8080")
    require(external_env.get("TOTP_ENCRYPTION_KEY") == "test-totp-encryption-key", "external TOTP key must be explicitly mapped")
    require(external_env.get("DATABASE_HOST") == "external-postgres.example", "external database host must be required and mapped")
    require(external_env.get("DATABASE_PORT") == "5432", "external database port must be mapped")
    require(external_env.get("DATABASE_USER") == "sub2api", "external database user must be mapped")
    require(external_env.get("DATABASE_PASSWORD") == "test-database-password", "external database password must be required and mapped")
    require(external_env.get("DATABASE_DBNAME") == "sub2api", "external database name must be mapped")
    require(external_env.get("REDIS_HOST") == "external-redis.example", "external Redis host must be required and mapped")
    require(external_env.get("REDIS_PORT") == "6379", "external Redis port must be mapped")
    require(external_env.get("REDIS_PASSWORD") == expected_password, "external Redis password must support blank and nonblank values")
    mounts = mounts_at(rendered, "sub2api", "/app/data")
    require(len(mounts) == 1 and mounts[0].get("type") == "volume", "external app data must use one named volume")
    require(mounts[0].get("source") == "sub2api_data", "external app data must use sub2api_data")
    require(set(rendered.get("volumes", {})) == {"sub2api_data"}, "external must define only the app-data volume")

print("PASS: canonical and variant deploy Compose contracts (ambient environment sanitized)")
PY
