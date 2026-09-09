#!/bin/bash
set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "$SCRIPT_DIR/../.." && pwd)"
FIXTURE_DIR="$(mktemp -d /tmp/dreamtrans-admin-update.XXXXXX)"
export INSTALL_DIR="$FIXTURE_DIR"

cleanup() {
    case "$FIXTURE_DIR" in
        /tmp/dreamtrans-admin-update.*)
            rm -rf -- "$FIXTURE_DIR"
            ;;
    esac
}
trap cleanup EXIT

# Reuse the production functions without executing install.sh's main entrypoint.
# shellcheck source=/dev/null
source <(sed '$d' "$REPO_ROOT/scripts/install.sh")

compose_service_container_id() {
    printf '%s\n' "admin-test-db"
}

docker() {
    if [[ "${1:-}" != "exec" ]]; then
        echo "Unexpected docker command in admin update test: $*" >&2
        return 99
    fi

    # Consume the SQL supplied to docker exec on stdin before returning the
    # deterministic database probe result for this test case.
    cat >/dev/null
    if [[ "$MOCK_DB_QUERY_EXIT" != "0" ]]; then
        return "$MOCK_DB_QUERY_EXIT"
    fi
    printf '%s\n' "$MOCK_DB_QUERY_OUTPUT"
}

prompt_admin_credentials() {
    PROMPT_CALLS=$((PROMPT_CALLS + 1))
    if [[ "$MOCK_PROMPT_ALLOWED" != "true" ]]; then
        echo "Administrator prompt was not expected in this test case" >&2
        return 98
    fi
    ADMIN_EMAIL="prompted@example.test"
    ADMIN_PASSWORD="prompted-password-123"
}

write_admin_env() {
    local email="$1"
    local password="$2"
    printf \
        '# admin update fixture\nADMIN_EMAIL=%s\nADMIN_PASSWORD=%s\n' \
        "$email" "$password" > "$INSTALL_DIR/.env"
    chmod 600 "$INSTALL_DIR/.env"
}

reset_case() {
    ADMIN_EMAIL=""
    ADMIN_PASSWORD=""
    ADMIN_BOOTSTRAP_PENDING_THIS_RUN="false"
    ADMIN_BOOTSTRAP_SECURED_THIS_RUN="false"
    ADMIN_DISPLAY_EMAIL=""
    ADMIN_DISPLAY_PASSWORD=""
    ADMIN_CREDENTIALS_ADDED="false"
    ADMIN_PASSWORD_GENERATED="false"
    PROMPT_CALLS=0
    MOCK_DB_QUERY_EXIT=0
    MOCK_DB_QUERY_OUTPUT=1
    MOCK_PROMPT_ALLOWED=false
}

assert_admin_env_empty() {
    test -z "$(read_env_value "ADMIN_EMAIL")"
    test -z "$(read_env_value "ADMIN_PASSWORD")"
    ! grep -q '^ADMIN_EMAIL=' "$INSTALL_DIR/.env"
    ! grep -q '^ADMIN_PASSWORD=' "$INSTALL_DIR/.env"
    test -z "$ADMIN_EMAIL"
    test -z "$ADMIN_PASSWORD"
}

# bcrypt accepts at most 72 bytes. The installer restricts bootstrap passwords
# to ASCII-safe characters, so the character and byte limits are identical.
reset_case
ADMIN_EMAIL="limits@example.test"
ADMIN_PASSWORD="$(printf 'a%.0s' {1..72})"
validate_admin_credentials
ADMIN_PASSWORD="${ADMIN_PASSWORD}a"
if validate_admin_credentials >/dev/null 2>&1; then
    echo "A 73-character administrator password was unexpectedly accepted" >&2
    exit 1
fi

# Persisted bootstrap credentials are stale once an active administrator with
# a non-legacy password exists. They must not trigger another bootstrap notice
# and must not remain available to Compose or docker inspect.
reset_case
write_admin_env "existing@example.test" "stale-plaintext-password"
configure_admin_for_update
test "$PROMPT_CALLS" = "0"
test "$ADMIN_CREDENTIALS_ADDED" != "true"
assert_admin_env_empty

