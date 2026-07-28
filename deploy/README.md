# Sub2API Deployment Files

This directory contains the supported Docker Compose and binary/systemd deployment inputs. Docker Compose is the recommended all-in-one path. Binary installation remains available on Linux through `install.sh`, which renders the shipped `sub2api.service` template. Its full installer, systemd, signal, symlink, and lifecycle contract is Linux-only and runs authoritatively in the `ubuntu-latest` deployment CI job; non-Linux hosts perform only static wiring and Bash syntax checks.

## Docker Compose prerequisites

- Docker Engine with the **Compose v2** plugin (`docker compose`, not `docker-compose`).
- OpenSSL when using `docker-deploy.sh` to generate secrets.
- The development override requires Docker Compose **v2.24.4 or newer** because it uses `!override`. CI uses v2.24.7 as a tested pin; operators may use any compatible newer v2 release and are not required to install exactly v2.24.7.

## Default deployment: managed services and named volumes

The canonical stack is `compose.yaml`. It runs Sub2API with PostgreSQL 18 and Redis 8. Its default persistence is in Docker named volumes:

- `sub2api_data` at `/app/data`
- `postgres_data` at `/var/lib/postgresql/data`
- `redis_data` at `/data`

From a repository checkout:

```bash
cd deploy
cp .env.example .env
# Set DATABASE_PASSWORD and TOTP_ENCRYPTION_KEY; set JWT_SECRET if you manage it externally.
# Example secret generator: openssl rand -hex 32
docker compose up -d
```

After a deployment directory has `compose.yaml` and `.env`, routine commands are plain commands from that directory:

```bash
docker compose ps
docker compose logs -f sub2api
docker compose restart sub2api
docker compose down
```

Do not run `docker compose config` into a terminal or support log: the rendered output contains credentials. The repository contract test uses isolated non-secret fixtures and redirects rendered configuration to temporary files.

### Bootstrap a separate deployment directory

The public bootstrap URL remains stable. It downloads the canonical `compose.yaml` and `.env.example` from one repository ref, generates `DATABASE_PASSWORD`, `JWT_SECRET`, and `TOTP_ENCRYPTION_KEY`, and creates a private `.env` without printing secret values.

```bash
curl -sSL https://raw.githubusercontent.com/is7Qin/sub2api/main/deploy/docker-deploy.sh -o docker-deploy.sh
chmod +x docker-deploy.sh
./docker-deploy.sh --destination sub2api-deploy --ref v1.2.3
cd sub2api-deploy
docker compose up -d
```

Replace `v1.2.3` with the Git release you intend to deploy. If `--ref` is omitted, the script uses `main`; release tag pinning is recommended for production so bootstrap files and the selected application version can be reviewed together. Image tags do not include the Git tag's leading `v`, so Git ref `v1.2.3` corresponds to `SUB2API_VERSION=1.2.3` in `.env`.

The destination must not exist, even as an empty directory or symlink. The script prepares all three files beside the destination, atomically claims the absent directory name, and uses same-filesystem no-clobber links with ownership-checked rollback; failures and handled interruptions therefore leave no partial deployment. The script never merges into an existing deployment, never overwrites `.env`, never creates or alters runtime data directories, and never starts containers. Choose a new, absent path when an installation already exists.

Bootstrap into a new directory is a **fresh Compose project** unless you deliberately preserve the old project identity and migrate or restore its data. Do not start the new directory against an existing installation until you have followed the named-volume or bind-directory migration guidance below.

Environment equivalents are available for automation: `SUB2API_DEPLOY_DIR`, `SUB2API_REF`, and `SUB2API_RAW_BASE_URL`.

## Explicit deployment modes

These modes are opt-in. They do not replace the named-volume default.

### Bind-mounted storage

Use host directories `./data`, `./postgres_data`, and `./redis_data`:

```bash
docker compose -f compose.yaml -f compose.bind.yaml up -d
```

Create and secure the directories according to your host's container UID/GID and backup policy. Keep using the same `-f` arguments for subsequent commands in this mode.

### Development source build

Build the repository-root `Dockerfile`, enable debug mode, and publish only on loopback:

```bash
docker compose -f compose.yaml -f compose.dev.yaml up -d
```

For development with bind-mounted storage:

```bash
docker compose -f compose.yaml -f compose.bind.yaml -f compose.dev.yaml up -d
```

These development commands require Compose v2.24.4+ for `!override`.

### External PostgreSQL and Redis

`compose.external.yaml` runs only Sub2API. Set external `DATABASE_HOST`, `DATABASE_PORT`, `DATABASE_USER`, `DATABASE_PASSWORD`, `DATABASE_DBNAME`, `REDIS_HOST`, `REDIS_PORT`, and optional `REDIS_PASSWORD` in `.env`. Leave `REDIS_USERNAME` empty to authenticate as the Redis default user; set it only for an externally managed named ACL user. Then run:

```bash
docker compose -f compose.external.yaml up -d
```

The application still persists `/app/data` in the `sub2api_data` named volume. External database and Redis backup, upgrade, and availability are the operator's responsibility.

## Initial setup and credentials

`AUTO_SETUP=true` is fixed by the canonical Compose topology. On first application startup it applies migrations, writes application state under `/app/data`, and creates the initial administrator. If `ADMIN_PASSWORD` is blank, inspect application logs for the generated password:

```bash
docker compose logs sub2api | grep "admin password"
```

`DATABASE_PASSWORD` and `TOTP_ENCRYPTION_KEY` are required by the Compose contract. A fixed TOTP key is essential: changing it invalidates existing encrypted TOTP data. A blank `JWT_SECRET` can be generated by initial auto-setup and persisted in `/app/data/config.yaml`, but production operators may set and manage a stable value explicitly.

