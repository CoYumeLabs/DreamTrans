#!/bin/bash
set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "$SCRIPT_DIR/../.." && pwd)"
FIXTURE_ROOT="$(mktemp -d /tmp/dreamtrans-fresh-lifecycle.XXXXXX)"
HOLDER_PID=""

cleanup() {
    if [[ -n "$HOLDER_PID" ]]; then
        kill "$HOLDER_PID" 2>/dev/null || true
        wait "$HOLDER_PID" 2>/dev/null || true
    fi
    case "$FIXTURE_ROOT" in
        /tmp/dreamtrans-fresh-lifecycle.*)
            find "$FIXTURE_ROOT" -depth -delete
            ;;
    esac
}
trap cleanup EXIT

export INSTALL_DIR="$FIXTURE_ROOT/install"
# shellcheck source=/dev/null
source <(sed '$d' "$REPO_ROOT/scripts/install.sh")

COMPOSE_LOG_PATH="$FIXTURE_ROOT/compose-cleanup.log"
mock_compose_cleanup() {
    printf '%s\n' "$*" >> "$COMPOSE_LOG_PATH"
}
COMPOSE_CMD="mock_compose_cleanup"

# A fresh attempt is marked as recoverable before any generated configuration is
# written. Recovery removes only the known installer files and fresh Docker
# resources while holding the lifecycle lock, then leaves the same locked target
# ready for the next attempt.
begin_fresh_install_transaction
claim_fresh_install_target
create_directories
test -f "$INSTALL_DIR/$INSTALL_IN_PROGRESS_MARKER"
test ! -e "$INSTALL_DIR/$INSTALL_SENTINEL"
printf '%s\n' 'SM_API_KEY=fresh-test' > "$INSTALL_DIR/.env"
printf '%s\n' 'services: {}' > "$INSTALL_DIR/docker-compose.yml"
printf '%s\n' '#!/bin/sh' > "$INSTALL_DIR/migrate.sh"
printf '%s\n' '-- test migration' > "$INSTALL_DIR/migrations/001_test.sql"
FRESH_INSTALL_TRANSACTION_ACTIVE="false"
trap - ERR INT TERM
release_update_lock
FRESH_INSTALL_LOCK_OWNED="false"

begin_fresh_install_transaction
claim_fresh_install_target
test -d "$INSTALL_DIR"
fresh_install_target_contains_only_lock
test "$(cat "$COMPOSE_LOG_PATH")" = "down -v --remove-orphans"

# Finalization atomically promotes the in-progress marker only after startup has
# succeeded.
create_directories
finalize_fresh_install
test "$FRESH_INSTALL_TRANSACTION_ACTIVE" = "false"
test -f "$INSTALL_DIR/$INSTALL_SENTINEL"
test ! -e "$INSTALL_DIR/$INSTALL_IN_PROGRESS_MARKER"
has_valid_install_sentinel

# A crash after the durable sentinel is written but before the in-progress
# marker is removed must never tear down the already-healthy installation.
printf '%s\n' 'durable-data' > "$INSTALL_DIR/data/completed-session"
acquire_update_lock
FRESH_INSTALL_LOCK_OWNED="true"
FRESH_INSTALL_ATTEMPT_ID="$(random_string 32)"
write_install_in_progress_marker
FRESH_INSTALL_LOCK_OWNED="false"
release_update_lock
: > "$COMPOSE_LOG_PATH"
acquire_update_lock
recover_incomplete_fresh_install
release_update_lock
test -f "$INSTALL_DIR/$INSTALL_SENTINEL"
test ! -e "$INSTALL_DIR/$INSTALL_IN_PROGRESS_MARKER"
test "$(cat "$INSTALL_DIR/data/completed-session")" = "durable-data"
test ! -s "$COMPOSE_LOG_PATH"

# Unknown files are never deleted by automatic recovery. Keeping the recovery
# marker makes the state diagnosable and retryable after the operator moves them.
find "$INSTALL_DIR" -depth -delete
begin_fresh_install_transaction
claim_fresh_install_target
create_directories
printf '%s\n' 'operator-owned' > "$INSTALL_DIR/operator-note.txt"
if cleanup_incomplete_fresh_install "$FRESH_INSTALL_ATTEMPT_ID"; then
    echo "Fresh recovery unexpectedly deleted or accepted an unknown file" >&2
    exit 1