# An already-clean installation with a secured administrator remains a no-op.
reset_case
write_admin_env "" ""
configure_admin_for_update
test "$PROMPT_CALLS" = "0"
test "$ADMIN_CREDENTIALS_ADDED" != "true"
assert_admin_env_empty

# A complete persisted pair may represent a bootstrap that did not reach app
# readiness. Reuse it only while the database confirms no secured admin exists.
reset_case
MOCK_DB_QUERY_OUTPUT=0
write_admin_env "pending@example.test" "pending-password-123"
configure_admin_for_update
test "$PROMPT_CALLS" = "0"
test "$ADMIN_CREDENTIALS_ADDED" = "true"
test "$(read_env_value "ADMIN_EMAIL")" = "pending@example.test"
test "$(read_env_value "ADMIN_PASSWORD")" = "pending-password-123"

# With no secured admin and no pending pair, request one complete pair once.
reset_case
MOCK_DB_QUERY_OUTPUT=0
MOCK_PROMPT_ALLOWED=true
write_admin_env "" ""
configure_admin_for_update
test "$PROMPT_CALLS" = "1"
test "$ADMIN_CREDENTIALS_ADDED" = "true"
test "$(read_env_value "ADMIN_EMAIL")" = "prompted@example.test"
test "$(read_env_value "ADMIN_PASSWORD")" = "prompted-password-123"

# An incomplete stale pair must not cause a fake password reset when a secured
# administrator already exists; bootstrapAdmin deliberately never resets one.
reset_case
write_admin_env "existing@example.test" ""
configure_admin_for_update
test "$PROMPT_CALLS" = "0"
test "$ADMIN_CREDENTIALS_ADDED" != "true"
assert_admin_env_empty

# Database uncertainty is not evidence that an administrator is absent. Fail
# closed without prompting or rewriting the existing environment.
reset_case
MOCK_DB_QUERY_EXIT=42
write_admin_env "preserve@example.test" "preserve-password-123"
env_before="$(sha256sum "$INSTALL_DIR/.env" | cut -d' ' -f1)"
if configure_admin_for_update; then
    echo "Database query failure was unexpectedly accepted" >&2
    exit 1
fi
test "$PROMPT_CALLS" = "0"
test "$env_before" = "$(sha256sum "$INSTALL_DIR/.env" | cut -d' ' -f1)"

reset_case
MOCK_DB_QUERY_OUTPUT="not-a-boolean"
write_admin_env "preserve@example.test" "preserve-password-123"
env_before="$(sha256sum "$INSTALL_DIR/.env" | cut -d' ' -f1)"
if configure_admin_for_update; then
    echo "Invalid database probe output was unexpectedly accepted" >&2
    exit 1
fi
test "$PROMPT_CALLS" = "0"
test "$env_before" = "$(sha256sum "$INSTALL_DIR/.env" | cut -d' ' -f1)"

# Bootstrap credentials are one-shot inputs, so both freshly generated and
# hardened legacy Compose files must remain valid after the pair is retired.
generate_compose_file >/dev/null
grep -Fq 'ADMIN_EMAIL=${ADMIN_EMAIL:-}' "$INSTALL_DIR/docker-compose.yml"
grep -Fq 'ADMIN_PASSWORD=${ADMIN_PASSWORD:-}' "$INSTALL_DIR/docker-compose.yml"
if grep -Eq 'ADMIN_(EMAIL|PASSWORD)=\$\{ADMIN_(EMAIL|PASSWORD):\?' \
    "$INSTALL_DIR/docker-compose.yml"; then
    echo "Fresh Compose still requires retired administrator credentials" >&2
    exit 1
fi