PostgreSQL's `POSTGRES_USER`, `POSTGRES_PASSWORD`, and `POSTGRES_DB` are **initialization variables**, not live credential-management controls. The PostgreSQL image reads them only when initializing an empty data directory. Editing `DATABASE_*` in `.env` later does not rename a populated database or rotate its database password; perform those changes in PostgreSQL and coordinate application settings separately.

## Compose project identity and data migration

The names `sub2api_data`, `postgres_data`, and `redis_data` in `compose.yaml` are logical volume keys. Docker's actual volume names are scoped by the Compose project, normally derived from the deployment directory name (for example, `sub2api-deploy_postgres_data`). Setting `COMPOSE_PROJECT_NAME`, using `docker compose -p`, moving or renaming the directory, or bootstrapping into a differently named directory can select a different project and cause Compose to create fresh volumes. An empty application after such a change often means the old volumes are detached, not deleted.

Before moving or converting an installation:

1. From the old directory, record the project name with `docker compose ls` and record the exact mounts/volume names with `docker compose config --volumes`, `docker volume ls`, and `docker volume inspect <volume>`. Treat rendered configuration as sensitive and do not publish it.
2. Back up PostgreSQL with a database-aware dump and back up `/app/data`; include Redis if required by the recovery plan. A named volume is not safely migrated by copying its host-internal directory while containers are running.
3. To reuse the canonical named volumes in a new directory, preserve the same project identity by setting the recorded `COMPOSE_PROJECT_NAME` (or consistently using `docker compose -p <old-project>`) before the first `up`. Inspect the rendered volume names before starting. Otherwise restore the backups into newly created volumes.
4. Bind-storage installations are different: stop the old stack, preserve ownership and permissions, copy `data`, `postgres_data`, and `redis_data` with an appropriate host backup tool, and continue using `compose.bind.yaml`. Do not treat bind directories as Compose named volumes.
5. Keep the old directory, project-name record, volume-name record, and backups until verification succeeds. Rollback must use the original Compose project identity and original volumes or restore the backup; merely changing back to an old directory name is not a complete rollback plan.

Never run `docker compose down -v` during migration or rollback: it deletes the project's named volumes. A bootstrap into a new directory is a fresh project unless project identity and data migration are explicitly preserved.

## Updates, backups, and major database upgrades

1. Back up PostgreSQL and `/app/data` before every update. Include Redis persistence if your recovery plan depends on it.
2. Prefer a release tag in `SUB2API_VERSION` instead of `latest`, and review release notes plus Compose changes before changing the pin.
3. Pull and recreate without deleting volumes:

   ```bash
   docker compose pull
   docker compose up -d
   ```

4. Verify health and logs before retiring the backup.

**Never use `docker compose down -v` for a normal stop or update.** `-v` deletes the named volumes and therefore the managed PostgreSQL database, Redis data, and `/app/data` application state.

The canonical stack runs PostgreSQL 18. A PostgreSQL 15, 16, or 17 data directory is not binary-compatible with PostgreSQL 18. **Never mount an older major-version data directory directly into the PostgreSQL 18 container.** Upgrade with a tested logical dump/restore or `pg_upgrade` procedure and retain a backup and rollback path. The same rule applies whether the source is a named volume or bind directory.

Application schema migrations are forward-only and tracked in `schema_migrations`; rollback requires restoring a database backup or applying a reviewed compensating migration.

## File map

| File | Purpose |
|------|---------|
| `compose.yaml` | Canonical managed production stack with named volumes |
| `compose.bind.yaml` | Bind-storage override |
| `compose.dev.yaml` | Development source-build/debug override |
| `compose.external.yaml` | App-only topology for external PostgreSQL and Redis |
| `.env.example` | Compose and application environment template |
| `docker-deploy.sh` | Safe bootstrap for a new isolated Compose directory |
| `test-compose-config.sh` | Static rendered Compose contract test |
| `test-docker-deploy.sh` | Isolated bootstrap behavior test |
| `test-install-contract.sh` | Network-free binary installer/systemd contract test |
| `DOCKER.md` | Published container image documentation |
| `install.sh` | Binary/systemd installer |
| `sub2api.service` | Systemd unit template consumed by `install.sh` |
| `config.example.yaml` | Binary/systemd configuration example |

Legacy `docker-compose*.yml` files remain for one compatibility release. They are frozen historical definitions, not wrappers or the primary deployment path, because redirecting an old command to the new topology could unexpectedly change volumes or PostgreSQL ownership. New installs and the canonical bootstrap use `compose.yaml` and the compact `compose.*.yaml` variants only.

## Binary/systemd installation

The binary installer is a Linux-only systemd manager and rejects every non-Linux kernel before privilege checks, downloads, or lifecycle work:

```bash
curl -sSL https://raw.githubusercontent.com/is7Qin/sub2api/main/deploy/install.sh | sudo bash
```

The release archive ships `sub2api.service`; `install.sh` renders that template with the selected user, install directory, host, and port. `SERVER_HOST` deliberately accepts only IPv4 addresses, DNS hostnames, or unbracketed pure-hex IPv6 literals; IPv4-embedded IPv6 forms are not supported. `SERVICE_USER` accepts a bounded Linux account-name grammar, and `INSTALL_DIR` must be a normalized absolute path with portable components; whitespace, control characters, unit specifiers, quotes, backslashes, and dot segments are rejected. The unit waits for `network-online.target` but does not require local PostgreSQL or Redis units, so externally managed services remain supported. See `config.example.yaml` for the file-based configuration shape.