fi
test "$(cat "$INSTALL_DIR/operator-note.txt")" = "operator-owned"
test -f "$INSTALL_DIR/$INSTALL_IN_PROGRESS_MARKER"
FRESH_INSTALL_TRANSACTION_ACTIVE="false"
trap - ERR INT TERM
release_update_lock
FRESH_INSTALL_LOCK_OWNED="false"

# Two fresh installers may validate the same initially empty path, but only one
# can claim it. The loser must neither replace the winner's attempt marker nor
# remove any files written by the winning transaction.
CONCURRENT_INSTALL_DIR="$FIXTURE_ROOT/concurrent-install"
HOLDER_READY="$FIXTURE_ROOT/concurrent-holder.ready"
HOLDER_RELEASE="$FIXTURE_ROOT/concurrent-holder.release"
HOLDER_OWNER="$FIXTURE_ROOT/concurrent-holder.owner"
env \
    INSTALLER_UNDER_TEST="$REPO_ROOT/scripts/install.sh" \
    CONCURRENT_INSTALL_DIR="$CONCURRENT_INSTALL_DIR" \
    HOLDER_READY="$HOLDER_READY" \
    HOLDER_RELEASE="$HOLDER_RELEASE" \
    HOLDER_OWNER="$HOLDER_OWNER" \
    bash -c '
        set -Eeuo pipefail
        source <(head -n -1 "$INSTALLER_UNDER_TEST")
        INSTALL_DIR="$CONCURRENT_INSTALL_DIR"
        COMPOSE_CMD=true
        begin_fresh_install_transaction
        claim_fresh_install_target
        create_directories
        printf "%s\n" "$FRESH_INSTALL_ATTEMPT_ID" > "$HOLDER_OWNER"
        printf "%s\n" "winner-data" > "$INSTALL_DIR/data/winner"
        : > "$HOLDER_READY"
        while [[ ! -e "$HOLDER_RELEASE" ]]; do
            sleep 0.05
        done
        FRESH_INSTALL_TRANSACTION_ACTIVE="false"
        trap - ERR INT TERM
        release_update_lock
        FRESH_INSTALL_LOCK_OWNED="false"
    ' &
HOLDER_PID="$!"

for _ in $(seq 1 200); do
    [[ -e "$HOLDER_READY" ]] && break
    if ! kill -0 "$HOLDER_PID" 2>/dev/null; then
        wait "$HOLDER_PID"
        echo "Concurrent fresh-install holder exited before acquiring the lock" >&2
        exit 1
    fi
    sleep 0.05
done
test -e "$HOLDER_READY"
winning_attempt="$(cat "$HOLDER_OWNER")"
test "$(sed -n '3p' "$CONCURRENT_INSTALL_DIR/$INSTALL_IN_PROGRESS_MARKER")" = \
    "attempt=$winning_attempt"

if env \
    INSTALLER_UNDER_TEST="$REPO_ROOT/scripts/install.sh" \
    CONCURRENT_INSTALL_DIR="$CONCURRENT_INSTALL_DIR" \
    bash -c '
        set -Eeuo pipefail
        source <(head -n -1 "$INSTALLER_UNDER_TEST")
        INSTALL_DIR="$CONCURRENT_INSTALL_DIR"
        COMPOSE_CMD=true
        begin_fresh_install_transaction
        claim_fresh_install_target
    ' >"$FIXTURE_ROOT/concurrent-contender.out" 2>&1; then
    echo "Concurrent fresh installer unexpectedly acquired the lifecycle lock" >&2
    exit 1
fi
grep -q 'Another DreamTrans lifecycle operation is already running' \
    "$FIXTURE_ROOT/concurrent-contender.out"
test "$(sed -n '3p' "$CONCURRENT_INSTALL_DIR/$INSTALL_IN_PROGRESS_MARKER")" = \
    "attempt=$winning_attempt"
test "$(cat "$CONCURRENT_INSTALL_DIR/data/winner")" = "winner-data"

: > "$HOLDER_RELEASE"
wait "$HOLDER_PID"
HOLDER_PID=""

# A failure after the winning process writes its marker cleans that attempt's
# files and lock, leaving no claim behind.
FAILED_INSTALL_DIR="$FIXTURE_ROOT/failed-install"
if env \
    INSTALLER_UNDER_TEST="$REPO_ROOT/scripts/install.sh" \
    FAILED_INSTALL_DIR="$FAILED_INSTALL_DIR" \
    bash -c '
        set -Eeuo pipefail
        source <(head -n -1 "$INSTALLER_UNDER_TEST")
        INSTALL_DIR="$FAILED_INSTALL_DIR"
        COMPOSE_CMD=true
        begin_fresh_install_transaction
        claim_fresh_install_target
        create_directories
        false
    ' >"$FIXTURE_ROOT/failed-install.out" 2>&1; then
    echo "Failed fresh-install transaction unexpectedly succeeded" >&2
    exit 1
