# DreamTrans Deployment Guide

The supported production path is Docker Compose on Linux. WSL is supported for
development and CI-equivalent testing; native Windows execution is not a
deployment target. Compose runs PostgreSQL, applies all pending migrations,
starts DreamTrans only after the schema is ready, and keeps the application
port bound to loopback by default.

## Requirements

- A Linux host with Docker Engine and Docker Compose v2
- A Speechmatics API key
- An OpenAI-compatible API key only if translation, AI chat/artifacts, or
  semantic indexing is needed; project management and lexical retrieval do not
  require it

For source development, use Go 1.26.5 and Node.js 24.18.0 LTS. The Go module
declares its toolchain, so a compatible Go installation can download the exact
compiler automatically.

## One-click installation

```bash
curl -fsSL \
  https://raw.githubusercontent.com/CoYumeLabs/DreamTrans/main/scripts/install.sh |
  bash
```

The installer writes a permission-restricted `.env`, generates independent
database/access-token/refresh-token secrets, creates the initial administrator,
and waits for `/readyz` before reporting success. Migration files are extracted
from the exact pulled application image rather than from a mutable Git branch.
If a stale GHCR login rejects the public application image, the installer
retries that pull with a temporary anonymous Docker configuration. It never
logs out or rewrites the operator's existing Docker credentials.

Useful commands:

```bash
# Update the existing tag.
curl -fsSL https://raw.githubusercontent.com/CoYumeLabs/DreamTrans/main/scripts/install.sh |
  bash -s -- --update

# Switch to a specific release tag and update.
curl -fsSL https://raw.githubusercontent.com/CoYumeLabs/DreamTrans/main/scripts/install.sh |
  bash -s -- --update --tag 1.2.3

# Status and logs.
curl -fsSL https://raw.githubusercontent.com/CoYumeLabs/DreamTrans/main/scripts/install.sh |
  bash -s -- --status
curl -fsSL https://raw.githubusercontent.com/CoYumeLabs/DreamTrans/main/scripts/install.sh |
  bash -s -- --logs
```

## Repository-based Compose deployment

Check out the same release as the application image tag. This keeps the
repository migration bundle and image schema expectations aligned.

```bash
git clone https://github.com/CoYumeLabs/DreamTrans.git
cd DreamTrans
git checkout v1.2.3

cp backend/.env.example .env
chmod 600 .env
```

Fill in at least:

```dotenv
IMAGE_TAG=1.2.3
SM_API_KEY=...
POSTGRES_PASSWORD=...
JWT_SECRET=...
JWT_REFRESH_SECRET=...
ADMIN_EMAIL=admin@example.com
ADMIN_PASSWORD=...
```

Use different random values for all three secrets. `ADMIN_EMAIL` and
`ADMIN_PASSWORD` are needed on a fresh database unless self-registration is
deliberately enabled.

```bash
docker compose up -d
docker compose ps
curl --fail http://127.0.0.1:16002/healthz
curl --fail http://127.0.0.1:16002/readyz
```

`/healthz` is a process liveness check. `/readyz` also verifies PostgreSQL when
database mode is configured. Neither endpoint calls Speechmatics or OpenAI.

To update, first back up PostgreSQL, then move the checkout and image tag to
the same release:

```bash
git fetch --tags
git checkout v1.2.4
# Set IMAGE_TAG=1.2.4 in .env.
docker compose pull
docker compose up -d
```

Migrations are transactional and recorded in `schema_migrations`. Back up the
PostgreSQL database before every production upgrade.

## Migration 023: USD wallet and membership billing

Migration 023 replaces DreamPoints with a USD wallet. It is not reversible:

- `users.dreampoints` / `dreampoints_used` are dropped after every balance is
  copied 1:1 into `billing_accounts.wallet_usd` (DreamPoints were pegged to
  USD, so no amounts change).
- `pricing_rules`, `tenant_api_usage` and `tenant_storage_usage` are dropped;
  `billing_config` loses `dp_per_usd`, `pending_config` and `pricing_state`.
  Retail prices are now derived at charge time from provider cost × markup ×
  (1 − plan discount) and frozen into each usage row.
