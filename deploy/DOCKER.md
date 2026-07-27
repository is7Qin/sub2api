# Sub2API Docker Image

Sub2API is an independently maintained AI API gateway. The published image basename remains `sub2api` for deployment compatibility.

## Recommended deployment

Use the repository's canonical Compose files rather than rebuilding the managed topology from this image reference:

```bash
git clone https://github.com/is7Qin/sub2api.git
cd sub2api/deploy
cp .env.example .env
# Set DATABASE_PASSWORD and TOTP_ENCRYPTION_KEY, then review the remaining settings.
docker compose up -d
```

The canonical stack runs the application with PostgreSQL 18 and Redis 8. It persists application configuration and runtime state at `/app/data` and persists the managed dependencies in named volumes. See [`deploy/README.md`](https://github.com/is7Qin/sub2api/blob/main/deploy/README.md) for bootstrap, bind-storage, development, external-service, backup, and PostgreSQL major-upgrade instructions.

For production, pin `SUB2API_VERSION` in `.env` to a reviewed release tag instead of relying on `latest`.

## Direct image contract

When running the image outside the canonical managed Compose stack, provide PostgreSQL and Redis separately and persist `/app/data`. The application reads Viper-style uppercase environment variables such as:

```bash
docker run -d \
  --name sub2api \
  -p 8080:8080 \
  -v sub2api_data:/app/data \
  -e AUTO_SETUP=true \
  -e SERVER_HOST=0.0.0.0 \
  -e SERVER_PORT=8080 \
  -e DATABASE_HOST=postgres.example.internal \
  -e DATABASE_PORT=5432 \
  -e DATABASE_USER=sub2api \
  -e DATABASE_PASSWORD='set-a-secret-value' \
  -e DATABASE_DBNAME=sub2api \
  -e DATABASE_SSLMODE=require \
  -e REDIS_HOST=redis.example.internal \
  -e REDIS_PORT=6379 \
  -e REDIS_PASSWORD='set-if-required' \
  -e REDIS_DB=0 \
  -e TOTP_ENCRYPTION_KEY='set-a-stable-random-value' \
  ghcr.io/is7qin/sub2api:1.2.3
```

Replace the example tag and secret values. Do not place real credentials in shell history; use your platform's secret injection mechanism where possible.

`AUTO_SETUP=true` enables first-run database migration and administrator setup. It does not provision external PostgreSQL or Redis. If `ADMIN_PASSWORD` is blank, the generated initial password is written to application logs.

## Core environment variables

### Server and setup

| Variable | Required | Typical value | Description |
|----------|----------|---------------|-------------|
| `AUTO_SETUP` | For unattended first run | `true` | Runs migrations and initial setup automatically |
| `SERVER_HOST` | No | `0.0.0.0` | Address listened on inside the container |
| `SERVER_PORT` | No | `8080` | Port listened on inside the container |
| `SERVER_MODE` | No | `release` | Application mode (`debug` or `release`) |
| `ADMIN_EMAIL` | No | `admin@sub2api.local` | Initial administrator email |
| `ADMIN_PASSWORD` | No | blank | Initial password; blank allows first-run generation |

Publish the host port with Docker's `-p` option. `SERVER_PORT` controls the process inside the container; it is not a replacement for host port publication.

### PostgreSQL

| Variable | Required | Typical value | Description |
|----------|----------|---------------|-------------|
| `DATABASE_HOST` | Yes | PostgreSQL host | Database host name or address |
| `DATABASE_PORT` | No | `5432` | Database port |
| `DATABASE_USER` | Yes | `sub2api` | Database user |
| `DATABASE_PASSWORD` | Yes | secret | Database password |
| `DATABASE_DBNAME` | Yes | `sub2api` | Database name |
| `DATABASE_SSLMODE` | No | `prefer` | `disable`, `prefer`, `require`, `verify-ca`, or `verify-full` |

The canonical Compose stack maps the same `DATABASE_*` identity values to PostgreSQL's one-shot `POSTGRES_*` initialization variables. Once a PostgreSQL data volume is initialized, changing `.env` does not rotate or rename the populated database.

### Redis

| Variable | Required | Typical value | Description |
|----------|----------|---------------|-------------|
| `REDIS_HOST` | Yes | Redis host | Redis host name or address |
| `REDIS_PORT` | No | `6379` | Redis port |
| `REDIS_PASSWORD` | No | blank | Password; blank supports unauthenticated Redis |
| `REDIS_DB` | No | `0` | Logical Redis database |
| `REDIS_ENABLE_TLS` | No | `false` | Enables TLS for the application connection |

### Persistent application secrets and state

| Variable/path | Requirement | Description |
|---------------|-------------|-------------|
| `TOTP_ENCRYPTION_KEY` | Stable value required | Protects TOTP and other encrypted application data; changing it invalidates existing data |
| `JWT_SECRET` | Stable value recommended | May be generated during auto-setup and persisted under `/app/data`; set explicitly when managed externally |
| `/app/data` | **Must be persisted** | Stores generated configuration, logs, and runtime data required across container replacement |

The complete supported environment template is [`deploy/.env.example`](https://github.com/is7Qin/sub2api/blob/main/deploy/.env.example).

## Managed dependency versions and upgrades

The canonical Compose deployment uses:

- PostgreSQL `18-alpine`
- Redis `8-alpine`

Back up PostgreSQL and `/app/data` before updates. PostgreSQL 15, 16, or 17 data directories must not be mounted directly into PostgreSQL 18. Use a tested dump/restore or `pg_upgrade` process for a major-version upgrade. Never use `docker compose down -v` during routine operations because it deletes named-volume data.

## Supported architectures

- `linux/amd64`
- `linux/arm64`

## Tags

- `latest` — latest stable release for this repository
- `x.y.z` — specific release; recommended for production pinning
- `x.y` — latest patch of a minor line, when published
- `x` — latest minor of a major line, when published

## Links

- [GitHub repository](https://github.com/is7Qin/sub2api)
- [Deployment guide](https://github.com/is7Qin/sub2api/blob/main/deploy/README.md)