# Exercise the upgrade path from the currently released required form. The
# Compose command is mocked here because this assertion concerns only the
# installer's deterministic rewrite.
sed -i \
    's|${ADMIN_EMAIL:-}|${ADMIN_EMAIL:?ADMIN_EMAIL must be set}|g;
     s|${ADMIN_PASSWORD:-}|${ADMIN_PASSWORD:?ADMIN_PASSWORD must be set}|g' \
    "$INSTALL_DIR/docker-compose.yml"
sed -i 's|ghcr.io/coyumelabs/dreamtrans:|ghcr.io/soaringjerry/dreamtrans:|' \
    "$INSTALL_DIR/docker-compose.yml"
set_env_value "BATCH_BILLING_RESERVATION_MINUTES" "4320"
set_env_value "ALLOW_UNMETERED_CLASSIC_TOKEN_WITH_BILLING" "true"
set_env_value "CLASSIC_TOKEN_BILLING_MINUTES" "7"
mock_compose_config() {
    test "${1:-}" = "config"
    test "${2:-}" = "--quiet"
}
COMPOSE_CMD="mock_compose_config"
harden_existing_compose
harden_existing_compose
grep -Fq 'ADMIN_EMAIL=${ADMIN_EMAIL:-}' "$INSTALL_DIR/docker-compose.yml"
grep -Fq 'ADMIN_PASSWORD=${ADMIN_PASSWORD:-}' "$INSTALL_DIR/docker-compose.yml"
if grep -Eq 'ADMIN_(EMAIL|PASSWORD)=\$\{ADMIN_(EMAIL|PASSWORD):\?' \
    "$INSTALL_DIR/docker-compose.yml"; then
    echo "Hardened Compose still requires retired administrator credentials" >&2
    exit 1
fi
test "$(read_env_value "BATCH_BILLING_RESERVATION_MINUTES")" = "4320"
test "$(read_env_value "ALLOW_UNMETERED_CLASSIC_TOKEN_WITH_BILLING")" = "true"
test "$(read_env_value "CLASSIC_TOKEN_BILLING_MINUTES")" = "7"
test "$(grep -c 'BATCH_BILLING_RESERVATION_MINUTES=' \
    "$INSTALL_DIR/docker-compose.yml")" = "1"
test "$(grep -c 'ALLOW_UNMETERED_CLASSIC_TOKEN_WITH_BILLING=' \
    "$INSTALL_DIR/docker-compose.yml")" = "1"
test "$(grep -c 'CLASSIC_TOKEN_BILLING_MINUTES=' \
    "$INSTALL_DIR/docker-compose.yml")" = "1"
# Stripe pass-throughs are added exactly once to installer-generated Compose
# files that predate online payments, and the .env gains empty placeholders.
# Images moved with the repository to the CoYumeLabs organisation.
grep -Fq 'image: ghcr.io/coyumelabs/dreamtrans:${IMAGE_TAG:-latest}' "$INSTALL_DIR/docker-compose.yml"
if grep -q 'soaringjerry' "$INSTALL_DIR/docker-compose.yml"; then
    echo "Hardened Compose still pulls from the old image owner" >&2
    exit 1
fi
for ai_key in OPENAI_MODEL OPENAI_EMBEDDING_MODEL AI_INDEX_WORKERS KNOWLEDGE_MAX_PDF_PAGES AI_PROVIDERS AI_PROVIDER_KEYS AI_PROVIDER_OPTIONS AI_EMBEDDING_PROVIDER; do
    test "$(grep -c "${ai_key}=" "$INSTALL_DIR/docker-compose.yml")" = "1"
done
for payment_key in STRIPE_SECRET_KEY STRIPE_WEBHOOK_SECRET APP_BASE_URL STRIPE_CURRENCY STRIPE_USD_EXCHANGE_RATE STRIPE_FX_MARKUP_PERCENT STRIPE_FX_RATE_URL; do
    test "$(grep -c "${payment_key}=" "$INSTALL_DIR/docker-compose.yml")" = "1"
    grep -q "^${payment_key}=" "$INSTALL_DIR/.env"
