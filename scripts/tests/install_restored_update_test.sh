#!/bin/bash
set -Eeuo pipefail

# Real Compose/PG/application/volume integration; only registry transport and
# host cron installation are replaced. Never touches an existing deployment.
[[ $# == 1 ]] || { echo "Usage: $0 APPLICATION_IMAGE" >&2; exit 2; }
TEST_BASE_IMAGE="$1"
REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
FIXTURE_ROOT="$(mktemp -d /tmp/dreamtrans-restored-update.XXXXXX)"
export INSTALL_DIR="$FIXTURE_ROOT/install"
mkdir "$INSTALL_DIR"
TEST_ID="restore-test-$$"
export COMPOSE_PROJECT_NAME="$TEST_ID"
unset COMPOSE_FILE IMAGE_TAG
TEST_APP_VOLUME="${TEST_ID}_appdata_final"
TEST_DB_VOLUME="${TEST_ID}_postgres_final"
TEST_OTHER_VOLUME="${TEST_ID}_unrelated"
TEST_OLD_IMAGE="dreamtrans-migration/app:$TEST_ID"
TEST_DB_IMAGE="dreamtrans-migration/db:$TEST_ID"
TEST_STANDARD_OLD_IMAGE="ghcr.io/coyumelabs/dreamtrans:$TEST_ID-old"
TEST_NEW_IMAGE="ghcr.io/coyumelabs/dreamtrans:$TEST_ID"
TEST_BAD_IMAGE="ghcr.io/coyumelabs/dreamtrans:$TEST_ID-health"
TEST_SQL_IMAGE="ghcr.io/coyumelabs/dreamtrans:$TEST_ID-sql"
cleanup() {
    (cd "$INSTALL_DIR" && docker compose down -v --remove-orphans) >/dev/null 2>&1 || true
    docker volume rm "$TEST_APP_VOLUME" "$TEST_DB_VOLUME" "$TEST_OTHER_VOLUME" >/dev/null 2>&1 || true
    docker image rm "$TEST_STANDARD_OLD_IMAGE" "$TEST_OLD_IMAGE" "$TEST_DB_IMAGE" "$TEST_NEW_IMAGE" "$TEST_BAD_IMAGE" "$TEST_SQL_IMAGE" >/dev/null 2>&1 || true
    rm -rf -- "$FIXTURE_ROOT"
}
trap cleanup EXIT
source <(sed '$d' "$REPO_ROOT/scripts/install.sh")
COMPOSE_CMD="test_compose"
cd "$INSTALL_DIR"
# Preserve real updater decisions, replace only network fetches with prebuilt
# local release images. Both authenticated and anonymous pull failures execute.
test_compose() {
    if [[ "$1" == pull ]]; then
        test "${2:-}" = --policy && test "${3:-}" = always || return 97
        printf '%s\n' "$*" >> "$FIXTURE_ROOT/pulls"
        [[ "${FAIL_PULL:-false}" != true ]] || return 42
        local ref
        while IFS= read -r ref; do
            docker image inspect "$ref" >/dev/null || return 1
        done < <(docker compose config --images)
        return 0
    fi
    docker compose "$@"
}
configure_backup_cron() { :; }
# Faster timeout, same real readiness and immutable-image checks.
eval "$(declare -f wait_for_app_ready | sed 's/local max_attempts=75/local max_attempts=10/')"

docker image inspect "$POSTGRES_IMAGE" >/dev/null 2>&1 || docker pull "$POSTGRES_IMAGE"
docker tag "$TEST_BASE_IMAGE" "$TEST_OLD_IMAGE"
docker tag "$TEST_BASE_IMAGE" "$TEST_STANDARD_OLD_IMAGE"
docker build --build-arg "BASE=$POSTGRES_IMAGE" -t "$TEST_DB_IMAGE" -f - "$FIXTURE_ROOT" <<'DOCKER'
ARG BASE
FROM ${BASE}
LABEL dreamtrans.update-fixture="restored-database"
DOCKER
cat > "$FIXTURE_ROOT/Dockerfile" <<'DOCKER'
ARG BASE
FROM ${BASE}
USER root
RUN echo updated-release > /app/update-test-version
USER dreamtrans
DOCKER
docker build --build-arg "BASE=$TEST_BASE_IMAGE" -t "$TEST_NEW_IMAGE" "$FIXTURE_ROOT" >/dev/null
cat > "$FIXTURE_ROOT/Dockerfile" <<'DOCKER'
ARG BASE
FROM ${BASE}
USER root
RUN touch /app/update-test-unhealthy
USER dreamtrans
DOCKER
docker build --build-arg "BASE=$TEST_NEW_IMAGE" -t "$TEST_BAD_IMAGE" "$FIXTURE_ROOT" >/dev/null
# An earlier transaction commits before the injected SQL failure. Recovery must
# preserve it and explicitly report that database rollback did not happen.
cat > "$FIXTURE_ROOT/fail-migrate.sh" <<'SH'
#!/bin/sh
set -eu
psql -X -v ON_ERROR_STOP=1 -c 'CREATE TABLE IF NOT EXISTS update_committed(value text); INSERT INTO update_committed VALUES ('"'"'committed-before-failure'"'"');'
psql -X -v ON_ERROR_STOP=1 -c 'SELECT deliberately_missing_update_function();'
SH
cat > "$FIXTURE_ROOT/Dockerfile" <<'DOCKER'
ARG BASE
FROM ${BASE}
USER root
COPY --chmod=0555 fail-migrate.sh /usr/share/dreamtrans/migrate.sh
USER dreamtrans
DOCKER
docker build --build-arg "BASE=$TEST_NEW_IMAGE" -t "$TEST_SQL_IMAGE" "$FIXTURE_ROOT" >/dev/null

generate_compose_file
cat > .env <<ENV
COMPOSE_FILE=docker-compose.yml:compose.restore.yml:compose.production.yml
IMAGE_TAG=$TEST_ID
SM_API_KEY=isolated-test-key
POSTGRES_USER=dreamtrans
POSTGRES_DB=dreamtrans
POSTGRES_PASSWORD=isolated-test-password
JWT_SECRET=0123456789abcdef0123456789abcdef
JWT_REFRESH_SECRET=fedcba9876543210fedcba9876543210
ADMIN_EMAIL=update-test@example.test
ADMIN_PASSWORD=isolated-admin-password
BIND_ADDRESS=127.0.0.1
PORT=0
ALLOW_ANONYMOUS_API=false
CORS_ALLOWED_ORIGINS=https://production.example.test
ENV
cat > compose.restore.yml <<YAML
services:
  app:
    image: $TEST_OLD_IMAGE
    pull_policy: never
  db:
    image: $TEST_DB_IMAGE
    pull_policy: never
  migrate:
    image: $TEST_DB_IMAGE
    pull_policy: never
YAML
cat > compose.production.yml <<YAML
services:
  app:
    environment:
      CORS_ALLOWED_ORIGINS: https://production.example.test
      RESTORE_SENTINEL: preserve-me
    healthcheck:
      test: [CMD-SHELL, 'test ! -f /app/update-test-unhealthy && wget -q -O /dev/null http://127.0.0.1:8080/readyz']
      interval: 1s
      start_period: 1s
      retries: 1
volumes:
  appdata:
    name: $TEST_APP_VOLUME
    external: true
  pgdata:
    name: $TEST_DB_VOLUME
    external: true
YAML
cat > start-production.sh <<'SH'
#!/bin/sh
exec docker compose -f docker-compose.yml -f compose.restore.yml -f compose.production.yml up -d
SH
chmod +x start-production.sh
printf '%s\n' '# existing backup helper used by cron' > backup.sh
for volume in "$TEST_APP_VOLUME" "$TEST_DB_VOLUME" "$TEST_OTHER_VOLUME"; do
    docker volume create "$volume" >/dev/null
done
# The updater is authorized to touch only the selected appdata volume.
docker run --rm --user 0:0 --entrypoint sh -v "$TEST_OTHER_VOLUME:/unrelated" "$TEST_BASE_IMAGE" -ec 'echo yuaction-sentinel > /unrelated/keep; chown -R 123:456 /unrelated'
prepare_release_migrations
docker compose up -d db
wait_for_db
run_migrations
docker compose up -d app
wait_for_app_ready
retire_admin_bootstrap_credentials
# New writes made AFTER restoration must survive every path.
docker compose exec -T db psql -U dreamtrans -d dreamtrans -c "CREATE TABLE restore_production_data(value text); INSERT INTO restore_production_data VALUES ('new-production-write');"
docker compose exec -T app sh -ec 'echo new-production-audio > /app/data/restore-sentinel'
cp -p .env "$FIXTURE_ROOT/original.env"
cp -p docker-compose.yml "$FIXTURE_ROOT/original.base"
cp -p compose.restore.yml "$FIXTURE_ROOT/original.restore"
cp -p compose.production.yml "$FIXTURE_ROOT/original.production"
cp -p backup.sh "$FIXTURE_ROOT/original.backup"
cp -p start-production.sh "$FIXTURE_ROOT/original.start"
OLD_ID="$(docker image inspect --format '{{.Id}}' "$TEST_OLD_IMAGE")"
OLD_DB_ID="$(docker image inspect --format '{{.Id}}' "$TEST_DB_IMAGE")"
NEW_ID="$(docker image inspect --format '{{.Id}}' "$TEST_NEW_IMAGE")"
test "$OLD_ID" != "$NEW_ID"

assert_data() {
    local app db
    app="$(docker compose ps -a -q app)"
    db="$(docker compose ps -a -q db)"
    test "$(confirmed_service_volume app /app/data "$app")" = "$TEST_APP_VOLUME"
    test "$(confirmed_service_volume db /var/lib/postgresql/data "$db")" = "$TEST_DB_VOLUME"
    test "$(docker compose exec -T app cat /app/data/restore-sentinel)" = new-production-audio
    test "$(docker compose exec -T db psql -At -U dreamtrans -d dreamtrans -c 'SELECT value FROM restore_production_data')" = new-production-write
    test "$(docker compose exec -T app printenv CORS_ALLOWED_ORIGINS)" = https://production.example.test
    test "$(docker compose exec -T app printenv RESTORE_SENTINEL)" = preserve-me
    cmp compose.production.yml "$FIXTURE_ROOT/original.production"
    cmp start-production.sh "$FIXTURE_ROOT/original.start"
    test "$(docker run --rm --user 0:0 --entrypoint sh -v "$TEST_OTHER_VOLUME:/unrelated" "$TEST_BASE_IMAGE" -ec 'cat /unrelated/keep; stat -c %u:%g /unrelated/keep')" = $'yuaction-sentinel\n123:456'
}
assert_rollback() {
    test "$(docker inspect --format '{{.Image}}' "$(docker compose ps -q db)")" = "$OLD_DB_ID"
    test "$(docker inspect --format '{{.Image}}' "$(docker compose ps -q app)")" = "$OLD_ID"
    for pair in '.env env' 'docker-compose.yml base' 'compose.restore.yml restore' 'backup.sh backup'; do
        read -r file suffix <<< "$pair"
        cmp "$file" "$FIXTURE_ROOT/original.$suffix"
    done
    assert_data
    ./start-production.sh
    assert_data
}
for failure in pull sql health; do
    echo "Testing restored update failure: $failure"
    FAIL_PULL=false
    IMAGE_TAG="$TEST_ID"
    case "$failure" in
        pull) FAIL_PULL=true ;;
        sql) IMAGE_TAG="$TEST_ID-sql" ;;
        health) IMAGE_TAG="$TEST_ID-health" ;;
    esac
    IMAGE_TAG_EXPLICIT=true
    if update_installation > "$FIXTURE_ROOT/$failure.log" 2>&1; then
        echo "Injected $failure failure unexpectedly succeeded" >&2
        exit 1
    fi
    cat "$FIXTURE_ROOT/$failure.log"
    case "$failure" in
        pull) grep -q 'Unable to pull the DreamTrans application image' "$FIXTURE_ROOT/$failure.log" ;;
        sql) grep -q 'deliberately_missing_update_function' "$FIXTURE_ROOT/$failure.log" ;;
        health) grep -q 'DreamTrans did not become ready' "$FIXTURE_ROOT/$failure.log" ;;
    esac
    FAIL_PULL=false
    unset IMAGE_TAG
    assert_rollback
    if [[ "$failure" != pull ]]; then
        grep -q 'committed schema/data changes have NOT been reverted' "$FIXTURE_ROOT/$failure.log"
    fi
    if [[ "$failure" == sql ]]; then
        test "$(docker compose exec -T db psql -At -U dreamtrans -d dreamtrans -c 'SELECT value FROM update_committed')" = committed-before-failure
    fi