fi
grep -q 'Fresh installation failed; cleaning only resources created by this attempt' \
    "$FIXTURE_ROOT/failed-install.out"
test ! -e "$FAILED_INSTALL_DIR"

# A fully parameterized invocation must work without a controlling terminal and
# must write its informational output to stdout.
NO_TTY_OUTPUT="$(
    env \
        INSTALLER_UNDER_TEST="$REPO_ROOT/scripts/install.sh" \
        SM_API_KEY="no-tty-sm-key" \
        OPENAI_API_KEY="" \
        OPENAI_API_BASE="" \
        ADMIN_EMAIL="no-tty@example.test" \
        ADMIN_PASSWORD="no-tty-password-123" \
        setsid bash -c '
            set -Eeuo pipefail
            source <(head -n -1 "$INSTALLER_UNDER_TEST")
            ! has_controlling_tty
            prompt_api_keys
            test "$OPENAI_API_BASE" = "https://api.openai.com/v1"
            COMPOSE_CMD="docker compose"
            INSTALL_DIR="/tmp/dreamtrans-no-tty-output"
            PORT="16002"
            ADMIN_DISPLAY_EMAIL="$ADMIN_EMAIL"
            ADMIN_DISPLAY_PASSWORD="$ADMIN_PASSWORD"
            print_completion
        ' </dev/null
)"
printf '%s\n' "$NO_TTY_OUTPUT" | grep -q 'DreamTrans installed successfully'
printf '%s\n' "$NO_TTY_OUTPUT" | grep -q 'no-tty@example.test'

if env \
    INSTALLER_UNDER_TEST="$REPO_ROOT/scripts/install.sh" \
    SM_API_KEY="" \
    ADMIN_EMAIL="no-tty@example.test" \
    ADMIN_PASSWORD="no-tty-password-123" \
    setsid bash -c '
        set -Eeuo pipefail
        source <(head -n -1 "$INSTALLER_UNDER_TEST")
        prompt_api_keys
    ' </dev/null >"$FIXTURE_ROOT/no-tty-missing.out" 2>&1; then
    echo "Missing non-interactive Speechmatics credentials were unexpectedly accepted" >&2
    exit 1
fi
grep -q 'SM_API_KEY is required when no interactive terminal is available' \
    "$FIXTURE_ROOT/no-tty-missing.out"

# ADMIN_PASSWORD_GENERATED is private installer state. An inherited value must
# not trick the completion page into printing a user-supplied password.
INJECTED_FLAG_OUTPUT="$(
    env \
        INSTALLER_UNDER_TEST="$REPO_ROOT/scripts/install.sh" \
        ADMIN_PASSWORD_GENERATED="true" \
        ADMIN_EMAIL="injected@example.test" \
        ADMIN_PASSWORD="injected-user-password-123" \
        bash -c '
            set -Eeuo pipefail
            source <(head -n -1 "$INSTALLER_UNDER_TEST")
            test "$ADMIN_PASSWORD_GENERATED" = "false"
            COMPOSE_CMD="docker compose"
            INSTALL_DIR="/tmp/dreamtrans-injected-flag-output"
            PORT="16002"
            ADMIN_DISPLAY_EMAIL="$ADMIN_EMAIL"
            ADMIN_DISPLAY_PASSWORD="$ADMIN_PASSWORD"
            print_completion
        '
)"
if printf '%s\n' "$INJECTED_FLAG_OUTPUT" |
    grep -Fq 'injected-user-password-123'; then
    echo "Inherited generated-password state leaked a user-supplied password" >&2
    exit 1
fi

# The update lock rejects a concurrent holder and becomes available immediately
# after the owning process releases it.
INSTALL_DIR="$FIXTURE_ROOT/lock-install"
mkdir -p "$INSTALL_DIR"
acquire_update_lock
test "$(stat -c '%a' "$INSTALL_DIR/.dreamtrans-update.lock")" = "600"
if flock -n "$INSTALL_DIR/.dreamtrans-update.lock" -c true; then
    echo "Concurrent update lock acquisition unexpectedly succeeded" >&2
    exit 1