done
grep -q '^STRIPE_CURRENCY=usd$' "$INSTALL_DIR/.env"
grep -q '^STRIPE_FX_MARKUP_PERCENT=0$' "$INSTALL_DIR/.env"
# Legacy plain Postgres pins must be rewritten to the pinned pgvector image so
# migration 019 can CREATE EXTENSION vector.
test "$(grep -c 'image: postgres:16' \
    "$INSTALL_DIR/docker-compose.yml" || true)" = "0"
grep -Fq "image: ${POSTGRES_IMAGE:-pgvector/pgvector:0.8.2-pg16-bookworm}" \
    "$INSTALL_DIR/docker-compose.yml"

# The retirement seam must remove the pair before force-recreating only the app
# service, then wait for the replacement to become healthy. Display copies are
# intentionally retained so a generated password can still be shown once.
mock_compose_retire() {
    RETIRE_COMPOSE_CALLS=$((RETIRE_COMPOSE_CALLS + 1))
    RETIRE_COMPOSE_ARGS="$*"
    return "$MOCK_RETIRE_COMPOSE_EXIT"
}

wait_for_app_ready() {
    RETIRE_WAIT_CALLS=$((RETIRE_WAIT_CALLS + 1))
    if [[ "${MOCK_READY_FAIL_ON_CALL:-0}" = "$RETIRE_WAIT_CALLS" ]]; then
        return "${MOCK_READY_FAIL_EXIT:-42}"
    fi
    return "$MOCK_RETIRE_WAIT_EXIT"
}

prepare_pending_retirement() {
    reset_case
    write_admin_env "pending@example.test" "pending-password-123"
    ADMIN_EMAIL="pending@example.test"
    ADMIN_PASSWORD="pending-password-123"
    export ADMIN_EMAIL ADMIN_PASSWORD
    ADMIN_BOOTSTRAP_PENDING_THIS_RUN="true"
    ADMIN_DISPLAY_EMAIL="$ADMIN_EMAIL"
    ADMIN_DISPLAY_PASSWORD="$ADMIN_PASSWORD"
    RETIRE_COMPOSE_CALLS=0
    RETIRE_COMPOSE_ARGS=""
    RETIRE_WAIT_CALLS=0
    MOCK_RETIRE_COMPOSE_EXIT=0
    MOCK_RETIRE_WAIT_EXIT=0
    MOCK_READY_FAIL_ON_CALL=0
    MOCK_READY_FAIL_EXIT=42
    COMPOSE_CMD="mock_compose_retire"
}

prepare_pending_retirement
retire_admin_bootstrap_credentials
assert_admin_env_empty
test "$RETIRE_COMPOSE_CALLS" = "1"
test "$RETIRE_COMPOSE_ARGS" = "up -d --no-deps --force-recreate app"
test "$RETIRE_WAIT_CALLS" = "1"
test "$ADMIN_BOOTSTRAP_PENDING_THIS_RUN" = "false"
test "$ADMIN_BOOTSTRAP_SECURED_THIS_RUN" = "true"
test "$ADMIN_DISPLAY_EMAIL" = "pending@example.test"
test "$ADMIN_DISPLAY_PASSWORD" = "pending-password-123"
if env | grep -Fq 'pending-password-123'; then
    echo "Retired administrator password remained exported" >&2
    exit 1
fi

# When no bootstrap was attempted in this run, retirement is a strict no-op:
# it must not probe the database, recreate a container, or wait for readiness.
reset_case
write_admin_env "" ""
MOCK_DB_QUERY_EXIT=88
RETIRE_COMPOSE_CALLS=0
RETIRE_WAIT_CALLS=0
MOCK_RETIRE_COMPOSE_EXIT=0
MOCK_RETIRE_WAIT_EXIT=0
COMPOSE_CMD="mock_compose_retire"
retire_admin_bootstrap_credentials
test "$RETIRE_COMPOSE_CALLS" = "0"
test "$RETIRE_WAIT_CALLS" = "0"