done

# Fail closed on mismatched production selection, binds, missing volumes and
# unlabelled non-external volumes before any permission helper can run.
PREVIOUS_APP_CONTAINER_ID="$(docker compose ps -q app)"
PREVIOUS_DB_CONTAINER_ID="$(docker compose ps -q db)"
if confirmed_service_volume app /app/data "$PREVIOUS_APP_CONTAINER_ID" true; then
    echo 'A discovery container was accepted as evidence for an external volume' >&2
    exit 1
fi
for invalid in mismatch missing managed bind; do
    cp "$FIXTURE_ROOT/original.production" compose.production.yml
    case "$invalid" in
        mismatch) sed -i "s/$TEST_APP_VOLUME/$TEST_OTHER_VOLUME/" compose.production.yml ;;
        missing) sed -i "s/$TEST_APP_VOLUME/${TEST_ID}_absent/" compose.production.yml ;;
        managed) sed -i 's/external: true/external: false/' compose.production.yml ;;
        bind) printf '\n' >> compose.production.yml
              sed -i '/  app:/a\    volumes:\n      - /tmp:/app/data' compose.production.yml ;;
    esac
    if validate_update_data_volumes; then
        echo "Unsafe $invalid volume unexpectedly accepted" >&2
        exit 1
    fi
done
cp "$FIXTURE_ROOT/original.production" compose.production.yml