fi
release_update_lock
flock -n "$INSTALL_DIR/.dreamtrans-update.lock" -c true

# A lock pathname supplied as a symlink or non-regular file is rejected without
# truncating or otherwise modifying the symlink target.
lock_path="$INSTALL_DIR/.dreamtrans-update.lock"
sensitive_path="$FIXTURE_ROOT/must-not-be-truncated"
printf '%s\n' 'preserve-this-content' > "$sensitive_path"
rm -f -- "$lock_path"
ln -s -- "$sensitive_path" "$lock_path"
if acquire_update_lock; then
    echo "Symlinked lifecycle lock was unexpectedly accepted" >&2
    exit 1
fi
test "$(cat "$sensitive_path")" = "preserve-this-content"
rm -f -- "$lock_path"
mkdir "$lock_path"
if acquire_update_lock; then
    echo "Non-regular lifecycle lock was unexpectedly accepted" >&2
    exit 1
fi
rmdir "$lock_path"

# Stop/start/restart all acquire the same lifecycle lock. Their Compose work
# runs after installation revalidation and the lock remains reusable.
INSTALL_DIR="$FIXTURE_ROOT/lifecycle-install"
mkdir -p "$INSTALL_DIR/data" "$INSTALL_DIR/migrations"
printf '%s\n' 'fixture=true' > "$INSTALL_DIR/.env"
printf '%s\n' 'services: {}' > "$INSTALL_DIR/docker-compose.yml"
write_install_sentinel
LIFECYCLE_COMPOSE_LOG="$FIXTURE_ROOT/lifecycle-compose.log"
mock_compose_lifecycle() {
    printf '%s\n' "$*" >> "$LIFECYCLE_COMPOSE_LOG"
}
wait_for_app_ready() {
    return 0
}
show_access_info() {
    return 0
}
COMPOSE_CMD="mock_compose_lifecycle"

stop_services
start_services_only
restart_services
test "$(sed -n '1p' "$LIFECYCLE_COMPOSE_LOG")" = "down"
test "$(sed -n '2p' "$LIFECYCLE_COMPOSE_LOG")" = "up -d"
test "$(sed -n '3p' "$LIFECYCLE_COMPOSE_LOG")" = "restart"
test "$(sed -n '$=' "$LIFECYCLE_COMPOSE_LOG")" = "3"
test -f "$INSTALL_DIR/.dreamtrans-update.lock"

# A held update lock prevents lifecycle work before Compose is invoked.
acquire_update_lock
if stop_services; then
    echo "Stop unexpectedly ran while the lifecycle lock was already held" >&2
    exit 1
fi
test "$(sed -n '$=' "$LIFECYCLE_COMPOSE_LOG")" = "3"
release_update_lock

# --init-db and --migrate serialize migration asset replacement, database
# startup, and migration execution with the same lifecycle lock.
DATABASE_MAINTENANCE_LOG="$FIXTURE_ROOT/database-maintenance.log"
mock_compose_database() {
    printf 'compose %s\n' "$*" >> "$DATABASE_MAINTENANCE_LOG"
}
prepare_release_migrations() {
    printf '%s\n' "prepare" >> "$DATABASE_MAINTENANCE_LOG"
}
init_database() {
    printf '%s\n' "init" >> "$DATABASE_MAINTENANCE_LOG"
}
run_migrations() {
    printf '%s\n' "migrate" >> "$DATABASE_MAINTENANCE_LOG"
}
COMPOSE_CMD="mock_compose_database"
run_database_maintenance init
run_database_maintenance migrate
test "$(sed -n '1p' "$DATABASE_MAINTENANCE_LOG")" = "prepare"
test "$(sed -n '2p' "$DATABASE_MAINTENANCE_LOG")" = "compose up -d db"
test "$(sed -n '3p' "$DATABASE_MAINTENANCE_LOG")" = "init"
test "$(sed -n '4p' "$DATABASE_MAINTENANCE_LOG")" = "prepare"
test "$(sed -n '5p' "$DATABASE_MAINTENANCE_LOG")" = "compose up -d db"
test "$(sed -n '6p' "$DATABASE_MAINTENANCE_LOG")" = "migrate"

acquire_update_lock
if run_database_maintenance migrate; then
    echo "Database maintenance unexpectedly ran while the lifecycle lock was held" >&2
    exit 1