- `usage_logs.cost` is renamed to `charge_usd` and `service_fee_dp` to
  `margin_usd`; new columns record which bucket paid (`grant_usd`,
  `wallet_usd`).
- New tables: `plans` (seeded with `free` and `pro`), `billing_accounts`,
  `grants`, `memberships`, `payments`, `stripe_events`, `topup_tiers`.
- Transcript storage is counted per billing account and limited by the plan.

Back up the database before upgrading. Stop the old application before the
migration runs, as with any release. After the upgrade, review the plans,
top-up tiers, markup and trial credit in `/pro/admin`, then configure
`STRIPE_SECRET_KEY`, `STRIPE_WEBHOOK_SECRET` and `APP_BASE_URL` (see
`docs/ENVIRONMENT_VARIABLES.md`) to enable online payments. Without Stripe the
system keeps working: administrators can grant credit and adjust wallets by
hand.

## PostgreSQL 16 and migration 019

Both the database and migration services are pinned to:

```text
pgvector/pgvector:0.8.2-pg16-bookworm
```

This keeps the PostgreSQL major version at 16 while making the `vector` and
`pg_trgm` extensions available to migration 019. Keep the exact image pinned;
do not replace it with `postgres:16`, `latest`, an Alpine variant, or a
different PostgreSQL major version for an existing volume.

Before upgrading a production installation to a release containing
`019_ai_knowledge_production.sql`, create a logical backup while the current
database is healthy:

```bash
mkdir -p backups
backup_path="backups/dreamtrans-before-019-$(date -u +%Y%m%dT%H%M%SZ).dump"
docker compose exec -T postgres sh -c \
  'pg_dump -U "$POSTGRES_USER" -d "$POSTGRES_DB" --format=custom' \
  > "$backup_path"
test -s "$backup_path"
```

Also retain the matching `.env`, application image tag, and migration bundle.
For high-value deployments, validate the dump with `pg_restore --list` and
perform a restore rehearsal in an isolated PG16 pgvector instance.

Migration 019 performs its metadata reconciliation and builds the initial
HNSW/trigram indexes in one transaction. On an installation with substantial
existing knowledge this can hold write locks for a long time and generate
significant temporary disk and WAL usage. Rehearse the migration against a
recent copy, verify free database/WAL space, schedule a maintenance window,
and stop the old `dreamtrans` application/workers before starting `migrate`.
Do not allow the old application to continue accepting writes during this
migration.

Then deploy the matching application and migration images:

```bash
git fetch --tags
git checkout v1.2.4
# Set IMAGE_TAG=1.2.4 in .env.
docker compose pull postgres migrate dreamtrans
docker compose stop dreamtrans
docker compose up -d postgres
docker compose run --rm migrate
docker compose up -d dreamtrans
docker compose ps
curl --fail http://127.0.0.1:16002/readyz
```

Migration 019 enables `vector` and `pg_trgm`, adds 1536-dimensional vector
columns, HNSW/trigram indexes, durable extraction/index jobs, and storage
accounting fields. Existing knowledge is reconciled and left `unindexed`;
the migration never sends it to an embedding provider and never creates a
paid backfill. Semantic vectors are created only after a user sees the index
preview and explicitly confirms the job.

Changing `OPENAI_EMBEDDING_MODEL` marks incompatible indexes `stale`. Rebuilding
them is another explicit, billable confirmation; do not script an automatic
backfill during deployment.

Migration 019 has no down migration. Do not roll back by deleting its
`schema_migrations` row, dropping columns, or starting the volume with a plain
PostgreSQL image: vector columns depend on the pgvector extension files. A full
rollback requires stopping writes, restoring the pre-upgrade dump into a fresh
PG16 pgvector database/volume, and deploying the matching older application.
Even when temporarily running an older application against the upgraded
schema, keep the pinned pgvector PG16 image.

## Database image changes and index integrity