# If recreation fails after the pair was removed, keep the pending marker true
# and record that the database already contains a secured administrator. A real
# update rollback will restore its complete legacy backup for startability.
prepare_pending_retirement
MOCK_RETIRE_COMPOSE_EXIT=41
if retire_admin_bootstrap_credentials; then
    echo "Failed credential-retirement recreation was unexpectedly accepted" >&2
    exit 1
fi
assert_admin_env_empty
test "$RETIRE_COMPOSE_CALLS" = "1"
test "$RETIRE_WAIT_CALLS" = "0"
test "$ADMIN_BOOTSTRAP_PENDING_THIS_RUN" = "true"
test "$ADMIN_BOOTSTRAP_SECURED_THIS_RUN" = "true"

# The same secured state applies when the credential-free replacement starts
# but never becomes ready.
prepare_pending_retirement
MOCK_RETIRE_WAIT_EXIT=42
if retire_admin_bootstrap_credentials; then
    echo "Failed post-retirement readiness check was unexpectedly accepted" >&2
    exit 1
fi
assert_admin_env_empty
test "$RETIRE_COMPOSE_CALLS" = "1"
test "$RETIRE_WAIT_CALLS" = "1"
test "$ADMIN_BOOTSTRAP_PENDING_THIS_RUN" = "true"
test "$ADMIN_BOOTSTRAP_SECURED_THIS_RUN" = "true"

# Exercise the real update rollback dispatcher without container side effects.
# Before the database confirms bootstrap success it restores the pending pair.
# After confirmation the rollback must preserve its backup byte-for-byte:
# released Compose files require the original pair and must remain startable.
rollback_update_files() {
    write_admin_env "$MOCK_ROLLBACK_EMAIL" "$MOCK_ROLLBACK_PASSWORD"
    printf '%s\n' "$MOCK_ROLLBACK_COMPOSE_CONTENT" \
        > "$INSTALL_DIR/docker-compose.yml"
}

restore_previous_app_image() {
    return 0
}

prepare_rollback_dispatch_case() {
    prepare_pending_retirement
    APP_DATA_PERMISSION_MIGRATION_ATTEMPTED="false"
    PREVIOUS_APP_CONTAINER_CREATED_FOR_DISCOVERY="false"
    PREVIOUS_APP_WAS_RUNNING="false"
    UPDATE_APP_RECREATE_ATTEMPTED="false"
    PREVIOUS_IMAGE_TAG_ENV=""
    MOCK_ROLLBACK_COMPOSE_CONTENT='services:
  app:
    environment:
      - ADMIN_EMAIL=${ADMIN_EMAIL:-}
      - ADMIN_PASSWORD=${ADMIN_PASSWORD:-}'
}

prepare_rollback_dispatch_case
MOCK_ROLLBACK_EMAIL=""
MOCK_ROLLBACK_PASSWORD=""
ADMIN_BOOTSTRAP_SECURED_THIS_RUN="false"
clear_admin_bootstrap_environment
rollback_update_deployment
test "$(read_env_value "ADMIN_EMAIL")" = "pending@example.test"
test "$(read_env_value "ADMIN_PASSWORD")" = "pending-password-123"
test "$(stat -c '%a' "$INSTALL_DIR/.env")" = "600"

prepare_rollback_dispatch_case
MOCK_ROLLBACK_EMAIL="stale@example.test"
MOCK_ROLLBACK_PASSWORD="stale-password-123"
MOCK_ROLLBACK_COMPOSE_CONTENT='services:
  app:
    environment:
      - ADMIN_EMAIL=${ADMIN_EMAIL:?ADMIN_EMAIL must be set}
      - ADMIN_PASSWORD=${ADMIN_PASSWORD:?ADMIN_PASSWORD must be set}'