fi
test "$(sed -n '$=' "$DATABASE_MAINTENANCE_LOG")" = "6"
release_update_lock
COMPOSE_CMD="mock_compose_lifecycle"

# A stale GHCR login must not force an operator to run `docker logout`. The
# application pull retries once with a private, empty DOCKER_CONFIG, preserves
# the caller's configuration byte-for-byte, and removes the temporary directory
# on both success and failure.
PULL_DOCKER_CONFIG="$FIXTURE_ROOT/docker-config"
PULL_LOG_PATH="$FIXTURE_ROOT/image-pull.log"
PULL_TEMP_PATH="$FIXTURE_ROOT/image-pull-temp-path"
mkdir -m 700 "$PULL_DOCKER_CONFIG"
printf '%s\n' '{"auths":{"ghcr.io":{"auth":"must-remain-unchanged"}}}' \
    > "$PULL_DOCKER_CONFIG/config.json"
chmod 600 "$PULL_DOCKER_CONFIG/config.json"
export DOCKER_CONFIG="$PULL_DOCKER_CONFIG"
export DOCKER_AUTH_CONFIG='{"auths":{"ghcr.io":{"auth":"also-must-not-leak"}}}'
docker_config_before="$(sha256sum "$PULL_DOCKER_CONFIG/config.json" | cut -d' ' -f1)"

mock_compose_pull() {
    local active_config="${DOCKER_CONFIG:-}"
    printf '%s\t%s\t%s\n' \
        "$active_config" "${DOCKER_AUTH_CONFIG-unset}" "$*" >> "$PULL_LOG_PATH"
    test "$*" = "pull --policy always app" || return 96

    if [[ "$active_config" == "$PULL_DOCKER_CONFIG" ]]; then
        return 23
    fi

    test -d "$active_config" || return 94
    test "$(stat -c '%a' "$active_config")" = "700" || return 93
    test -z "$(find "$active_config" -mindepth 1 -print -quit)" || return 92
    test "${DOCKER_AUTH_CONFIG-unset}" = "unset" || return 91
    printf '%s\n' "$active_config" > "$PULL_TEMP_PATH"
    if [[ "$PULL_MOCK_RESULT" == "success" ]]; then
        return 0
    fi
    return 24
}
COMPOSE_CMD="mock_compose_pull"

PULL_MOCK_RESULT="success"
: > "$PULL_LOG_PATH"
rm -f -- "$PULL_TEMP_PATH"
pull_app_image
test "$(sed -n '$=' "$PULL_LOG_PATH")" = "2"
test "$(sed -n '1p' "$PULL_LOG_PATH" | cut -f1)" = "$PULL_DOCKER_CONFIG"
anonymous_config="$(cat "$PULL_TEMP_PATH")"
test "$anonymous_config" != "$PULL_DOCKER_CONFIG"
test ! -e "$anonymous_config"
test "$docker_config_before" = \
    "$(sha256sum "$PULL_DOCKER_CONFIG/config.json" | cut -d' ' -f1)"

PULL_MOCK_RESULT="failure"
: > "$PULL_LOG_PATH"
rm -f -- "$PULL_TEMP_PATH"
if pull_app_image; then
    echo "Two failed application image pulls were unexpectedly accepted" >&2
    exit 1
fi
test "$(sed -n '$=' "$PULL_LOG_PATH")" = "2"
anonymous_config="$(cat "$PULL_TEMP_PATH")"
test ! -e "$anonymous_config"
test "$docker_config_before" = \
    "$(sha256sum "$PULL_DOCKER_CONFIG/config.json" | cut -d' ' -f1)"
test "$DOCKER_CONFIG" = "$PULL_DOCKER_CONFIG"
test "$DOCKER_AUTH_CONFIG" = \
    '{"auths":{"ghcr.io":{"auth":"also-must-not-leak"}}}'
unset DOCKER_CONFIG DOCKER_AUTH_CONFIG
COMPOSE_CMD="mock_compose_lifecycle"

# Uninstall holds the same lock through Compose teardown and file deletion, then
# removes that installer-owned lock safely so the empty install directory can go.
read_input() {
    printf -v "$2" '%s' "yes"
}
uninstall
test ! -e "$INSTALL_DIR"
test "$(sed -n '4p' "$LIFECYCLE_COMPOSE_LOG")" = "down -v --remove-orphans"
test "$(sed -n '$=' "$LIFECYCLE_COMPOSE_LOG")" = "4"

echo "Installer fresh lifecycle and non-interactive checks passed"
