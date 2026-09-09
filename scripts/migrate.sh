#!/bin/sh
set -eu

: "${MIGRATIONS_DIR:=/migrations}"
: "${PGHOST:?PGHOST must be set}"
: "${PGPORT:=5432}"
: "${PGDATABASE:?PGDATABASE must be set}"
: "${PGUSER:?PGUSER must be set}"
: "${PGPASSWORD:?PGPASSWORD must be set}"

PSQL="psql -X -v ON_ERROR_STOP=1"

$PSQL <<'SQL'
CREATE TABLE IF NOT EXISTS schema_migrations (
    version VARCHAR(255) PRIMARY KEY,
    checksum CHAR(64),
    applied_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP
);
ALTER TABLE schema_migrations
    ADD COLUMN IF NOT EXISTS checksum CHAR(64);
SQL

# The runner and SQL bundle ship together in one immutable application image.
# Keep this marker aligned with the highest migration in that release so a
# truncated/corrupt bundle fails before applying only a prefix.
expected_latest_prefix=052
expected_sequence=1
latest_prefix=
found_migration=false
for migration in "$MIGRATIONS_DIR"/[0-9][0-9][0-9]_*.sql; do
    if [ ! -f "$migration" ]; then
        continue
    fi
    found_migration=true
    version=${migration##*/}
    case "$version" in
        *[!A-Za-z0-9_.-]*)
            echo "Unsafe migration filename: $version" >&2
            exit 1
            ;;
    esac
    prefix=${version%%_*}
    expected_prefix=$(printf '%03d' "$expected_sequence")
    if [ "$prefix" != "$expected_prefix" ]; then
        echo "Migration sequence is incomplete: expected $expected_prefix, found $prefix" >&2
        exit 1
    fi
    latest_prefix=$prefix
    expected_sequence=$((expected_sequence + 1))
done

if [ "$found_migration" != "true" ]; then
    echo "No migration files found in $MIGRATIONS_DIR" >&2
    exit 1
fi
if [ "$latest_prefix" != "$expected_latest_prefix" ]; then
    echo "Migration bundle is incomplete: expected latest version $expected_latest_prefix, found $latest_prefix" >&2
    exit 1
fi

for migration in "$MIGRATIONS_DIR"/[0-9][0-9][0-9]_*.sql; do
    version=${migration##*/}
    checksum=$(sha256sum "$migration")
    checksum=${checksum%% *}
    case "$checksum" in
        ''|*[!0-9a-f]*)
            echo "Unable to calculate SHA-256 checksum for migration: $version" >&2
            exit 1
            ;;
    esac
    if [ "${#checksum}" -ne 64 ]; then
        echo "Invalid SHA-256 checksum for migration: $version" >&2
        exit 1
    fi

    # 021 was briefly published before its legacy-table repair also merged
    # duplicate provider/model rows. Instances where that first revision
    # succeeded already have the intended composite primary key and do not
    # need to rerun the expanded migration. Accept only that exact historical
    # checksum, then advance its recorded checksum to the bundled revision.
    compatible_checksum=$checksum
    case "$version" in
        021_provider_models_primary_key.sql)
            compatible_checksum=7fa7f919b164148b15d76158fd8bde5e2e19cefddbab5d8e75af45696e574f6b
            ;;
    esac

    echo "Applying migration: $version"
    {
        printf '%s\n' \
            'BEGIN;' \
            'SELECT pg_advisory_xact_lock(1146243412, 1);' \
            'DO $migration_checksum$' \
            'BEGIN' \
            "  IF EXISTS (" \
            "    SELECT 1 FROM schema_migrations" \
            "    WHERE version = '$version'" \
            "      AND checksum IS NOT NULL" \
            "      AND checksum <> '$checksum'" \
            "      AND checksum <> '$compatible_checksum'" \
            "  ) THEN" \
            "    RAISE EXCEPTION 'migration checksum mismatch: $version';" \
            "  END IF;" \
            'END' \
            '$migration_checksum$;' \
            "UPDATE schema_migrations" \
            "SET checksum = '$checksum'" \
            "WHERE version = '$version'" \
            "  AND (checksum IS NULL OR checksum = '$compatible_checksum');" \
            "SELECT NOT EXISTS (SELECT 1 FROM schema_migrations WHERE version = '$version') AS migration_pending" \
            '\gset' \
            '\if :migration_pending'
        cat "$migration"
        printf '\n%s\n' \
            "INSERT INTO schema_migrations (version, checksum) VALUES ('$version', '$checksum');" \
            '\else' \
            "\\echo Migration $version already applied" \
            '\endif' \
            'COMMIT;'
    } | $PSQL
done