ADMIN_BOOTSTRAP_SECURED_THIS_RUN="true"
clear_admin_bootstrap_environment
rollback_update_deployment
test "$(read_env_value "ADMIN_EMAIL")" = "stale@example.test"
test "$(read_env_value "ADMIN_PASSWORD")" = "stale-password-123"
grep -Fq 'ADMIN_EMAIL=${ADMIN_EMAIL:?ADMIN_EMAIL must be set}' \
    "$INSTALL_DIR/docker-compose.yml"
grep -Fq 'ADMIN_PASSWORD=${ADMIN_PASSWORD:?ADMIN_PASSWORD must be set}' \
    "$INSTALL_DIR/docker-compose.yml"
test "$(stat -c '%a' "$INSTALL_DIR/.env")" = "600"

# Fresh installation uses the same one-shot lifecycle. Mock its orchestration to
# prove the first healthy app receives the pair, while the second healthy app is
# force-recreated after the pair is removed.
mock_compose_fresh() {
    FRESH_COMPOSE_LOG="${FRESH_COMPOSE_LOG}|$*"
}

prepare_release_migrations() {
    FRESH_PREPARE_CALLS=$((FRESH_PREPARE_CALLS + 1))
}

init_database() {
    FRESH_MIGRATION_CALLS=$((FRESH_MIGRATION_CALLS + 1))
}

prepare_fresh_case() {
    reset_case
    write_admin_env "fresh@example.test" "fresh-password-123"
    ADMIN_EMAIL="fresh@example.test"
    ADMIN_PASSWORD="fresh-password-123"
    export ADMIN_EMAIL ADMIN_PASSWORD
    FRESH_COMPOSE_LOG=""
    FRESH_PREPARE_CALLS=0
    FRESH_MIGRATION_CALLS=0
    RETIRE_WAIT_CALLS=0
    MOCK_RETIRE_WAIT_EXIT=0
    MOCK_READY_FAIL_ON_CALL=0
    MOCK_READY_FAIL_EXIT=42
    COMPOSE_CMD="mock_compose_fresh"
}

prepare_fresh_case
start_services
test "$FRESH_PREPARE_CALLS" = "1"
test "$FRESH_MIGRATION_CALLS" = "1"
test "$RETIRE_WAIT_CALLS" = "2"
test "$FRESH_COMPOSE_LOG" = \
    "|pull db migrate|pull app|up -d db|up -d app|up -d --no-deps --force-recreate app"
test "$ADMIN_BOOTSTRAP_PENDING_THIS_RUN" = "false"
test "$ADMIN_BOOTSTRAP_SECURED_THIS_RUN" = "true"
test "$ADMIN_DISPLAY_EMAIL" = "fresh@example.test"
test "$ADMIN_DISPLAY_PASSWORD" = "fresh-password-123"
assert_admin_env_empty
if env | grep -Fq 'fresh-password-123'; then
    echo "Fresh administrator password remained exported after retirement" >&2
    exit 1
fi

# If the credential-free replacement is not healthy, fresh installation must
# fail but restore the pair and recreate a retryable app configuration.
prepare_fresh_case
MOCK_READY_FAIL_ON_CALL=2
if start_services; then
    echo "Fresh install accepted a failed credential-free replacement" >&2
    exit 1
fi
test "$RETIRE_WAIT_CALLS" = "3"
test "$FRESH_COMPOSE_LOG" = \
    "|pull db migrate|pull app|up -d db|up -d app|up -d --no-deps --force-recreate app|up -d --no-deps --force-recreate app"
test "$ADMIN_BOOTSTRAP_PENDING_THIS_RUN" = "true"
test "$ADMIN_EMAIL" = "fresh@example.test"
test "$ADMIN_PASSWORD" = "fresh-password-123"
test "$(read_env_value "ADMIN_EMAIL")" = "fresh@example.test"
test "$(read_env_value "ADMIN_PASSWORD")" = "fresh-password-123"
test "$(stat -c '%a' "$INSTALL_DIR/.env")" = "600"

echo "Installer administrator update checks passed"
