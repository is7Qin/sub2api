# Sub2API

Sub2API is an independently maintained AI API gateway for routing, managing, and operating multiple AI upstream accounts behind OpenAI-compatible and Anthropic-compatible APIs.

This branch keeps the deployable name `sub2api` for compatibility, but its releases are owned by this repository and start from `v1.0.0`.

## Independent Maintenance

This repository is maintained independently from upstream `Wei-Shaw/sub2api`.

- We do not automatically rebase or merge upstream `main`.
- Upstream pull requests are reviewed individually before being accepted.
- Local releases use independent SemVer tags starting at `v1.0.0`.
- Existing binary, service, and image basenames remain `sub2api` so current deployments do not need a rename.

## Relationship to Upstream

Sub2API originated from `Wei-Shaw/sub2api`, but this branch is no longer an upstream-tracking branch. Upstream changes are treated as review candidates. A change is merged only when it matches this branch's architecture, operating assumptions, and user-facing behavior.

## Core Capabilities

- Multi-upstream AI API gateway for subscription and API-key based accounts.
- OpenAI-compatible, Anthropic-compatible, and Claude Code oriented request handling.
- Account selection, failover, concurrency control, and rate-limit cooldown handling.
- Web administration UI for accounts, groups, keys, models, and operational settings.
- Docker Compose and binary/systemd deployment paths.
- Release artifacts built under the compatible `sub2api` name.

## Installation

### Docker Compose

The canonical Compose stack runs Sub2API with managed PostgreSQL 18 and Redis 8 using named volumes. Docker Engine with the Compose v2 plugin is required.

From a repository checkout:

```bash
git clone https://github.com/is7Qin/sub2api.git
cd sub2api/deploy
cp .env.example .env
# Set DATABASE_PASSWORD and TOTP_ENCRYPTION_KEY; review the remaining settings.
docker compose up -d
```

To prepare a separate deployment directory with generated secrets:

```bash
curl -sSL https://raw.githubusercontent.com/is7Qin/sub2api/main/deploy/docker-deploy.sh -o docker-deploy.sh
chmod +x docker-deploy.sh
./docker-deploy.sh --destination sub2api-deploy --ref v1.2.3
cd sub2api-deploy
docker compose up -d
```

Replace the example Git release tag and pin the corresponding non-`v` image tag in `SUB2API_VERSION` (for example, Git ref `v1.2.3` uses image tag `1.2.3`). The bootstrap requires an absent destination, uses no-clobber publication with rollback, never overwrites an existing `.env`, and never starts containers. A new directory is a fresh Compose project unless the prior project identity and data are deliberately preserved; see `deploy/README.md` before migrating an installation.

See [`deploy/README.md`](deploy/README.md) for named-volume defaults, explicit bind/development/external modes, backup and update guidance, and PostgreSQL major-version migration warnings.

### Binary Install on Linux

```bash
curl -sSL https://raw.githubusercontent.com/is7Qin/sub2api/main/deploy/install.sh | sudo bash
```

This Linux-only installer downloads release assets from `is7Qin/sub2api` and installs the `sub2api` binary and systemd service. It rejects non-Linux systems before privilege checks, downloads, or lifecycle work.

### Manual Download

Download archives from:

```text
https://github.com/is7Qin/sub2api/releases
```

Choose the archive matching your OS and CPU architecture, then follow the deployment notes in `deploy/README.md`.

## Configuration

Start from the canonical deployment examples under `deploy/`:

- `deploy/compose.yaml` for the default managed stack with named volumes.
- `deploy/compose.bind.yaml` as an explicit bind-storage override.
- `deploy/compose.dev.yaml` as an explicit source-build/debug override; it requires Compose v2.24.4+.
- `deploy/compose.external.yaml` when PostgreSQL and Redis are provided separately.
- `deploy/.env.example` for Docker Compose and application environment variables.
- `deploy/config.example.yaml` for binary/systemd configuration.

For Docker deployments, copy `.env.example` to `.env` and set required secrets before starting. Do not print or publish the rendered output of `docker compose config`, because it contains credentials.

## Upgrade and Release Policy

- First independent release: `v1.0.0`.
- Patch releases (`v1.0.x`) contain small fixes, documentation updates, and reviewed upstream PR cherry-picks.
- Minor releases (`v1.x.0`) contain new features or notable behavior changes.
- Major releases (`v2.0.0+`) are reserved for breaking configuration, deployment, API, or data-model changes.

The runtime `VERSION` value is generated from the tag by the release workflow. For example, tag `v1.0.0` produces runtime version `1.0.0`.

Back up PostgreSQL and `/app/data` before deployment updates. Never use `docker compose down -v` as a routine stop command: it deletes named-volume data. The canonical stack uses PostgreSQL 18; PostgreSQL 15, 16, or 17 data requires dump/restore or `pg_upgrade`, never a direct mount into PostgreSQL 18.

## Upstream PR Review Policy

When an upstream PR is considered, review includes:

1. Intent and user impact.
2. Implementation risk and architectural fit.
3. Tests and operational behavior.
4. Interaction with local changes.
5. Known edge cases and accepted limitations.

PRs are merged or cherry-picked only after explicit approval for that PR.

## Disclaimer

This is an independently maintained branch. It is not an official upstream release and is not endorsed by upstream maintainers. Use it according to your own operational and compliance requirements.