IMAGE_TAG="$TEST_ID"
IMAGE_TAG_EXPLICIT=true
update_installation
assert_data
test "$(docker inspect --format '{{.Image}}' "$(docker compose ps -q app)")" = "$NEW_ID"
test "$(docker compose exec -T app cat /app/update-test-version)" = updated-release
! grep -q 'dreamtrans-migration/\|pull_policy: never' compose.restore.yml
# Explicit -f startup survives recreation, and repeated standard updates work.
docker compose stop app
./start-production.sh
APP_IMAGE_ID="$NEW_ID"
wait_for_app_ready
assert_data
# Publish different contents under the SAME tag: a repeat update must replace
# the running image even though the selected reference is already cached.
FIRST_RELEASE_ID="$NEW_ID"
docker build --build-arg "BASE=$TEST_NEW_IMAGE" -t "$TEST_NEW_IMAGE" -f - "$FIXTURE_ROOT" <<'DOCKER'
ARG BASE
FROM ${BASE}
LABEL dreamtrans.update-fixture="second-release-same-tag"
DOCKER
NEW_ID="$(docker image inspect --format '{{.Id}}' "$TEST_NEW_IMAGE")"
test "$NEW_ID" != "$FIRST_RELEASE_ID"
update_installation
assert_data
test "$(docker inspect --format '{{.Image}}' "$(docker compose ps -q app)")" = "$NEW_ID"
test "$(docker inspect --format '{{.Image}}' "$(docker compose ps -q db)")" = "$(docker image inspect --format '{{.Id}}' "$POSTGRES_IMAGE")"
# Standard installation without restore image overrides also takes this path.
cat > compose.restore.yml <<'YAML'
services: {}
YAML
update_installation
assert_data
# Exercise actual ownership conversion AND reversal on the unlabelled external
# volume, including a stopped old container. Database contents stay untouched.
docker compose stop app
docker run --rm --user 0:0 --entrypoint sh -v "$TEST_APP_VOLUME:/app/data" "$TEST_BASE_IMAGE" -ec 'chown -R 100:101 /app/data'
begin_update_transaction
APP_IMAGE_ID="$NEW_ID"
repair_app_data_permissions_for_update
test "$APP_DATA_PREVIOUS_OWNER" = 100:101
test "$APP_DATA_PERMISSION_MIGRATION_ATTEMPTED" = true
rollback_update_deployment
test "$(docker run --rm --user 0:0 --entrypoint stat -v "$TEST_APP_VOLUME:/app/data" "$TEST_BASE_IMAGE" -c '%u:%g' /app/data/restore-sentinel)" = 100:101
docker run --rm --user 0:0 --entrypoint sh -v "$TEST_APP_VOLUME:/app/data" "$TEST_BASE_IMAGE" -ec 'chown -R 10001:10001 /app/data'
./start-production.sh
wait_for_app_ready
assert_data
# A genuinely ordinary installation has no override chain and uses volumes
# created/labeled by Compose. Verify the full standard update path as well.
docker compose down --remove-orphans
cp "$FIXTURE_ROOT/original.env" .env
sed -i '/^COMPOSE_FILE=/d' .env
set_env_value IMAGE_TAG "$TEST_ID-old"
IMAGE_TAG="$TEST_ID-old"
export IMAGE_TAG
generate_compose_file
prepare_release_migrations
docker compose up -d app
APP_IMAGE_ID="$OLD_ID"
wait_for_app_ready
NORMAL_APP_VOLUME="$(confirmed_service_volume app /app/data "$(docker compose ps -q app)")"
NORMAL_DB_VOLUME="$(confirmed_service_volume db /var/lib/postgresql/data "$(docker compose ps -q db)")"
test "$NORMAL_APP_VOLUME" != "$TEST_APP_VOLUME"
test "$NORMAL_DB_VOLUME" != "$TEST_DB_VOLUME"
docker compose exec -T app sh -ec 'echo normal-new-write > /app/data/standard-sentinel'
docker compose exec -T db psql -U dreamtrans -d dreamtrans -c "CREATE TABLE normal_data(value text); INSERT INTO normal_data VALUES ('normal-new-write');"
IMAGE_TAG="$TEST_ID"
update_installation
test "$(docker inspect --format '{{.Image}}' "$(docker compose ps -q app)")" = "$NEW_ID"
test "$(confirmed_service_volume app /app/data "$(docker compose ps -q app)")" = "$NORMAL_APP_VOLUME"
test "$(confirmed_service_volume db /var/lib/postgresql/data "$(docker compose ps -q db)")" = "$NORMAL_DB_VOLUME"
test "$(docker compose exec -T app cat /app/data/standard-sentinel)" = normal-new-write
test "$(docker compose exec -T db psql -At -U dreamtrans -d dreamtrans -c 'SELECT value FROM normal_data')" = normal-new-write
echo 'Restored and standard deployment update integration checks passed'