B-tree indexes over text columns are ordered by the collation libraries of the
operating system inside the database image. Reusing an existing volume with an
image built on a different Debian/glibc release — even at the same PostgreSQL
major version — can silently corrupt those indexes: unique indexes stop
detecting duplicates (`ON CONFLICT` upserts start failing with spurious
`duplicate key` errors) and index scans can miss committed rows while the
underlying table data stays intact. This has happened on a production
installation; the application cannot repair it because the database lies to it.

When changing the pinned database image in any way beyond a same-base patch
release, prefer a logical dump into a fresh volume. If the existing volume must
be reused, immediately rebuild every index and verify the result:

```bash
docker compose exec -T postgres sh -c \
  'psql -U "$POSTGRES_USER" -d "$POSTGRES_DB"' <<'SQL'
REINDEX DATABASE CONCURRENTLY;
CREATE EXTENSION IF NOT EXISTS amcheck;
DO $$
DECLARE r RECORD;
BEGIN
  FOR r IN
    SELECT c.relname AS idx, i.indexrelid
    FROM pg_index i
    JOIN pg_class c ON c.oid = i.indexrelid
    JOIN pg_am a ON a.oid = c.relam
    WHERE a.amname = 'btree' AND i.indisvalid
  LOOP
    BEGIN
      PERFORM bt_index_check(r.indexrelid, true);
    EXCEPTION WHEN OTHERS THEN
      RAISE WARNING 'CORRUPT: % -> %', r.idx, SQLERRM;
    END;
  END LOOP;
  RAISE NOTICE 'index sweep complete';
END $$;
SQL
```

`REINDEX ... CONCURRENTLY` avoids blocking writes but fails on unique indexes
whose table already contains duplicates let in by the corruption. In that case
resolve the reported duplicates first (keep the most recently updated row),
then rerun the plain `REINDEX DATABASE` during a short maintenance window.

## Data storage, retention, and deletion

The `postgres_data` volume contains accounts, cloud transcripts, extracted
knowledge text, chunks, vectors, index jobs, AI artifacts, and idempotency
records. The `dreamtrans_data` volume contains uploaded knowledge source files
under `/app/data/knowledge` and legacy SQLite RAG data. Back up both when these
features are in use.

Tenant storage quota accounts for original knowledge files, extracted text,
legacy and semantic vectors, session AI chunks, and saved AI artifacts in
addition to cloud transcripts. Deleting a knowledge source or project removes
its database rows and the application attempts to remove the corresponding
file from `dreamtrans_data`. Inspect application logs for an orphaned-file
warning after a failed filesystem deletion. Deleting an artifact releases its
primary stored-content quota; unlinking a project and session deletes only the
relationship.

Application deletion does not erase existing backups, volume snapshots,
reverse-proxy logs, or data retained by upstream providers. Define independent
retention and destruction schedules for each of those systems. Completed chat
responses in `ai_generation_requests` are retained for at most 24 hours for
idempotent retries, are pruned after expiry, and are deleted through their
session foreign key when the session is deleted. Successful artifact generation
releases its temporary generation reservation; the artifact exists only in
`ai_artifacts`, so deleting it removes the persisted content and releases its
quota without leaving a response copy in the generation cache.

## Network exposure and TLS

Compose defaults to:

```dotenv
BIND_ADDRESS=127.0.0.1
PORT=16002
```

Keep that loopback binding when an Nginx, Caddy, or other same-host reverse
proxy terminates HTTPS. If direct network publication is intentional, set
`BIND_ADDRESS=0.0.0.0` and enforce TLS/firewall controls externally.

Proxy `/`, `/api/`, and `/ws/` to the same DreamTrans origin. WebSocket proxying
must preserve the browser-visible `Host` and the `Upgrade`, `Connection`, and
`Sec-WebSocket-Protocol` headers. Do not rewrite the `/ws/` path.

For the normal signed-in deployment, `CORS_ALLOWED_ORIGINS` can remain empty:
DreamTrans validates the explicit WebSocket JWT before its origin policy, so an
internal `Host` rewrite by a reverse proxy no longer breaks authenticated
connections. Preserving the browser-visible `Host` is still recommended.

