#!/usr/bin/env bash
set -euo pipefail
source /tmp/dt-config-ci-results/db.env
cd /tmp/dt-config-read-contention
export MIGRATIONS_DIR="$PWD/backend/migrations"
bash scripts/tests/migration_bundle_test.sh > /tmp/dt-config-ci-results/migration-bundle.log 2>&1
bash scripts/migrate.sh > /tmp/dt-config-ci-results/migrations.log 2>&1
psql -v ON_ERROR_STOP=1 > /tmp/dt-config-ci-results/schema.log <<'SQL'
DO $verify$
DECLARE pk_columns text;
BEGIN
 SELECT string_agg(attribute.attname, ',' ORDER BY key_column.ordinality)
 INTO pk_columns
 FROM pg_constraint AS constraint_record
 CROSS JOIN LATERAL unnest(constraint_record.conkey) WITH ORDINALITY AS key_column(attnum, ordinality)
 JOIN pg_attribute AS attribute ON attribute.attrelid=constraint_record.conrelid AND attribute.attnum=key_column.attnum
 WHERE constraint_record.conrelid='provider_models'::regclass AND constraint_record.contype='p';
 IF pk_columns IS DISTINCT FROM 'provider,model_id' THEN
  RAISE EXCEPTION 'provider_models key unexpected: %', pk_columns;
 END IF;
END
$verify$;
SQL
for script in provider_models_migration_test model_catalog_constraints_migration_test acquisition_migration_test; do
 bash "scripts/tests/$script.sh" > "/tmp/dt-config-ci-results/$script.log" 2>&1
done
cd backend
go version > /tmp/dt-config-ci-results/go-version.log
unformatted="$(gofmt -l .)"
if [[ -n "$unformatted" ]]; then printf '%s\n' "$unformatted"; exit 1; fi
go mod tidy -diff > /tmp/dt-config-ci-results/deps.log 2>&1
go mod download >> /tmp/dt-config-ci-results/deps.log 2>&1
go mod verify >> /tmp/dt-config-ci-results/deps.log 2>&1
/tmp/dreamtrans-balance-ci-tools/golangci-lint run --timeout=5m --build-tags=event_worker > /tmp/dt-config-ci-results/lint.log 2>&1
go test -v -race -coverprofile=/tmp/dt-config-ci-results/coverage.txt ./... > /tmp/dt-config-ci-results/backend.log 2>&1
go test -v -race -tags=event_worker ./cmd/event-worker > /tmp/dt-config-ci-results/event-worker.log 2>&1
printf 'Backend CI-equivalent checks passed\n'