Anonymous or service-key browser WebSockets do not receive that JWT exception.
They must remain same-origin, or their complete frontend origins must be listed
in `CORS_ALLOWED_ORIGINS` (for example, `https://app.example.com`). A genuinely
cross-origin frontend also needs the same CORS configuration for its HTTP API
requests. Browser microphone access requires HTTPS outside localhost.

Anonymous provider access and self-registration are disabled by default. Do
not expose `ALLOW_ANONYMOUS_API=true` outside a trusted loopback development
environment.

## Updating a deployment restored from backup

Restored installer deployments can use the standard command without removing
their production overrides:

```bash
curl -fsSL https://raw.githubusercontent.com/soaringjerry/DreamTrans/main/scripts/install.sh | \
  sudo bash -s -- --update --dir /root/dreamtrans --tag latest
```

Updates require Python 3 and Docker Compose v2 with `config --environment` and
`config --format json`. The installer reads the effective `COMPOSE_FILE` chain
and backs up `.env`, the base file, all selected overrides, migration assets and
the existing `backup.sh` before changing them. Selected Compose files must be
regular, installer-user-owned files directly inside the installation directory.

For `dreamtrans-migration/*` recovery images, the installer changes only the
`app`, `db` and `migrate` image/pull-policy fields in their existing YAML service
blocks: the app uses `ghcr.io/coyumelabs/dreamtrans:${IMAGE_TAG:-latest}`, and both
database services use the pinned PostgreSQL 16/pgvector release. `never` becomes
`missing`; the updater explicitly pulls before changing containers. Unsupported
YAML layouts fail validation and restore the configuration. The override chain,
production volume declarations, CORS and other override settings remain in
place. A `start-production.sh` that explicitly selects those same files with
`-f` therefore uses the new release too. Subsequent updates use the same command.

Before replacing containers or changing permissions, the final merged Compose
volume names must match the existing app's `/app/data` and database's
`/var/lib/postgresql/data` mounts. Existing external local volumes do not require
Compose ownership labels. Managed volumes still require matching labels; bind
mounts, volume subpaths, driver mount options and mismatched targets are refused.
Keep the restored production containers present for this first update. No data
volume is deleted or replaced. Existing backup cron entries and unrelated
applications are not removed; the backup helper keeps the same path and Compose
configuration.

On failure, the installer restores configuration and the previous local images
without pulling or rerunning dependency migrations. **This is not a database
rollback:** each committed migration, including any before a later failed
migration, remains applied. After migration was attempted, the private
`.update-backup.*` configuration directory is retained for recovery (it contains
secrets, not a database dump). Restarting the previous app requires schema
compatibility; if it fails readiness, the installer stops it and reports manual
forward recovery is needed. Even a healthy old app does not prove all schema
changes are backward compatible. Do not overwrite new production writes with
an older database dump to undo an image update.

## Local source development

Backend-only Classic UI development:

```bash
cd backend
SM_API_KEY=... ALLOW_ANONYMOUS_API=true go run ./cmd/web
```

Keep this anonymous mode on localhost. For frontend development:

```bash
cd frontend
npm ci
VITE_BACKEND_URL=http://127.0.0.1:8080 \
VITE_BACKEND_WS_URL=ws://127.0.0.1:8080 \
npm run dev
```

Production frontend assets are served by the Go application; a separate PM2 or
frontend container is not required.

## Troubleshooting

```bash
docker compose ps
docker compose logs migrate
docker compose logs dreamtrans
docker inspect --format '{{.State.Health.Status}}' dreamtrans
```

- A failed migration is rolled back and prevents the application from starting.
- `401` from provider-backed endpoints means JWT/service-key authentication is
  working as configured; it is not a health-check failure.
- If `/healthz` succeeds but `/readyz` returns `503`, inspect PostgreSQL and
  migration logs.
- The server allows up to 20 seconds to drain active requests and WebSockets;
  Compose grants a 30-second stop window.
