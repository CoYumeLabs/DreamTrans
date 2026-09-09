#!/bin/bash
set -Ee
umask 077

# ============================================================================
# DreamTrans One-Click Installer
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/CoYumeLabs/DreamTrans/main/scripts/install.sh | bash
#
# Or with options:
#   curl -fsSL ... | bash -s -- --port 8080
# ============================================================================

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
NC='\033[0m' # No Color

# Default values
INSTALL_DIR="${INSTALL_DIR:-$HOME/dreamtrans}"
PORT="${PORT:-16002}"
BIND_ADDRESS="${BIND_ADDRESS:-127.0.0.1}"
RAG_MAX_DB_MB="${RAG_MAX_DB_MB:-102400}"
if [[ -n "${IMAGE_TAG:-}" ]]; then
    IMAGE_TAG_EXPLICIT="true"
else
    IMAGE_TAG="latest"
    IMAGE_TAG_EXPLICIT="false"
fi
INSTALL_SENTINEL=".dreamtrans-install"
INSTALL_SENTINEL_VERSION="dreamtrans-install-v1"
INSTALL_IN_PROGRESS_MARKER=".dreamtrans-installing"
INSTALL_IN_PROGRESS_VERSION="dreamtrans-installing-v1"
APP_RUNTIME_UID="10001"
APP_RUNTIME_GID="10001"
# Migration 019 requires the pgvector and pg_trgm extension files. Pin the same
# PG16 image used by docker-compose.yml / CI; plain postgres:16* images fail
# CREATE EXTENSION vector during upgrades.
POSTGRES_IMAGE="${POSTGRES_IMAGE:-pgvector/pgvector:0.8.2-pg16-bookworm}"
SM_API_KEY="${SM_API_KEY:-}"
OPENAI_API_KEY="${OPENAI_API_KEY:-}"
OPENAI_API_BASE="${OPENAI_API_BASE:-}"
ADMIN_EMAIL="${ADMIN_EMAIL:-}"
ADMIN_PASSWORD="${ADMIN_PASSWORD:-}"
# Internal state only. Never trust an inherited environment variable here:
# doing so could make the completion message print a user-supplied password.
ADMIN_PASSWORD_GENERATED="false"

# Print banner
print_banner() {
    echo -e "${CYAN}"
    echo "╔═══════════════════════════════════════════════════════════════╗"
    echo "║                                                               ║"
    echo "║       ██████╗ ██████╗ ███████╗ █████╗ ███╗   ███╗            ║"
    echo "║       ██╔══██╗██╔══██╗██╔════╝██╔══██╗████╗ ████║            ║"
    echo "║       ██║  ██║██████╔╝█████╗  ███████║██╔████╔██║            ║"
    echo "║       ██║  ██║██╔══██╗██╔══╝  ██╔══██║██║╚██╔╝██║            ║"
    echo "║       ██████╔╝██║  ██║███████╗██║  ██║██║ ╚═╝ ██║            ║"
    echo "║       ╚═════╝ ╚═╝  ╚═╝╚══════╝╚═╝  ╚═╝╚═╝     ╚═╝            ║"
    echo "║                      TRANS                                    ║"
    echo "║                                                               ║"
    echo "║          Real-time Transcription & Translation                ║"
    echo "║                                                               ║"
    echo "╚═══════════════════════════════════════════════════════════════╝"
    echo -e "${NC}"
}

# Print info message
info() {
    echo -e "${BLUE}[INFO]${NC} $1"
}

# Print success message
success() {
    echo -e "${GREEN}[SUCCESS]${NC} $1"
}

# Print warning message
warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

# Print error message
error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# Read from terminal (works with curl pipe)
read_input() {
    local prompt="$1"
    local var_name="$2"
    local default="$3"
    local secret="$4"

    if [[ "$secret" == "true" ]]; then
        echo -ne "$prompt" >/dev/tty
        read -rs "$var_name" </dev/tty
        echo "" >/dev/tty
    else
        echo -ne "$prompt" >/dev/tty
        read -r "$var_name" </dev/tty
    fi

    # Use default if empty
    if [[ -z "${!var_name}" && -n "$default" ]]; then
        printf -v "$var_name" '%s' "$default"
    fi
}

# Check if command exists
command_exists() {
    command -v "$1" >/dev/null 2>&1
}

has_controlling_tty() {
    [[ -t 0 || -t 1 || -t 2 ]] &&
        [[ -r /dev/tty && -w /dev/tty ]] &&
        (: </dev/tty) 2>/dev/null
}

user_output() {
    if has_controlling_tty; then
        printf '%s\n' "$*" >/dev/tty
    else
        printf '%s\n' "$*"
    fi
}

normalize_install_dir() {
    if ! command_exists realpath; then
        error "realpath is required to validate --dir safely"
        return 1
    fi

    if [[ -z "$INSTALL_DIR" || "$INSTALL_DIR" == *$'\n'* || "$INSTALL_DIR" == *$'\r'* ]]; then
        error "Installation directory is empty or contains a newline"
        return 1
    fi

    local normalized_dir
    local normalized_home
    normalized_dir="$(realpath -m -- "$INSTALL_DIR")" || {
        error "Unable to normalize installation directory: $INSTALL_DIR"
        return 1
    }
    normalized_home="$(realpath -m -- "${HOME:?HOME must be set}")"

    if [[ -z "$normalized_dir" || "$normalized_dir" != /* ||
          "$normalized_dir" == "/" || "$normalized_dir" == "$normalized_home" ||
          "$normalized_dir" == *$'\n'* || "$normalized_dir" == *$'\r'* ]]; then
        error "Unsafe installation directory: $normalized_dir"
        echo "  --dir must resolve to a dedicated absolute directory, never / or HOME."
        return 1
    fi

    INSTALL_DIR="$normalized_dir"
}

validate_deployment_options() {
    if [[ ! "$PORT" =~ ^[0-9]+$ || ${#PORT} -gt 5 ]] ||
       ((10#$PORT < 1 || 10#$PORT > 65535)); then
        error "PORT must be an integer between 1 and 65535"
        return 1
    fi
    PORT="$((10#$PORT))"
    if [[ ! "$IMAGE_TAG" =~ ^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$ ]]; then
        error "IMAGE_TAG is not a valid OCI tag"
        return 1
    fi
    if [[ ! "$BIND_ADDRESS" =~ ^([A-Za-z0-9][A-Za-z0-9._-]*|\[[0-9A-Fa-f:]+\])$ ]]; then
        error "BIND_ADDRESS must be an IPv4 address, hostname, or bracketed IPv6 address"
        return 1
    fi
    if [[ ! "$RAG_MAX_DB_MB" =~ ^(-1|[0-9]+)$ ]]; then
        error "RAG_MAX_DB_MB must be a non-negative MiB value, or -1 to disable the limit"
        return 1
    fi
}

write_install_sentinel() {
    local sentinel_path="$INSTALL_DIR/$INSTALL_SENTINEL"
    local temporary_path
    if ! temporary_path="$(mktemp "$INSTALL_DIR/.dreamtrans-install.tmp.XXXXXX")"; then
        error "Unable to stage the installation marker"
        return 1
    fi
    if ! printf '%s\npath=%s\n' "$INSTALL_SENTINEL_VERSION" "$INSTALL_DIR" > "$temporary_path" ||
       ! chmod 600 "$temporary_path" ||
       ! mv -f -- "$temporary_path" "$sentinel_path"; then
        rm -f -- "$temporary_path"
        error "Unable to write the installation marker"
        return 1
    fi
}

write_install_in_progress_marker() {
    local marker_path="$INSTALL_DIR/$INSTALL_IN_PROGRESS_MARKER"
    local temporary_path
    if ! lifecycle_lock_is_held ||
       [[ "${FRESH_INSTALL_LOCK_OWNED:-false}" != "true" ]]; then
        error "Fresh-install recovery marker requires the lifecycle lock"
        return 1
    fi
    if [[ ! "${FRESH_INSTALL_ATTEMPT_ID:-}" =~ ^[0-9a-f]{32}$ ]]; then
        error "Fresh-install attempt identity is missing or invalid"
        return 1
    fi
    if [[ -e "$marker_path" || -L "$marker_path" ]]; then
        error "Refusing to overwrite an existing fresh-install recovery marker"
        return 1
    fi
    if ! temporary_path="$(mktemp "$INSTALL_DIR/.dreamtrans-installing.tmp.XXXXXX")"; then
        error "Unable to stage the fresh-install recovery marker"
        return 1
    fi
    if ! printf '%s\npath=%s\nattempt=%s\n' \
            "$INSTALL_IN_PROGRESS_VERSION" "$INSTALL_DIR" "$FRESH_INSTALL_ATTEMPT_ID" \
            > "$temporary_path" ||
       ! chmod 600 "$temporary_path" ||
       ! ln -T -- "$temporary_path" "$marker_path"; then
        rm -f -- "$temporary_path"
        error "Unable to write the fresh-install recovery marker"
        return 1
    fi
    rm -f -- "$temporary_path"
}

has_valid_install_sentinel() {
    local sentinel_path="$INSTALL_DIR/$INSTALL_SENTINEL"
    [[ -f "$sentinel_path" && ! -L "$sentinel_path" && -O "$sentinel_path" ]] || return 1
    [[ "$(sed -n '1p' "$sentinel_path")" == "$INSTALL_SENTINEL_VERSION" ]] || return 1
    [[ "$(sed -n '2p' "$sentinel_path")" == "path=$INSTALL_DIR" ]]
}

has_valid_install_in_progress_marker() {
    local marker_path="$INSTALL_DIR/$INSTALL_IN_PROGRESS_MARKER"
    local attempt_line
    [[ -f "$marker_path" && ! -L "$marker_path" && -O "$marker_path" ]] || return 1
    [[ "$(sed -n '1p' "$marker_path")" == "$INSTALL_IN_PROGRESS_VERSION" ]] || return 1
    [[ "$(sed -n '2p' "$marker_path")" == "path=$INSTALL_DIR" ]] || return 1
    attempt_line="$(sed -n '3p' "$marker_path")"
    # Two-line markers were written by the previous installer and remain
    # recoverable. New markers carry a per-attempt identity so a failing
    # concurrent process can never clean another process's transaction.
    [[ -z "$attempt_line" || "$attempt_line" =~ ^attempt=[0-9a-f]{32}$ ]]
}

fresh_install_marker_is_owned_by_attempt() {
    local expected_attempt="$1"
    has_valid_install_in_progress_marker || return 1
    [[ "$(sed -n '3p' "$INSTALL_DIR/$INSTALL_IN_PROGRESS_MARKER")" == \
       "attempt=$expected_attempt" ]]
}

lifecycle_lock_is_held() {
    local lock_path="$INSTALL_DIR/.dreamtrans-update.lock"
    local lock_path_identity
    local lock_fd_identity

    [[ -n "${UPDATE_LOCK_FD:-}" ]] || return 1
    [[ -f "$lock_path" && ! -L "$lock_path" && -O "$lock_path" ]] || return 1
    lock_path_identity="$(stat -Lc '%d:%i' -- "$lock_path" 2>/dev/null || true)"
    lock_fd_identity="$(stat -Lc '%d:%i' -- \
        "/proc/$$/fd/$UPDATE_LOCK_FD" 2>/dev/null || true)"
    [[ -n "$lock_path_identity" && "$lock_path_identity" == "$lock_fd_identity" ]]
}

looks_like_legacy_installation() {
    local env_file="$INSTALL_DIR/.env"
    local compose_file="$INSTALL_DIR/docker-compose.yml"

    [[ -d "$INSTALL_DIR" && ! -L "$INSTALL_DIR" && -O "$INSTALL_DIR" &&
       -f "$env_file" && ! -L "$env_file" && -O "$env_file" &&
       -f "$compose_file" && ! -L "$compose_file" && -O "$compose_file" ]] || return 1
    grep -q '^# Generated by install\.sh$' "$compose_file" || return 1
    grep -q '^  db:$' "$compose_file" || return 1
    grep -q '^  app:$' "$compose_file" || return 1
    grep -Eq '^[[:space:]]*image:[[:space:]]*ghcr\.io/(coyumelabs|soaringjerry)/dreamtrans:' \
        "$compose_file" || return 1
    (cd "$INSTALL_DIR" && $COMPOSE_CMD config --quiet >/dev/null 2>&1)
}

fresh_install_target_contains_only_lock() {
    [[ -d "$INSTALL_DIR" && ! -L "$INSTALL_DIR" && -O "$INSTALL_DIR" ]] || return 1
    ! find "$INSTALL_DIR" -mindepth 1 -maxdepth 1 \
        ! -name '.dreamtrans-update.lock' -print -quit | grep -q .
}

validate_fresh_install_target_for_claim() {
    [[ -e "$INSTALL_DIR" ]] || return 0
    if [[ ! -d "$INSTALL_DIR" || -L "$INSTALL_DIR" || ! -O "$INSTALL_DIR" ]]; then
        error "Installation target must be a directory owned by the current user: $INSTALL_DIR"
        return 1
    fi
    if find "$INSTALL_DIR" -mindepth 1 -maxdepth 1 -print -quit | grep -q .; then
        if has_valid_install_in_progress_marker || fresh_install_target_contains_only_lock; then
            return 0
        elif has_valid_install_sentinel; then
            error "DreamTrans is already installed at $INSTALL_DIR"
            echo "  Use --update to preserve its database and credentials, or --uninstall first."
        else
            error "Refusing to install into a non-empty directory without a valid DreamTrans marker:"
            echo "  $INSTALL_DIR"
        fi
        return 1
    fi
}

cleanup_incomplete_fresh_install() {
    local expected_attempt="${1:-}"
    if ! lifecycle_lock_is_held; then
        error "Fresh-install cleanup requires the DreamTrans lifecycle lock"
        return 1
    fi
    if ! has_valid_install_in_progress_marker; then
        error "Valid fresh-install recovery marker not found"
        return 1
    fi
    if [[ -n "$expected_attempt" ]] &&
       ! fresh_install_marker_is_owned_by_attempt "$expected_attempt"; then
        error "Refusing to clean a fresh-install transaction owned by another attempt"
        return 1
    fi

    local compose_file="$INSTALL_DIR/docker-compose.yml"
    if [[ -e "$compose_file" ]]; then
        if [[ ! -f "$compose_file" || -L "$compose_file" || ! -O "$compose_file" ]]; then
            error "Refusing to clean an unsafe partial Compose file"
            return 1
        fi
        info "Removing containers and volumes created by the incomplete fresh install..."
        if ! (
            cd "$INSTALL_DIR" &&
                $COMPOSE_CMD down -v --remove-orphans
        ); then
            error "Unable to clean the incomplete fresh-install Docker resources"
            echo "  The recovery marker and files were preserved for the next retry."
            return 1
        fi
    fi

    chmod -R u+w "$INSTALL_DIR/migrations" "$INSTALL_DIR/data" 2>/dev/null || true
    rm -f -- \
        "$INSTALL_DIR/.env" \
        "$INSTALL_DIR/docker-compose.yml" \
        "$INSTALL_DIR/migrate.sh" \
        "$INSTALL_DIR/backup.sh" \
        "$INSTALL_DIR/.backup.sh.new" \
        "$INSTALL_DIR/$INSTALL_SENTINEL"
    rm -rf --one-file-system -- \
        "$INSTALL_DIR/migrations" \
        "$INSTALL_DIR/data"
    find "$INSTALL_DIR" -mindepth 1 -maxdepth 1 \
        \( -name '.migration-assets.*' -o -name '.migrations.previous.*' -o \
           -name '.migrate.sh.new.*' -o -name '.dreamtrans-install.tmp.*' -o \
           -name '.dreamtrans-installing.tmp.*' \) \
        -exec rm -rf --one-file-system -- {} +

    if find "$INSTALL_DIR" -mindepth 1 -maxdepth 1 \
        ! -name "$INSTALL_IN_PROGRESS_MARKER" \
        ! -name '.dreamtrans-update.lock' -print -quit | grep -q .; then
        warn "Unknown files remain in the incomplete installation; they were preserved"
        echo "  Remove or move them, then rerun the installer: $INSTALL_DIR"
        return 1
    fi

    if [[ -n "$expected_attempt" ]] &&
       ! fresh_install_marker_is_owned_by_attempt "$expected_attempt"; then
        error "Fresh-install recovery marker ownership changed during cleanup"
        return 1
    fi
    rm -f -- "$INSTALL_DIR/$INSTALL_IN_PROGRESS_MARKER"
    success "Incomplete fresh installation cleaned; retry can start safely"
}

recover_incomplete_fresh_install() {
    if ! lifecycle_lock_is_held; then
        error "Fresh-install recovery requires the DreamTrans lifecycle lock"
        return 1
    fi
    if [[ -e "$INSTALL_DIR" ]] && has_valid_install_in_progress_marker; then
        # finalize_fresh_install writes the durable completion marker before
        # removing the recovery marker. If the process is interrupted between
        # those two operations, both markers are valid and startup has already
        # completed. Treat that narrow state as committed; running `down -v`
        # here would destroy a healthy fresh installation and its data.
        if has_valid_install_sentinel; then
            warn "Found a completed DreamTrans installation with a stale recovery marker"
            if ! rm -f -- "$INSTALL_DIR/$INSTALL_IN_PROGRESS_MARKER"; then
                error "Unable to clear the stale fresh-install recovery marker"
                return 1
            fi
            success "Fresh installation completion recovered safely"
            return 0
        fi
        warn "Found an incomplete DreamTrans fresh installation"
        cleanup_incomplete_fresh_install || return 1
    fi
}

begin_fresh_install_transaction() {
    FRESH_INSTALL_ATTEMPT_ID="$(random_string 32)"
    if [[ ! "$FRESH_INSTALL_ATTEMPT_ID" =~ ^[0-9a-f]{32}$ ]]; then
        error "Unable to create a fresh-install attempt identity"
        return 1
    fi
    FRESH_INSTALL_TRANSACTION_ACTIVE="true"
    FRESH_INSTALL_LOCK_OWNED="false"
    trap 'rollback_fresh_install_on_error "$?"' ERR
    trap 'rollback_fresh_install_on_error 130' INT
    trap 'rollback_fresh_install_on_error 143' TERM
}

claim_fresh_install_target() {
    validate_fresh_install_target_for_claim || return 1
    mkdir -p -- "$INSTALL_DIR" || return 1
    if [[ ! -d "$INSTALL_DIR" || -L "$INSTALL_DIR" || ! -O "$INSTALL_DIR" ]]; then
        error "Installation directory ownership changed while claiming it"
        return 1
    fi

    acquire_update_lock || return 1
    FRESH_INSTALL_LOCK_OWNED="true"
    if [[ ! -d "$INSTALL_DIR" || -L "$INSTALL_DIR" || ! -O "$INSTALL_DIR" ]]; then
        error "Installation directory changed during lifecycle lock acquisition"
        return 1
    fi
    recover_incomplete_fresh_install || return 1

    if ! fresh_install_target_contains_only_lock; then
        if has_valid_install_sentinel; then
            error "DreamTrans is already installed at $INSTALL_DIR"
            echo "  Use --update to preserve its database and credentials, or --uninstall first."
        else
            error "Refusing to install into a non-empty directory after acquiring its lifecycle lock:"
            echo "  $INSTALL_DIR"
        fi
        return 1
    fi
}

rollback_fresh_install_on_error() {
    local status="${1:-1}"
    local remove_empty_claim="false"
    trap - ERR INT TERM
    if [[ "${FRESH_INSTALL_TRANSACTION_ACTIVE:-false}" == "true" ]]; then
        warn "Fresh installation failed; cleaning only resources created by this attempt..."
        if [[ "${FRESH_INSTALL_LOCK_OWNED:-false}" == "true" &&
              -d "$INSTALL_DIR" ]] &&
           fresh_install_marker_is_owned_by_attempt \
               "${FRESH_INSTALL_ATTEMPT_ID:-invalid}"; then
            if cleanup_incomplete_fresh_install "$FRESH_INSTALL_ATTEMPT_ID"; then
                remove_empty_claim="true"
            else
                warn "Automatic fresh-install cleanup was incomplete; rerun the installer to retry cleanup"
            fi
        elif [[ "${FRESH_INSTALL_LOCK_OWNED:-false}" == "true" ]] &&
             fresh_install_target_contains_only_lock; then
            remove_empty_claim="true"
        fi
    fi
    if [[ "${FRESH_INSTALL_LOCK_OWNED:-false}" == "true" ]]; then
        if [[ "$remove_empty_claim" == "true" ]]; then
            remove_update_lock_file ||
                warn "Unable to remove the failed fresh-install lifecycle lock safely"
        fi
        release_update_lock ||
            warn "Unable to release the failed fresh-install lifecycle lock"
        FRESH_INSTALL_LOCK_OWNED="false"
        if [[ "$remove_empty_claim" == "true" ]]; then
            rmdir -- "$INSTALL_DIR" 2>/dev/null || true
        fi
    fi
    exit "$status"
}

finalize_fresh_install() {
    if ! lifecycle_lock_is_held ||
       ! fresh_install_marker_is_owned_by_attempt \
            "${FRESH_INSTALL_ATTEMPT_ID:-invalid}"; then
        error "Fresh-install transaction ownership could not be verified"
        return 1
    fi
    write_install_sentinel || return 1
    rm -f -- "$INSTALL_DIR/$INSTALL_IN_PROGRESS_MARKER" || return 1
    FRESH_INSTALL_TRANSACTION_ACTIVE="false"
    trap - ERR INT TERM
    if ! release_update_lock; then
        warn "Installation completed, but the lifecycle lock descriptor could not be released early"
    fi
    FRESH_INSTALL_LOCK_OWNED="false"
}

require_installation() {
    local allow_legacy="${1:-false}"
    if [[ ! -d "$INSTALL_DIR" || -L "$INSTALL_DIR" || ! -O "$INSTALL_DIR" ]]; then
        error "Installation not found at $INSTALL_DIR"
        return 1
    fi
    if has_valid_install_sentinel; then
        if [[ ! -f "$INSTALL_DIR/.env" || -L "$INSTALL_DIR/.env" ||
              ! -O "$INSTALL_DIR/.env" ||
              ! -f "$INSTALL_DIR/docker-compose.yml" ||
              -L "$INSTALL_DIR/docker-compose.yml" ||
              ! -O "$INSTALL_DIR/docker-compose.yml" ]]; then
            error "DreamTrans installation files are missing, symlinked, or owned by another user"
            return 1
        fi
        return 0
    fi
    if [[ "$allow_legacy" == "true" ]] && looks_like_legacy_installation; then
        warn "Adopting a verified legacy DreamTrans installation and adding its safety marker"
        write_install_sentinel
        return 0
    fi
    error "Valid DreamTrans installation marker not found at $INSTALL_DIR/$INSTALL_SENTINEL"
    echo "  Refusing to manage this directory. Run --update once to adopt a verified legacy installation."
    return 1
}

# Check prerequisites
check_prerequisites() {
    info "Checking prerequisites..."

    local required_command
    for required_command in docker realpath; do
        if ! command_exists "$required_command"; then
            error "$required_command is required"
            exit 1
        fi
    done

    # Check if docker compose is available (v2 or v1)
    if docker compose version >/dev/null 2>&1; then
        COMPOSE_CMD="docker compose"
    elif command_exists docker-compose; then
        COMPOSE_CMD="docker-compose"
    else
        error "Docker Compose is not installed. Please install Docker Compose."
        echo "  Visit: https://docs.docker.com/compose/install/"
        exit 1
    fi

    if ! docker info >/dev/null 2>&1; then
        error "Docker daemon is unavailable or the current user lacks permission"
        echo "  Start Docker and verify that 'docker info' succeeds before installing."
        exit 1
    fi

    success "Prerequisites check passed"
}

# Parse command line arguments
parse_args() {
    while [[ $# -gt 0 ]]; do
        case $1 in
            --port)
                [[ $# -ge 2 && -n "$2" ]] || { error "--port requires a value"; exit 1; }
                PORT="$2"
                shift 2
                ;;
            --dir)
                [[ $# -ge 2 && -n "$2" ]] || { error "--dir requires a value"; exit 1; }
                INSTALL_DIR="$2"
                shift 2
                ;;
            --tag)
                [[ $# -ge 2 && -n "$2" ]] || { error "--tag requires a value"; exit 1; }
                IMAGE_TAG="$2"
                IMAGE_TAG_EXPLICIT="true"
                shift 2
                ;;
            --sm-key)
                [[ $# -ge 2 && -n "$2" ]] || { error "--sm-key requires a value"; exit 1; }
                SM_API_KEY="$2"
                shift 2
                ;;
            --openai-key)
                [[ $# -ge 2 ]] || { error "--openai-key requires a value"; exit 1; }
                OPENAI_API_KEY="$2"
                shift 2
                ;;
            --admin-email)
                [[ $# -ge 2 && -n "$2" ]] || { error "--admin-email requires a value"; exit 1; }
                ADMIN_EMAIL="$2"
                shift 2
                ;;
            --admin-password)
                [[ $# -ge 2 && -n "$2" ]] || { error "--admin-password requires a value"; exit 1; }
                ADMIN_PASSWORD="$2"
                shift 2
                ;;
            --update)
                UPDATE_MODE="true"
                shift
                ;;
            --stop)
                STOP_MODE="true"
                shift
                ;;
            --start)
                START_MODE="true"
                shift
                ;;
            --restart)
                RESTART_MODE="true"
                shift
                ;;
            --status)
                STATUS_MODE="true"
                shift
                ;;
            --logs)
                LOGS_MODE="true"
                shift
                ;;
            --init-db)
                INIT_DB_MODE="true"
                shift
                ;;
            --migrate)
                MIGRATE_MODE="true"
                shift
                ;;
            --uninstall)
                UNINSTALL_MODE="true"
                shift
                ;;
            -h|--help)
                show_help
                exit 0
                ;;
            *)
                error "Unknown option: $1"
                echo "  Run with --help to see supported commands."
                exit 1
                ;;
        esac
    done
}

validate_command_selection() {
    local selected=0
    local mode
    for mode in \
        "${UPDATE_MODE:-false}" "${STOP_MODE:-false}" "${START_MODE:-false}" \
        "${RESTART_MODE:-false}" "${STATUS_MODE:-false}" "${LOGS_MODE:-false}" \
        "${INIT_DB_MODE:-false}" "${MIGRATE_MODE:-false}" "${UNINSTALL_MODE:-false}"; do
        [[ "$mode" == "true" ]] && selected=$((selected + 1))
    done
    if ((selected > 1)); then
        error "Choose only one command mode per invocation"
        return 1
    fi
}

# Show help
show_help() {
    echo "DreamTrans Installer & Manager"
    echo ""
    echo "Usage:"
    echo "  curl -fsSL https://raw.githubusercontent.com/CoYumeLabs/DreamTrans/main/scripts/install.sh | bash"
    echo "  curl -fsSL ... | bash -s -- [COMMAND] [OPTIONS]"
    echo ""
    echo "Commands:"
    echo "  (default)         Install DreamTrans (interactive)"
    echo "  --update          Pull latest image, run migrations, and restart"
    echo "  --stop            Stop services"
    echo "  --start           Start services"
    echo "  --restart         Restart services"
    echo "  --status          Show service status"
    echo "  --logs            Show logs (follow mode)"
    echo "  --init-db         Initialize database schema"
    echo "  --migrate         Run database migrations only"
    echo "  --uninstall       Remove DreamTrans and all data"
    echo ""
    echo "Options:"
    echo "  --port PORT       Set the port (default: 16002)"
    echo "  --dir DIR         Set installation directory (default: ~/dreamtrans)"
    echo "  --tag TAG         Docker image tag (default: latest)"
    echo "  --sm-key KEY      Speechmatics API key (skip prompt)"
    echo "  --openai-key KEY  OpenAI API key (skip prompt)"
    echo "  --admin-email     Bootstrap administrator email"
    echo "  --admin-password  Bootstrap password (environment variable is safer)"
    echo "  -h, --help        Show this help message"
    echo ""
    echo "Security:"
    echo "  Values passed with --sm-key, --openai-key, or --admin-password may be"
    echo "  recorded in shell history. Interactive secret prompts are safer."
    echo ""
    echo "Examples:"
    echo "  # Install"
    echo "  curl -fsSL ... | bash"
    echo ""
    echo "  # Install with custom port"
    echo "  curl -fsSL ... | bash -s -- --port 8080"
    echo ""
    echo "  # Non-interactive install"
    echo "  curl -fsSL ... | bash -s -- --sm-key YOUR_KEY --openai-key YOUR_KEY \\"
    echo "    --admin-email you@example.com --admin-password A_STRONG_PASSWORD"
    echo ""
    echo "  # Update"
    echo "  curl -fsSL ... | bash -s -- --update"
    echo ""
    echo "  # Stop/Start/Restart"
    echo "  curl -fsSL ... | bash -s -- --stop"
    echo "  curl -fsSL ... | bash -s -- --start"
    echo "  curl -fsSL ... | bash -s -- --restart"
    echo ""
    echo "  # View logs"
    echo "  curl -fsSL ... | bash -s -- --logs"
}

# Generate random string
random_string() {
    local length=${1:-32}
    local byte_count=$(((length + 1) / 2))
    local random_hex
    random_hex="$(od -An -N "$byte_count" -tx1 /dev/urandom)"
    random_hex="${random_hex//[[:space:]]/}"
    printf '%s' "${random_hex:0:length}"
}

validate_admin_credentials() {
    if [[ ! "$ADMIN_EMAIL" =~ ^[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+$ ]]; then
        error "A valid ADMIN_EMAIL is required"
        return 1
    fi
    if [[ ${#ADMIN_PASSWORD} -lt 16 || ${#ADMIN_PASSWORD} -gt 72 ]]; then
        error "ADMIN_PASSWORD must be 16-72 characters"
        return 1
    fi
    if [[ ! "$ADMIN_PASSWORD" =~ ^[A-Za-z0-9._~!@%+=:,/-]+$ ]]; then
        error "ADMIN_PASSWORD contains characters that cannot be stored safely in .env"
        echo "  Use letters, numbers, or: . _ ~ ! @ % + = : , / -"
        return 1
    fi
}

prompt_admin_credentials() {
    if [[ -z "$ADMIN_EMAIL" ]]; then
        if ! has_controlling_tty; then
            error "ADMIN_EMAIL is required when no interactive terminal is available"
            echo "  Set ADMIN_EMAIL or pass --admin-email."
            return 1
        fi
        echo "" >/dev/tty
        echo -e "${CYAN}Administrator email${NC} (required for the admin console):" >/dev/tty
        read_input "  ADMIN_EMAIL: " ADMIN_EMAIL "" false
    fi

    if [[ -z "$ADMIN_PASSWORD" ]]; then
        if ! has_controlling_tty; then
            error "ADMIN_PASSWORD is required when no interactive terminal is available"
            echo "  Set ADMIN_PASSWORD or pass --admin-password."
            return 1
        fi
        echo "" >/dev/tty
        echo -e "${CYAN}Administrator password${NC}:" >/dev/tty
        read_input "  ADMIN_PASSWORD (press Enter to generate): " ADMIN_PASSWORD "" true
        if [[ -z "$ADMIN_PASSWORD" ]]; then
            ADMIN_PASSWORD="$(random_string 32)"
            ADMIN_PASSWORD_GENERATED="true"
            user_output "  Generated password: $ADMIN_PASSWORD"
            user_output "  Store this password now; it will not be retained after bootstrap."
        fi
    fi
    validate_admin_credentials
}

set_env_value() {
    local key="$1"
    local value="$2"
    local env_file="$INSTALL_DIR/.env"

    if grep -q "^${key}=" "$env_file"; then
        sed -i "s|^${key}=.*$|${key}=${value}|" "$env_file" || return 1
    else
        printf '\n%s=%s\n' "$key" "$value" >> "$env_file" || return 1
    fi
}

unset_env_value() {
    local key="$1"
    local env_file="$INSTALL_DIR/.env"
    sed -i "/^${key}=/d" "$env_file" || return 1
}

read_env_value() {
    local key="$1"
    local env_file="$INSTALL_DIR/.env"
    grep "^${key}=" "$env_file" 2>/dev/null | tail -n 1 | cut -d= -f2-
}

sync_image_tag_for_update() {
    local persisted_tag
    persisted_tag="$(read_env_value "IMAGE_TAG")"

    if [[ "$IMAGE_TAG_EXPLICIT" == "true" ]]; then
        :
    elif [[ -n "$persisted_tag" ]]; then
        IMAGE_TAG="$persisted_tag"
    else
        # Legacy installer files embedded the selected tag directly in Compose.
        local legacy_tag
        legacy_tag="$(sed -n \
            's|^[[:space:]]*image:[[:space:]]*ghcr.io/\(coyumelabs\|soaringjerry\)/dreamtrans:\([A-Za-z0-9_.-]*\)[[:space:]]*$|\2|p' \
            "$INSTALL_DIR/docker-compose.yml" | head -n 1)"
        if [[ -n "$legacy_tag" ]]; then
            IMAGE_TAG="$legacy_tag"
        fi
    fi

    validate_deployment_options || return 1
    # Process environment overrides Compose's .env file for this update only.
    # The selected tag is persisted after migrations and readiness succeed.
    export IMAGE_TAG
}

compose_app_image_ref() {
    local compose_config
    compose_config="$($COMPOSE_CMD config)"
    printf '%s\n' "$compose_config" | awk '
        /^  app:$/ { in_app = 1; next }
        in_app && /^    image:/ {
            sub(/^[[:space:]]*image:[[:space:]]*/, "")
            print
            exit
        }
        in_app && /^  [^ ]/ { exit }
    '
}

pull_app_image_anonymously() (
    local anonymous_docker_config=""
    trap - ERR

    cleanup_anonymous_docker_config() {
        local operation_status=$?
        trap - EXIT HUP INT QUIT TERM
        if [[ -n "$anonymous_docker_config" ]] &&
           ! rm -rf -- "$anonymous_docker_config"; then
            error "Unable to remove the temporary anonymous Docker configuration"
            operation_status=1
        fi
        exit "$operation_status"
    }

    trap cleanup_anonymous_docker_config EXIT
    trap 'exit 129' HUP
    trap 'exit 130' INT
    trap 'exit 131' QUIT
    trap 'exit 143' TERM

    if ! anonymous_docker_config="$(mktemp -d)"; then
        error "Unable to create an isolated Docker configuration for anonymous pull"
        exit 1
    fi
    if ! chmod 0700 "$anonymous_docker_config"; then
        error "Unable to secure the isolated Docker configuration"
        exit 1
    fi

    # Compose also accepts credentials through DOCKER_AUTH_CONFIG. Remove that
    # inherited override inside this subshell so the retry is genuinely
    # anonymous while leaving every caller setting and Docker config untouched.
    unset DOCKER_AUTH_CONFIG
    export DOCKER_CONFIG="$anonymous_docker_config"
    $COMPOSE_CMD pull app
)

pull_app_image() {
    if $COMPOSE_CMD pull app; then
        return 0
    fi

    warn "Application image pull failed; retrying anonymously with an isolated Docker configuration..."
    if pull_app_image_anonymously; then
        success "Application image pulled without modifying the existing Docker login"
        return 0
    fi

    error "Unable to pull the DreamTrans application image"
    return 1
}

resolve_app_image_identity() {
    APP_IMAGE_REF="$(compose_app_image_ref)"
    if [[ -z "$APP_IMAGE_REF" ]]; then
        error "Unable to resolve the DreamTrans app image from Docker Compose"
        return 1
    fi

    APP_IMAGE_ID="$(docker image inspect --format '{{.Id}}' "$APP_IMAGE_REF" 2>/dev/null)" || {
        error "Pulled app image is unavailable locally: $APP_IMAGE_REF"
        return 1
    }
    if [[ ! "$APP_IMAGE_ID" =~ ^sha256:[0-9a-f]{64}$ ]]; then
        error "Docker returned an invalid immutable image ID"
        return 1
    fi

    APP_IMAGE_REVISION="$(docker image inspect \
        --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' \
        "$APP_IMAGE_ID" 2>/dev/null || true)"
    if [[ "$APP_IMAGE_REVISION" =~ ^[0-9a-fA-F]{40,64}$ ]]; then
        APP_IMAGE_REVISION="${APP_IMAGE_REVISION,,}"
        set_env_value "DREAMTRANS_IMAGE_REVISION" "$APP_IMAGE_REVISION" || return 1
        info "Release revision: $APP_IMAGE_REVISION"
    else
        APP_IMAGE_REVISION=""
        unset_env_value "DREAMTRANS_IMAGE_REVISION" || return 1
        warn "Image has no valid OCI revision label; using its immutable image ID for asset extraction"
    fi
    set_env_value "DREAMTRANS_IMAGE_ID" "$APP_IMAGE_ID" || return 1
}

extract_release_migration_assets() {
    local staging_dir
    local asset_container
    local previous_migrations=""
    if ! staging_dir="$(mktemp -d "$INSTALL_DIR/.migration-assets.XXXXXX")"; then
        error "Unable to create migration asset staging directory"
        return 1
    fi
    if ! mkdir -p "$staging_dir/migrations"; then
        rm -rf -- "$staging_dir"
        error "Unable to prepare migration asset staging directory"
        return 1
    fi

    asset_container="$(docker create "$APP_IMAGE_ID")" || {
        rm -rf -- "$staging_dir"
        error "Unable to create migration asset container from $APP_IMAGE_ID"
        return 1
    }
    if ! docker cp \
        "$asset_container:/usr/share/dreamtrans/migrations/." \
        "$staging_dir/migrations" ||
       ! docker cp \
        "$asset_container:/usr/share/dreamtrans/migrate.sh" \
        "$staging_dir/migrate.sh"; then
        docker rm -f "$asset_container" >/dev/null 2>&1 || true
        rm -rf -- "$staging_dir"
        error "The selected image does not contain its matching migration bundle"
        echo "  Refusing to download mutable migrations from a Git branch."
        return 1
    fi
    # The backup helper is optional: releases before it shipped simply lack
    # the file, and a missing helper must never block an update.
    docker cp \
        "$asset_container:/usr/share/dreamtrans/backup.sh" \
        "$staging_dir/backup.sh" >/dev/null 2>&1 || rm -f -- "$staging_dir/backup.sh"
    docker rm "$asset_container" >/dev/null 2>&1 || \
        warn "Could not remove temporary asset container $asset_container"

    if [[ -f "$staging_dir/backup.sh" && ! -L "$staging_dir/backup.sh" ]]; then
        if install -m 0555 "$staging_dir/backup.sh" "$INSTALL_DIR/.backup.sh.new" &&
           mv -f -- "$INSTALL_DIR/.backup.sh.new" "$INSTALL_DIR/backup.sh"; then
            info "Backup helper installed at $INSTALL_DIR/backup.sh (see docs/BACKUP.md)"
        else
            rm -f -- "$INSTALL_DIR/.backup.sh.new"
            warn "Could not install the backup helper; download scripts/backup.sh manually"
        fi
    fi

    if [[ ! -f "$staging_dir/migrate.sh" || -L "$staging_dir/migrate.sh" ]] ||
       find "$staging_dir/migrations" -type l -print -quit | grep -q . ||
       ! find "$staging_dir/migrations" -maxdepth 1 -type f \
            -name '[0-9][0-9][0-9]_*.sql' -print -quit | grep -q .; then
        rm -rf -- "$staging_dir"
        error "Image migration bundle is missing or unsafe"
        return 1
    fi

    # docker cp preserves image modes and umask 077 makes staging directories
    # private. Normalize explicitly so the non-root postgres migration service
    # can traverse and read every release asset.
    if ! find "$staging_dir/migrations" -type d -exec chmod 0755 {} + ||
       ! find "$staging_dir/migrations" -type f -exec chmod 0444 {} + ||
       ! chmod 0555 "$staging_dir/migrate.sh"; then
        rm -rf -- "$staging_dir"
        error "Unable to normalize migration asset permissions"
        return 1
    fi

    local pending_runner
    if ! pending_runner="$(mktemp "$INSTALL_DIR/.migrate.sh.new.XXXXXX")"; then
        rm -rf -- "$staging_dir"
        error "Unable to stage the release migration runner"
        return 1
    fi
    if ! install -m 0555 "$staging_dir/migrate.sh" "$pending_runner"; then
        rm -f -- "$pending_runner"
        rm -rf -- "$staging_dir"
        error "Unable to stage the release migration runner"
        return 1
    fi

    if [[ -e "$INSTALL_DIR/migrations" ]]; then
        if ! previous_migrations="$(mktemp -d "$INSTALL_DIR/.migrations.previous.XXXXXX")" ||
           ! rmdir -- "$previous_migrations" ||
           ! mv -- "$INSTALL_DIR/migrations" "$previous_migrations"; then
            rm -f -- "$pending_runner"
            rm -rf -- "$staging_dir"
            error "Unable to stage the previous migration bundle"
            return 1
        fi
    fi
    if ! mv -- "$staging_dir/migrations" "$INSTALL_DIR/migrations" ||
       ! mv -f -- "$pending_runner" "$INSTALL_DIR/migrate.sh"; then
        rm -f -- "$pending_runner"
        rm -rf -- "$INSTALL_DIR/migrations"
        if [[ -n "$previous_migrations" && -e "$previous_migrations" ]]; then
            mv -- "$previous_migrations" "$INSTALL_DIR/migrations" || true
        fi
        rm -rf -- "$staging_dir"
        error "Unable to install release migration bundle"
        return 1
    fi
    if [[ -n "$previous_migrations" ]]; then
        chmod -R u+w "$previous_migrations" 2>/dev/null || true
        rm -rf -- "$previous_migrations" || return 1
    fi
    rm -rf -- "$staging_dir" || return 1
    success "Migration assets extracted from immutable image $APP_IMAGE_ID"
}

acquire_update_lock() {
    local lock_path="$INSTALL_DIR/.dreamtrans-update.lock"
    local temporary_path=""
    local lock_path_identity
    local lock_fd_identity
    local lock_fd_path

    if ! command_exists flock; then
        error "flock is required to serialize DreamTrans lifecycle operations"
        return 1
    fi
    if [[ -n "${UPDATE_LOCK_FD:-}" ]]; then
        error "A DreamTrans lifecycle lock is already held by this process"
        return 1
    fi

    # Create the persistent lock without following an attacker-controlled
    # destination. A same-directory hard link is atomic and `ln -T` refuses an
    # existing file, directory, or symlink. Existing locks are opened without
    # truncation only after ownership/type checks.
    if [[ ! -e "$lock_path" && ! -L "$lock_path" ]]; then
        if ! temporary_path="$(mktemp "$INSTALL_DIR/.dreamtrans-update.lock.tmp.XXXXXX")" ||
           ! chmod 600 "$temporary_path"; then
            rm -f -- "$temporary_path"
            error "Unable to stage the DreamTrans lifecycle lock"
            return 1
        fi
        if ! ln -T -- "$temporary_path" "$lock_path" 2>/dev/null &&
           [[ ! -e "$lock_path" && ! -L "$lock_path" ]]; then
            rm -f -- "$temporary_path"
            error "Unable to create the DreamTrans lifecycle lock"
            return 1
        fi
        rm -f -- "$temporary_path"
    fi

    if [[ ! -f "$lock_path" || -L "$lock_path" || ! -O "$lock_path" ]]; then
        error "DreamTrans lifecycle lock must be a regular file owned by the current user"
        return 1
    fi
    if ! exec {UPDATE_LOCK_FD}<>"$lock_path"; then
        error "Unable to open the DreamTrans lifecycle lock"
        return 1
    fi

    lock_fd_path="/proc/$$/fd/$UPDATE_LOCK_FD"
    lock_path_identity="$(stat -Lc '%d:%i' -- "$lock_path" 2>/dev/null || true)"
    lock_fd_identity="$(stat -Lc '%d:%i' -- "$lock_fd_path" 2>/dev/null || true)"
    if [[ -z "$lock_path_identity" || "$lock_path_identity" != "$lock_fd_identity" ||
          ! -f "$lock_path" || -L "$lock_path" || ! -O "$lock_path" ]]; then
        exec {UPDATE_LOCK_FD}>&-
        UPDATE_LOCK_FD=""
        error "DreamTrans lifecycle lock changed while it was being opened"
        return 1
    fi
    if ! flock -n "$UPDATE_LOCK_FD"; then
        exec {UPDATE_LOCK_FD}>&-
        UPDATE_LOCK_FD=""
        error "Another DreamTrans lifecycle operation is already running for $INSTALL_DIR"
        return 1
    fi
    if ! chmod 600 "$lock_fd_path"; then
        flock -u "$UPDATE_LOCK_FD" >/dev/null 2>&1 || true
        exec {UPDATE_LOCK_FD}>&-
        UPDATE_LOCK_FD=""
        error "Unable to secure the DreamTrans lifecycle lock"
        return 1
    fi

    # Recheck the pathname after locking so a replacement cannot make another
    # process lock a different inode under the same installation path.
    lock_path_identity="$(stat -Lc '%d:%i' -- "$lock_path" 2>/dev/null || true)"
    if [[ "$lock_path_identity" != "$lock_fd_identity" ||
          ! -f "$lock_path" || -L "$lock_path" || ! -O "$lock_path" ]]; then
        flock -u "$UPDATE_LOCK_FD" >/dev/null 2>&1 || true
        exec {UPDATE_LOCK_FD}>&-
        UPDATE_LOCK_FD=""
        error "DreamTrans lifecycle lock path changed during acquisition"
        return 1
    fi
}

release_update_lock() {
    if [[ -n "${UPDATE_LOCK_FD:-}" ]]; then
        local release_failed="false"
        flock -u "$UPDATE_LOCK_FD" || release_failed="true"
        exec {UPDATE_LOCK_FD}>&-
        UPDATE_LOCK_FD=""
        [[ "$release_failed" != "true" ]]
    fi
}

remove_update_lock_file() {
    local lock_path="$INSTALL_DIR/.dreamtrans-update.lock"
    local lock_path_identity
    local lock_fd_identity

    if [[ -z "${UPDATE_LOCK_FD:-}" ]]; then
        error "DreamTrans lifecycle lock is not held"
        return 1
    fi
    if [[ ! -f "$lock_path" || -L "$lock_path" || ! -O "$lock_path" ]]; then
        error "Refusing to remove an unsafe DreamTrans lifecycle lock"
        return 1
    fi
    lock_path_identity="$(stat -Lc '%d:%i' -- "$lock_path" 2>/dev/null || true)"
    lock_fd_identity="$(stat -Lc '%d:%i' -- "/proc/$$/fd/$UPDATE_LOCK_FD" 2>/dev/null || true)"
    if [[ -z "$lock_path_identity" || "$lock_path_identity" != "$lock_fd_identity" ]]; then
        error "Refusing to remove a replaced DreamTrans lifecycle lock"
        return 1
    fi
    rm -f -- "$lock_path"
}

begin_update_transaction() {
    local previous_app_state
    local previous_db_state
    if ! UPDATE_BACKUP_DIR="$(mktemp -d "$INSTALL_DIR/.update-backup.XXXXXX")"; then
        error "Unable to create update backup directory"
        return 1
    fi
    UPDATE_HAD_MIGRATIONS="false"
    UPDATE_HAD_RUNNER="false"
    UPDATE_APP_RECREATE_ATTEMPTED="false"
    UPDATE_DB_RUNTIME_TOUCHED="false"
    UPDATE_DATABASE_MIGRATION_ATTEMPTED="false"
    APP_DATA_PERMISSION_MIGRATION_ATTEMPTED="false"
    APP_DATA_VOLUME_NAME=""
    APP_DATA_PREVIOUS_OWNER=""
    PREVIOUS_APP_CONTAINER_ID=""
    PREVIOUS_APP_CONTAINER_CREATED_FOR_DISCOVERY="false"
    ADMIN_BOOTSTRAP_PENDING_THIS_RUN="false"
    ADMIN_BOOTSTRAP_SECURED_THIS_RUN="false"
    ADMIN_DISPLAY_EMAIL=""
    ADMIN_DISPLAY_PASSWORD=""
    ADMIN_CREDENTIALS_ADDED="false"
    PREVIOUS_APP_IMAGE_REF=""
    PREVIOUS_APP_IMAGE_ID=""
    PREVIOUS_APP_WAS_RUNNING="false"
    PREVIOUS_DB_CONTAINER_ID=""
    PREVIOUS_DB_CONTAINER_PRESENT="false"
    PREVIOUS_DB_IMAGE_ID=""
    PREVIOUS_DB_WAS_RUNNING="false"
    PREVIOUS_IMAGE_TAG_ENV="$(read_env_value "IMAGE_TAG")"
    if ! cp -p -- "$INSTALL_DIR/.env" "$UPDATE_BACKUP_DIR/.env" ||
       ! cp -p -- "$INSTALL_DIR/docker-compose.yml" "$UPDATE_BACKUP_DIR/docker-compose.yml"; then
        rm -rf -- "$UPDATE_BACKUP_DIR"
        UPDATE_BACKUP_DIR=""
        error "Unable to create update configuration backup"
        return 1
    fi
    if [[ -d "$INSTALL_DIR/migrations" ]]; then
        if ! cp -a -- "$INSTALL_DIR/migrations" "$UPDATE_BACKUP_DIR/migrations"; then
            chmod -R u+w "$UPDATE_BACKUP_DIR" 2>/dev/null || true
            rm -rf -- "$UPDATE_BACKUP_DIR"
            UPDATE_BACKUP_DIR=""
            error "Unable to back up existing migration assets"
            return 1
        fi
        UPDATE_HAD_MIGRATIONS="true"
    fi
    if [[ -f "$INSTALL_DIR/migrate.sh" && ! -L "$INSTALL_DIR/migrate.sh" ]]; then
        if ! cp -p -- "$INSTALL_DIR/migrate.sh" "$UPDATE_BACKUP_DIR/migrate.sh"; then
            chmod -R u+w "$UPDATE_BACKUP_DIR" 2>/dev/null || true
            rm -rf -- "$UPDATE_BACKUP_DIR"
            UPDATE_BACKUP_DIR=""
            error "Unable to back up the existing migration runner"
            return 1
        fi
        UPDATE_HAD_RUNNER="true"
    fi

    # Ignore an inherited/CLI IMAGE_TAG while resolving the deployment that is
    # currently configured in its own .env file.
    PREVIOUS_APP_IMAGE_REF="$(unset IMAGE_TAG; compose_app_image_ref 2>/dev/null || true)"
    if ! PREVIOUS_APP_CONTAINER_ID="$(compose_service_container_id_any_state app)"; then
        rollback_update_files || true
        return 1
    fi
    if [[ -n "$PREVIOUS_APP_CONTAINER_ID" ]]; then
        PREVIOUS_APP_IMAGE_ID="$(docker inspect --format '{{.Image}}' \
            "$PREVIOUS_APP_CONTAINER_ID" 2>/dev/null || true)"
        previous_app_state="$(docker inspect --format '{{.State.Status}}' \
            "$PREVIOUS_APP_CONTAINER_ID" 2>/dev/null || true)"
        case "$previous_app_state" in
            running|restarting|paused)
                PREVIOUS_APP_WAS_RUNNING="true"
                ;;
        esac
    elif [[ -n "$PREVIOUS_APP_IMAGE_REF" ]]; then
        PREVIOUS_APP_IMAGE_ID="$(docker image inspect --format '{{.Id}}' \
            "$PREVIOUS_APP_IMAGE_REF" 2>/dev/null || true)"
    fi
    if [[ ! "$PREVIOUS_APP_IMAGE_ID" =~ ^sha256:[0-9a-f]{64}$ ]]; then
        PREVIOUS_APP_IMAGE_ID=""
    fi

    if ! PREVIOUS_DB_CONTAINER_ID="$(compose_service_container_id_any_state db)"; then
        rollback_update_files || true
        return 1
    fi
    if [[ -n "$PREVIOUS_DB_CONTAINER_ID" ]]; then
        PREVIOUS_DB_CONTAINER_PRESENT="true"
        PREVIOUS_DB_IMAGE_ID="$(docker inspect --format '{{.Image}}' \
            "$PREVIOUS_DB_CONTAINER_ID" 2>/dev/null || true)"
        previous_db_state="$(docker inspect --format '{{.State.Status}}' \
            "$PREVIOUS_DB_CONTAINER_ID" 2>/dev/null || true)"
        if [[ ! "$PREVIOUS_DB_IMAGE_ID" =~ ^sha256:[0-9a-f]{64}$ ]]; then
            error "Unable to record the previous database image"
            rollback_update_files || true
            return 1
        fi
        case "$previous_db_state" in
            running|restarting|paused)
                PREVIOUS_DB_WAS_RUNNING="true"
                ;;
            created|exited|dead)
                ;;
            *)
                error "Unable to record the previous database container state"
                rollback_update_files || true
                return 1
                ;;
        esac
    fi
}

ensure_app_container_for_update_discovery() {
    local discovered_image_id

    if [[ -n "${PREVIOUS_APP_CONTAINER_ID:-}" ]]; then
        return 0
    fi

    info "Creating a stopped app container to locate its existing data volume..."
    # Set this before Compose runs so rollback also cleans up a container that
    # was partially created before `up --no-start` returned an error or signal.
    PREVIOUS_APP_CONTAINER_CREATED_FOR_DISCOVERY="true"
    if ! $COMPOSE_CMD up --no-start --no-deps --no-build app; then
        error "Unable to create a stopped app container for update discovery"
        return 1
    fi
    if ! PREVIOUS_APP_CONTAINER_ID="$(compose_service_container_id_any_state app)" ||
       [[ -z "$PREVIOUS_APP_CONTAINER_ID" ]]; then
        error "Unable to resolve the stopped app container"
        return 1
    fi

    discovered_image_id="$(docker inspect --format '{{.Image}}' \
        "$PREVIOUS_APP_CONTAINER_ID" 2>/dev/null || true)"
    if [[ ! "$discovered_image_id" =~ ^sha256:[0-9a-f]{64}$ ]]; then
        error "Unable to resolve the stopped app container image"
        return 1
    fi
    if [[ -n "${PREVIOUS_APP_IMAGE_ID:-}" &&
          "$discovered_image_id" != "$PREVIOUS_APP_IMAGE_ID" ]]; then
        error "Stopped app container does not use the previously resolved image"
        return 1
    fi
    PREVIOUS_APP_IMAGE_ID="$discovered_image_id"
}

rollback_update_files() {
    trap - ERR INT TERM
    local restore_failed="false"
    if [[ -n "${UPDATE_BACKUP_DIR:-}" && -d "$UPDATE_BACKUP_DIR" ]]; then
        cp -p -- "$UPDATE_BACKUP_DIR/.env" "$INSTALL_DIR/.env" 2>/dev/null ||
            restore_failed="true"
        cp -p -- "$UPDATE_BACKUP_DIR/docker-compose.yml" \
            "$INSTALL_DIR/docker-compose.yml" 2>/dev/null || restore_failed="true"
        rm -rf -- "$INSTALL_DIR/migrations" 2>/dev/null || restore_failed="true"
        rm -f -- "$INSTALL_DIR/migrate.sh" 2>/dev/null || restore_failed="true"
        if [[ "${UPDATE_HAD_MIGRATIONS:-false}" == "true" ]]; then
            mv -- "$UPDATE_BACKUP_DIR/migrations" "$INSTALL_DIR/migrations" \
                2>/dev/null || restore_failed="true"
        fi
        if [[ "${UPDATE_HAD_RUNNER:-false}" == "true" ]]; then
            mv -- "$UPDATE_BACKUP_DIR/migrate.sh" "$INSTALL_DIR/migrate.sh" \
                2>/dev/null || restore_failed="true"
        fi
        if [[ "$restore_failed" == "true" ]]; then
            error "Automatic update rollback was incomplete"
            echo "  Recovery backup preserved at: $UPDATE_BACKUP_DIR"
            return 1
        fi
        chmod -R u+w "$UPDATE_BACKUP_DIR" 2>/dev/null || true
        rm -rf -- "$UPDATE_BACKUP_DIR" 2>/dev/null || {
            error "Restored deployment files, but could not remove backup: $UPDATE_BACKUP_DIR"
            return 1
        }
        UPDATE_BACKUP_DIR=""
    fi
}

restore_previous_app_image() {
    if [[ -z "${PREVIOUS_APP_IMAGE_ID:-}" ||
          -z "${PREVIOUS_APP_IMAGE_REF:-}" ||
          "$PREVIOUS_APP_IMAGE_REF" == *@* ||
          "$PREVIOUS_APP_IMAGE_REF" == sha256:* ]]; then
        return 0
    fi
    if ! docker image inspect "$PREVIOUS_APP_IMAGE_ID" >/dev/null 2>&1 ||
       ! docker tag "$PREVIOUS_APP_IMAGE_ID" "$PREVIOUS_APP_IMAGE_REF"; then
        error "Unable to restore the previous app image tag"
        return 1
    fi
}

restore_previous_app_data_permissions() {
    local owners

    if [[ "${APP_DATA_PERMISSION_MIGRATION_ATTEMPTED:-false}" != "true" ]]; then
        return 0
    fi
    if [[ ! "${APP_DATA_PREVIOUS_OWNER:-}" =~ ^[0-9]+:[0-9]+$ ]]; then
        error "Previous application data owner was not recorded"
        return 1
    fi

    info "Restoring previous application data permissions..."
    if ! run_app_data_permission_helper "
        chown -R \"$APP_DATA_PREVIOUS_OWNER\" /app/data
    "; then
        error "Unable to restore /app/data ownership to $APP_DATA_PREVIOUS_OWNER"
        return 1
    fi
    if ! owners="$(run_app_data_permission_helper '
        find /app/data -xdev -exec stat -c "%u:%g" {} + | sort -u
    ')" || [[ "$owners" != "$APP_DATA_PREVIOUS_OWNER" ]]; then
        error "Previous application data ownership could not be verified"
        return 1
    fi
    APP_DATA_PERMISSION_MIGRATION_ATTEMPTED="false"
    success "Previous application data permissions restored"
}

restore_previous_db_runtime_state() {
    local restored_db_container_id
    local restored_db_image_id
    local restored_db_state

    if [[ "${UPDATE_DB_RUNTIME_TOUCHED:-false}" != "true" ]]; then
        return 0
    fi

    info "Restoring the previous database container runtime state..."
    if [[ "${PREVIOUS_DB_CONTAINER_PRESENT:-false}" != "true" ]]; then
        if ! $COMPOSE_CMD rm -f -s db >/dev/null 2>&1; then
            error "Temporary database container could not be removed"
            return 1
        fi
        if ! restored_db_container_id="$(compose_service_container_id_any_state db)" ||
           [[ -n "$restored_db_container_id" ]]; then
            error "Database container existed after restoring its previous absent state"
            return 1
        fi
        success "Previous absent database container state restored"
        return 0
    fi

    if [[ "${PREVIOUS_DB_WAS_RUNNING:-false}" == "true" ]]; then
        if ! $COMPOSE_CMD up -d db >/dev/null; then
            error "Previous running database container could not be restored"
            return 1
        fi
        wait_for_db || return 1
    else
        if ! restored_db_container_id="$(compose_service_container_id_any_state db)"; then
            error "Unable to inspect the database container during rollback"
            return 1
        fi
        if [[ -z "$restored_db_container_id" ]]; then
            if ! $COMPOSE_CMD up --no-start --no-deps db >/dev/null; then
                error "Previous stopped database container could not be recreated"
                return 1
            fi
        elif ! $COMPOSE_CMD stop db >/dev/null; then
            error "Previous stopped database container could not be stopped"
            return 1
        fi
    fi

    if ! restored_db_container_id="$(compose_service_container_id_any_state db)" ||
       [[ -z "$restored_db_container_id" ]]; then
        error "Restored database container could not be resolved"
        return 1
    fi
    restored_db_image_id="$(docker inspect --format '{{.Image}}' \
        "$restored_db_container_id" 2>/dev/null || true)"
    restored_db_state="$(docker inspect --format '{{.State.Status}}' \
        "$restored_db_container_id" 2>/dev/null || true)"
    if [[ "$restored_db_image_id" != "${PREVIOUS_DB_IMAGE_ID:-}" ]]; then
        error "Restored database container does not use the previous image"
        return 1
    fi
    if [[ "${PREVIOUS_DB_WAS_RUNNING:-false}" == "true" ]]; then
        case "$restored_db_state" in
            running|restarting|paused)
                ;;
            *)
                error "Previous running database state was not restored"
                return 1
                ;;
        esac
    else
        case "$restored_db_state" in
            running|restarting|paused|"")
                error "Previous stopped database state was not restored"
                return 1
                ;;
        esac
    fi
    success "Previous database container runtime state restored"
}

rollback_update_deployment() {
    local rollback_failed="false"
    local files_restored="true"
    local restored_container_id
    local restored_image_id
    local restored_state
    if ! rollback_update_files; then
        files_restored="false"
        rollback_failed="true"
    fi
    if [[ "$files_restored" == "true" &&
          "${ADMIN_BOOTSTRAP_PENDING_THIS_RUN:-false}" == "true" &&
          "${ADMIN_BOOTSTRAP_SECURED_THIS_RUN:-false}" != "true" ]]; then
        retain_pending_admin_credentials_after_rollback || rollback_failed="true"
    fi
    if [[ -n "${PREVIOUS_IMAGE_TAG_ENV:-}" ]]; then
        IMAGE_TAG="$PREVIOUS_IMAGE_TAG_ENV"
        export IMAGE_TAG
    else
        unset IMAGE_TAG
    fi
    restore_previous_app_image || rollback_failed="true"

    if [[ "${APP_DATA_PERMISSION_MIGRATION_ATTEMPTED:-false}" == "true" ]]; then
        if ! $COMPOSE_CMD stop app >/dev/null 2>&1; then
            error "Application could not be stopped before restoring data permissions"
            rollback_failed="true"
        elif ! restore_previous_app_data_permissions; then
            rollback_failed="true"
        fi
    fi

    if [[ "${PREVIOUS_APP_CONTAINER_CREATED_FOR_DISCOVERY:-false}" == "true" &&
          "${PREVIOUS_APP_WAS_RUNNING:-false}" != "true" ]]; then
        if ! $COMPOSE_CMD rm -f -s app >/dev/null 2>&1; then
            error "Temporary stopped app container could not be removed"
            rollback_failed="true"
        fi
    fi

    if [[ "${UPDATE_APP_RECREATE_ATTEMPTED:-false}" == "true" &&
          "${PREVIOUS_APP_CONTAINER_CREATED_FOR_DISCOVERY:-false}" != "true" ]]; then
        if [[ -z "${PREVIOUS_APP_IMAGE_ID:-}" ]]; then
            error "Previous application image is unavailable for rollback"
            rollback_failed="true"
        elif [[ "$rollback_failed" == "false" ]]; then
            APP_IMAGE_ID="$PREVIOUS_APP_IMAGE_ID"
            if [[ "${PREVIOUS_APP_WAS_RUNNING:-false}" == "true" ]]; then
                if ! $COMPOSE_CMD up -d --force-recreate app; then
                    error "Previous running application could not be restored automatically"
                    rollback_failed="true"
                fi
            elif ! $COMPOSE_CMD up --no-start --no-deps --force-recreate app; then
                error "Previous stopped application could not be restored automatically"
                rollback_failed="true"
            fi

            if [[ "$rollback_failed" == "false" ]]; then
                if ! restored_container_id="$(compose_service_container_id_any_state app)" ||
                   [[ -z "$restored_container_id" ]]; then
                    error "Restored application container could not be resolved"
                    rollback_failed="true"
                else
                    restored_image_id="$(docker inspect --format '{{.Image}}' \
                        "$restored_container_id" 2>/dev/null || true)"
                    restored_state="$(docker inspect --format '{{.State.Status}}' \
                        "$restored_container_id" 2>/dev/null || true)"
                    if [[ "$restored_image_id" != "$PREVIOUS_APP_IMAGE_ID" ]]; then
                        error "Restored application container does not use the previous image"
                        rollback_failed="true"
                    elif [[ "${PREVIOUS_APP_WAS_RUNNING:-false}" != "true" ]]; then
                        case "$restored_state" in
                            running|restarting|paused|"")
                                error "Previous stopped application was not restored in a stopped state"
                                rollback_failed="true"
                                ;;
                        esac
                    fi
                fi
            fi
        fi
    fi

    restore_previous_db_runtime_state || rollback_failed="true"
    release_update_lock || rollback_failed="true"

    if [[ "$rollback_failed" == "true" ]]; then
        error "Update rollback needs manual recovery"
        return 1
    fi
    warn "Update failed; restored the previous image, configuration, migration assets, and container runtime state"
    if [[ "${UPDATE_DATABASE_MIGRATION_ATTEMPTED:-false}" == "true" ]]; then
        warn "Database schema migrations are forward-only; any committed versions were not reverted"
        echo "  Release migrations must remain backward-compatible with the previous app image."
    fi
}

rollback_update_on_error() {
    local status="${1:-1}"
    rollback_update_deployment || true
    exit "$status"
}

commit_update_transaction() {
    chmod -R u+w "$UPDATE_BACKUP_DIR" 2>/dev/null || true
    rm -rf -- "$UPDATE_BACKUP_DIR" || return 1
    UPDATE_BACKUP_DIR=""
}

prepare_release_migrations() {
    resolve_app_image_identity || return 1
    extract_release_migration_assets || return 1
}

ensure_jwt_secrets_for_update() {
    local env_file="$INSTALL_DIR/.env"
    if [[ ! -f "$env_file" ]]; then
        error "Environment file not found at $env_file"
        return 1
    fi
    local access_secret
    local refresh_secret
    access_secret="$(read_env_value "JWT_SECRET")"
    refresh_secret="$(read_env_value "JWT_REFRESH_SECRET")"

    if [[ ${#access_secret} -lt 32 ]]; then
        access_secret="$(random_string 64)"
        set_env_value "JWT_SECRET" "$access_secret" || return 1
        warn "Generated a new JWT_SECRET; existing access tokens will be invalidated"
    fi
    if [[ ${#refresh_secret} -lt 32 || "$refresh_secret" == "$access_secret" ]]; then
        refresh_secret="$(random_string 64)"
        set_env_value "JWT_REFRESH_SECRET" "$refresh_secret" || return 1
        warn "Generated a new independent JWT_REFRESH_SECRET; existing refresh tokens will be invalidated"
    fi
    chmod 600 "$env_file" || return 1
}

probe_secured_admin_for_update() {
    local db_container_id
    local query_output

    SECURED_ADMIN_PRESENT=""
    db_container_id="$(compose_service_container_id db)"
    if [[ -z "$db_container_id" ]]; then
        error "Unable to resolve the database container for this Compose project"
        return 1
    fi
    if ! query_output="$(docker exec -i "$db_container_id" /bin/sh -c '
        exec psql -v ON_ERROR_STOP=1 \
            -U "${POSTGRES_USER:-dreamtrans}" \
            -d "${POSTGRES_DB:-dreamtrans}" -At
    ' <<'SQL'
SELECT CASE WHEN EXISTS (
    SELECT 1 FROM users
    WHERE role = 'super_admin'
      AND is_active = true
      AND password_hash <> '$2a$10$DEoAtxRrvaAbHFrSSgw3uu.rhEuoc3UJr2ctVDEooZv96sRC.7Eie'
      AND password_hash <> 'disabled-insecure-default-account'
) THEN 1 ELSE 0 END;
SQL
)"; then
        error "Unable to verify the existing DreamTrans administrator"
        return 1
    fi
    query_output="$(printf '%s' "$query_output" | tr -d '[:space:]')"
    case "$query_output" in
        0|1)
            SECURED_ADMIN_PRESENT="$query_output"
            ;;
        *)
            error "Database returned an invalid administrator status"
            return 1
            ;;
    esac
}

clear_admin_bootstrap_environment() {
    unset_env_value "ADMIN_EMAIL" || return 1
    unset_env_value "ADMIN_PASSWORD" || return 1
    ADMIN_EMAIL=""
    ADMIN_PASSWORD=""
    chmod 600 "$INSTALL_DIR/.env" || return 1
}

configure_admin_for_update() {
    local env_file="$INSTALL_DIR/.env"
    local persisted_email
    local persisted_password

    if [[ ! -f "$env_file" ]]; then
        error "Environment file not found at $env_file"
        return 1
    fi

    # The database is authoritative. Bootstrap variables are one-shot inputs,
    # not the stored administrator account, and must never reset an existing
    # active super administrator.
    probe_secured_admin_for_update || return 1
    if [[ "$SECURED_ADMIN_PRESENT" == "1" ]]; then
        ADMIN_BOOTSTRAP_SECURED_THIS_RUN="true"
        persisted_email="$(read_env_value "ADMIN_EMAIL")"
        persisted_password="$(read_env_value "ADMIN_PASSWORD")"
        if [[ -n "$persisted_email" || -n "$persisted_password" ||
              -n "${ADMIN_EMAIL:-}" || -n "${ADMIN_PASSWORD:-}" ]]; then
            info "Existing secured administrator retained; removing stale bootstrap credentials"
        fi
        clear_admin_bootstrap_environment || return 1
        return 0
    fi

    persisted_email="$(read_env_value "ADMIN_EMAIL")"
    persisted_password="$(read_env_value "ADMIN_PASSWORD")"
    if [[ -z "$ADMIN_EMAIL" ]]; then
        ADMIN_EMAIL="$persisted_email"
    fi
    if [[ -z "$ADMIN_PASSWORD" ]]; then
        ADMIN_PASSWORD="$persisted_password"
    fi

    if [[ -z "$ADMIN_EMAIL" || -z "$ADMIN_PASSWORD" ]]; then
        prompt_admin_credentials || return 1
    else
        validate_admin_credentials || return 1
    fi

    set_env_value "ADMIN_EMAIL" "$ADMIN_EMAIL" || return 1
    set_env_value "ADMIN_PASSWORD" "$ADMIN_PASSWORD" || return 1
    chmod 600 "$env_file" || return 1
    ADMIN_BOOTSTRAP_PENDING_THIS_RUN="true"
    ADMIN_DISPLAY_EMAIL="$ADMIN_EMAIL"
    ADMIN_DISPLAY_PASSWORD="$ADMIN_PASSWORD"
    ADMIN_CREDENTIALS_ADDED="true"
}

retain_pending_admin_credentials_after_rollback() {
    if [[ "${ADMIN_BOOTSTRAP_PENDING_THIS_RUN:-false}" != "true" ]]; then
        return 0
    fi
    if [[ -z "${ADMIN_DISPLAY_EMAIL:-}" || -z "${ADMIN_DISPLAY_PASSWORD:-}" ]]; then
        error "Pending administrator credentials are unavailable for update retry"
        return 1
    fi
    set_env_value "ADMIN_EMAIL" "$ADMIN_DISPLAY_EMAIL" || return 1
    set_env_value "ADMIN_PASSWORD" "$ADMIN_DISPLAY_PASSWORD" || return 1
    chmod 600 "$INSTALL_DIR/.env" || return 1
    warn "Bootstrap administrator credentials were retained for the next update attempt"
}

retire_admin_bootstrap_credentials() {
    if [[ "${ADMIN_BOOTSTRAP_PENDING_THIS_RUN:-false}" != "true" ]]; then
        return 0
    fi

    probe_secured_admin_for_update || return 1
    if [[ "$SECURED_ADMIN_PRESENT" != "1" ]]; then
        error "Bootstrap administrator was not created by the updated application"
        return 1
    fi
    ADMIN_BOOTSTRAP_SECURED_THIS_RUN="true"

    info "Removing one-shot administrator credentials from the deployment..."
    clear_admin_bootstrap_environment || return 1
    if ! $COMPOSE_CMD up -d --no-deps --force-recreate app; then
        error "Unable to recreate the application without bootstrap credentials"
        return 1
    fi
    wait_for_app_ready || return 1
    ADMIN_BOOTSTRAP_PENDING_THIS_RUN="false"
    success "Bootstrap administrator credentials retired"
}

restore_fresh_admin_bootstrap_credentials() {
    if [[ -z "${ADMIN_DISPLAY_EMAIL:-}" || -z "${ADMIN_DISPLAY_PASSWORD:-}" ]]; then
        return 1
    fi
    ADMIN_EMAIL="$ADMIN_DISPLAY_EMAIL"
    ADMIN_PASSWORD="$ADMIN_DISPLAY_PASSWORD"
    set_env_value "ADMIN_EMAIL" "$ADMIN_EMAIL" || return 1
    set_env_value "ADMIN_PASSWORD" "$ADMIN_PASSWORD" || return 1
    chmod 600 "$INSTALL_DIR/.env" || return 1
    $COMPOSE_CMD up -d --no-deps --force-recreate app || return 1
    wait_for_app_ready
}

harden_existing_compose() {
    local compose_file="$INSTALL_DIR/docker-compose.yml"
    local env_file="$INSTALL_DIR/.env"
    local published_port

    published_port="$(sed -nE \
        's/^[[:space:]]*-[[:space:]]*"([^"]*:)?([0-9]+):8080"[[:space:]]*$/\2/p' \
        "$compose_file" | head -n 1)"
    if [[ -n "$published_port" ]]; then
        set_env_value "PORT" "$published_port" || return 1
    fi

    # Preserve existing values while migrating legacy variable names.
    sed -i 's/^OPENAI_BASE=/OPENAI_API_BASE=/' "$env_file" || return 1
    sed -i "/^version: ['\"]3\.[0-9]['\"]$/d" "$compose_file" || return 1
    sed -i 's|OPENAI_BASE|OPENAI_API_BASE|g' "$compose_file" || return 1
    # The repository moved to the CoYumeLabs organisation; images publish
    # under the new owner and the old package no longer receives updates.
    sed -i 's|ghcr.io/soaringjerry/dreamtrans:|ghcr.io/coyumelabs/dreamtrans:|g' \
        "$compose_file" || return 1
    sed -i 's|^\([[:space:]]*image:[[:space:]]*ghcr.io/coyumelabs/dreamtrans:\).*$|\1${IMAGE_TAG:-latest}|' \
        "$compose_file" || return 1
    # Existing one-click installs used plain postgres:16 images. Migration 019
    # needs pgvector binaries in the database container filesystem; the data
    # volume stays PG16-compatible across this image swap.
    if grep -Eq '^[[:space:]]*image:[[:space:]]*(postgres:16([^[:alnum:]_.-]|$)|postgres:16-alpine|pgvector/pgvector:)' \
        "$compose_file"; then
        sed -i "s|^[[:space:]]*image:[[:space:]]*postgres:16-alpine[[:space:]]*$|    image: ${POSTGRES_IMAGE}|g" \
            "$compose_file" || return 1
        sed -i "s|^[[:space:]]*image:[[:space:]]*postgres:16[[:space:]]*$|    image: ${POSTGRES_IMAGE}|g" \
            "$compose_file" || return 1
        # Keep already-pgvector installs on the pinned release tag when they
        # still track an older 0.8.x bookworm pin from a previous installer.
        sed -i "s|^[[:space:]]*image:[[:space:]]*pgvector/pgvector:[^[:space:]]*[[:space:]]*$|    image: ${POSTGRES_IMAGE}|g" \
            "$compose_file" || return 1
    fi
    if ! grep -q '^BIND_ADDRESS=' "$env_file"; then
        set_env_value "BIND_ADDRESS" "127.0.0.1" || return 1
    fi
    if ! grep -q 'BIND_ADDRESS.*:.*:8080' "$compose_file"; then
        sed -i 's|^\([[:space:]]*- "\)\([0-9][0-9]*:8080"\)$|\1${BIND_ADDRESS:-127.0.0.1}:\2|' \
            "$compose_file" || return 1
    fi

    # Remove known insecure fallbacks from installer-generated compose files.
    sed -i 's|${POSTGRES_PASSWORD:-dreamtrans}|${POSTGRES_PASSWORD:?POSTGRES_PASSWORD must be set}|g' \
        "$compose_file" || return 1
    sed -i 's|${SM_API_KEY}|${SM_API_KEY:?SM_API_KEY must be set}|g' \
        "$compose_file" || return 1
    sed -i 's|^[[:space:]]*- DATABASE_URL=.*|      - DATABASE_URL=postgres://${POSTGRES_USER:-dreamtrans}:${POSTGRES_PASSWORD:?POSTGRES_PASSWORD must be set}@db:5432/${POSTGRES_DB:-dreamtrans}?sslmode=disable|' \
        "$compose_file" || return 1
    sed -i 's|${JWT_SECRET}|${JWT_SECRET:?JWT_SECRET must be set}|g' \
        "$compose_file" || return 1
    sed -i 's|${JWT_REFRESH_SECRET:-}|${JWT_REFRESH_SECRET:?JWT_REFRESH_SECRET must be set}|g' \
        "$compose_file" || return 1
    sed -i 's|${ADMIN_EMAIL:?ADMIN_EMAIL must be set}|${ADMIN_EMAIL:-}|g' \
        "$compose_file" || return 1
    sed -i 's|${ADMIN_PASSWORD:?ADMIN_PASSWORD must be set}|${ADMIN_PASSWORD:-}|g' \
        "$compose_file" || return 1

    if ! grep -q 'ADMIN_EMAIL=' "$compose_file"; then
        if ! grep -q 'JWT_REFRESH_SECRET=' "$compose_file"; then
            error "Cannot safely add bootstrap administrator variables to $compose_file"
            return 1
        fi
        sed -i '/JWT_REFRESH_SECRET=/a\
      - ADMIN_EMAIL=${ADMIN_EMAIL:-}\
      - ADMIN_PASSWORD=${ADMIN_PASSWORD:-}' "$compose_file" || return 1
    fi

    if ! grep -q '^SM_API_KEY_NO_TRAINING=' "$env_file"; then
        set_env_value "SM_API_KEY_NO_TRAINING" "" || return 1
    fi
    if ! grep -q '^BATCH_BILLING_RESERVATION_MINUTES=' "$env_file"; then
        set_env_value "BATCH_BILLING_RESERVATION_MINUTES" "10080" || return 1
    fi
    if ! grep -q '^ALLOW_UNMETERED_CLASSIC_TOKEN_WITH_BILLING=' "$env_file"; then
        set_env_value "ALLOW_UNMETERED_CLASSIC_TOKEN_WITH_BILLING" "false" || return 1
    fi
    if ! grep -q '^CLASSIC_TOKEN_BILLING_MINUTES=' "$env_file"; then
        set_env_value "CLASSIC_TOKEN_BILLING_MINUTES" "10" || return 1
    fi
    if ! grep -q 'BATCH_BILLING_RESERVATION_MINUTES=' "$compose_file"; then
        sed -i '/SM_API_KEY=/a\
      - BATCH_BILLING_RESERVATION_MINUTES=${BATCH_BILLING_RESERVATION_MINUTES:-10080}' \
            "$compose_file" || return 1
    fi
    # The optional no-training key shipped after the billing safety variables.
    if ! grep -q 'SM_API_KEY_NO_TRAINING=' "$compose_file"; then
        sed -i '/- SM_API_KEY=/a\
      - SM_API_KEY_NO_TRAINING=${SM_API_KEY_NO_TRAINING:-}' \
            "$compose_file" || return 1
    fi
    if ! grep -q 'ALLOW_UNMETERED_CLASSIC_TOKEN_WITH_BILLING=' "$compose_file"; then
        sed -i '/BATCH_BILLING_RESERVATION_MINUTES=/a\
      - ALLOW_UNMETERED_CLASSIC_TOKEN_WITH_BILLING=${ALLOW_UNMETERED_CLASSIC_TOKEN_WITH_BILLING:-false}' \
            "$compose_file" || return 1
    fi
    if ! grep -q 'CLASSIC_TOKEN_BILLING_MINUTES=' "$compose_file"; then
        sed -i '/ALLOW_UNMETERED_CLASSIC_TOKEN_WITH_BILLING=/a\
      - CLASSIC_TOKEN_BILLING_MINUTES=${CLASSIC_TOKEN_BILLING_MINUTES:-10}' \
            "$compose_file" || return 1
    fi

    if ! grep -q 'DREAMTRANS_API_KEY=' "$compose_file"; then
        sed -i '/ADMIN_PASSWORD=/a\
      - DREAMTRANS_API_KEY=${DREAMTRANS_API_KEY:-}\
      - DREAMTRANS_ADMIN_API_KEY=${DREAMTRANS_ADMIN_API_KEY:-}\
      - ALLOW_ANONYMOUS_API=${ALLOW_ANONYMOUS_API:-false}\
      - ALLOW_WEBSOCKET_QUERY_TOKEN=${ALLOW_WEBSOCKET_QUERY_TOKEN:-false}\
      - API_RATE_LIMIT_PER_MINUTE=${API_RATE_LIMIT_PER_MINUTE:-120}\
      - WEBSOCKET_MAX_CONNECTIONS=${WEBSOCKET_MAX_CONNECTIONS:-256}\
      - WEBSOCKET_MAX_CONNECTIONS_PER_PRINCIPAL=${WEBSOCKET_MAX_CONNECTIONS_PER_PRINCIPAL:-4}\
      - REGISTRATION_ENABLED=${REGISTRATION_ENABLED:-false}\
      - REGISTRATION_INVITE_CODE=${REGISTRATION_INVITE_CODE:-}\
      - CORS_ALLOWED_ORIGINS=${CORS_ALLOWED_ORIGINS:-}' "$compose_file" || return 1
    fi

    if ! grep -q 'SIGNUP_RISK_SECRET=' "$compose_file"; then
        sed -i '/REGISTRATION_INVITE_CODE=/a\
      - SIGNUP_RISK_SECRET=${SIGNUP_RISK_SECRET:-}' "$compose_file" || return 1
    fi
    if ! grep -q 'TRUSTED_PROXY_CIDRS=' "$compose_file"; then
        sed -i '/REGISTRATION_INVITE_CODE=/a\
      - TRUSTED_PROXY_CIDRS=${TRUSTED_PROXY_CIDRS:-}' "$compose_file" || return 1
    fi

    # Email verification and registration abuse controls shipped after the
    # one-click installer; pass them through so SMTP settings reach the app.
    for mail_key in SIGNUP_RISK_SECRET TRUSTED_PROXY_CIDRS RESEND_API_KEY MAIL_FROM SMTP_HOST SMTP_PORT SMTP_USERNAME SMTP_PASSWORD SMTP_FROM SMTP_TLS EMAIL_VERIFICATION_REQUIRED REGISTRATION_ALLOWED_EMAIL_DOMAINS REGISTRATION_BLOCKED_EMAIL_DOMAINS; do
        if ! grep -q "^${mail_key}=" "$env_file"; then
            set_env_value "$mail_key" "" || return 1
        fi
    done
    if ! grep -q "^REGISTRATION_RATE_LIMIT_PER_HOUR=" "$env_file"; then
        set_env_value "REGISTRATION_RATE_LIMIT_PER_HOUR" "5" || return 1
    fi
    if ! grep -q 'SMTP_HOST=' "$compose_file"; then
        if ! grep -q 'REGISTRATION_INVITE_CODE=' "$compose_file"; then
            error "Cannot safely add mail variables to $compose_file"
            return 1
        fi
        sed -i '/REGISTRATION_INVITE_CODE=/a\
      - REGISTRATION_RATE_LIMIT_PER_HOUR=${REGISTRATION_RATE_LIMIT_PER_HOUR:-5}\
      - REGISTRATION_ALLOWED_EMAIL_DOMAINS=${REGISTRATION_ALLOWED_EMAIL_DOMAINS:-}\
      - REGISTRATION_BLOCKED_EMAIL_DOMAINS=${REGISTRATION_BLOCKED_EMAIL_DOMAINS:-}\
      - EMAIL_VERIFICATION_REQUIRED=${EMAIL_VERIFICATION_REQUIRED:-}\
      - RESEND_API_KEY=${RESEND_API_KEY:-}\
      - MAIL_FROM=${MAIL_FROM:-}\
      - SMTP_HOST=${SMTP_HOST:-}\
      - SMTP_PORT=${SMTP_PORT:-}\
      - SMTP_USERNAME=${SMTP_USERNAME:-}\
      - SMTP_PASSWORD=${SMTP_PASSWORD:-}\
      - SMTP_FROM=${SMTP_FROM:-}\
      - SMTP_TLS=${SMTP_TLS:-}' "$compose_file" || return 1
    fi

    if ! grep -q 'ALLOW_WEBSOCKET_QUERY_TOKEN=' "$compose_file"; then
        sed -i '/ALLOW_ANONYMOUS_API=/a\
      - ALLOW_WEBSOCKET_QUERY_TOKEN=${ALLOW_WEBSOCKET_QUERY_TOKEN:-false}' \
            "$compose_file" || return 1
    fi
    if ! grep -q 'WEBSOCKET_MAX_CONNECTIONS=' "$compose_file"; then
        sed -i '/API_RATE_LIMIT_PER_MINUTE=/a\
      - WEBSOCKET_MAX_CONNECTIONS=${WEBSOCKET_MAX_CONNECTIONS:-256}\
      - WEBSOCKET_MAX_CONNECTIONS_PER_PRINCIPAL=${WEBSOCKET_MAX_CONNECTIONS_PER_PRINCIPAL:-4}' \
            "$compose_file" || return 1
    fi
    if ! grep -q 'WEBSOCKET_MAX_CONNECTIONS_PER_PRINCIPAL=' "$compose_file"; then
        sed -i '/WEBSOCKET_MAX_CONNECTIONS=/a\
      - WEBSOCKET_MAX_CONNECTIONS_PER_PRINCIPAL=${WEBSOCKET_MAX_CONNECTIONS_PER_PRINCIPAL:-4}' \
            "$compose_file" || return 1
    fi

    if ! grep -q '^RAG_MAX_DB_MB=' "$env_file"; then
        set_env_value "RAG_MAX_DB_MB" "$RAG_MAX_DB_MB" || return 1
    fi
    if ! grep -q 'RAG_MAX_DB_MB=' "$compose_file"; then
        if grep -q 'ALLOW_USER_API_KEY=' "$compose_file"; then
            sed -i '/ALLOW_USER_API_KEY=/a\
      - RAG_MAX_DB_MB=${RAG_MAX_DB_MB:-102400}' "$compose_file" || return 1
        elif grep -q 'CORS_ALLOWED_ORIGINS=' "$compose_file"; then
            sed -i '/CORS_ALLOWED_ORIGINS=/a\
      - RAG_MAX_DB_MB=${RAG_MAX_DB_MB:-102400}' "$compose_file" || return 1
        else
            error "Cannot safely add RAG_MAX_DB_MB to $compose_file"
            return 1
        fi
    fi

    # Additional AI providers were introduced after the single-endpoint
    # installs; forward their variables so .env values reach the app.
    if ! grep -q 'AI_PROVIDERS=' "$compose_file"; then
        if ! grep -q 'OPENAI_API_BASE=' "$compose_file"; then
            error "Cannot safely add AI provider variables to $compose_file"
            return 1
        fi
        sed -i '/OPENAI_API_BASE=/a\
      - AI_PROVIDERS=${AI_PROVIDERS:-}\
      - AI_PROVIDER_KEYS=${AI_PROVIDER_KEYS:-}\
      - AI_PROVIDER_OPTIONS=${AI_PROVIDER_OPTIONS:-}\
      - AI_EMBEDDING_PROVIDER=${AI_EMBEDDING_PROVIDER:-}' "$compose_file" || return 1
    fi

    # AI tuning and knowledge-extraction limits are optional overrides with
    # code defaults; forward them so .env values actually reach the app.
    if ! grep -q 'OPENAI_MODEL=' "$compose_file"; then
        if ! grep -q 'OPENAI_API_BASE=' "$compose_file"; then
            error "Cannot safely add AI configuration variables to $compose_file"
            return 1
        fi
        sed -i '/OPENAI_API_BASE=/a\
      - OPENAI_MODEL=${OPENAI_MODEL:-gpt-5.6-sol}\
      - OPENAI_EMBEDDING_MODEL=${OPENAI_EMBEDDING_MODEL:-text-embedding-3-small}\
      - OPENAI_USE_RESPONSES=${OPENAI_USE_RESPONSES:-}\
      - OPENAI_PROMPT_CACHE=${OPENAI_PROMPT_CACHE:-}\
      - OPENAI_PROMPT_CACHE_TTL=${OPENAI_PROMPT_CACHE_TTL:-1800}\
      - AI_MAX_CONTEXT_TOKENS=${AI_MAX_CONTEXT_TOKENS:-256000}\
      - AI_CONTEXT_OUTPUT_RESERVE_TOKENS=${AI_CONTEXT_OUTPUT_RESERVE_TOKENS:-4096}\
      - AI_MODEL_CONTEXT_WINDOW_TOKENS=${AI_MODEL_CONTEXT_WINDOW_TOKENS:-260096}\
      - AI_INDEX_WORKERS=${AI_INDEX_WORKERS:-2}\
      - KNOWLEDGE_DATA_PATH=/app/data/knowledge\
      - KNOWLEDGE_MAX_FILE_MB=${KNOWLEDGE_MAX_FILE_MB:-50}\
      - KNOWLEDGE_MAX_EXTRACTED_MB=${KNOWLEDGE_MAX_EXTRACTED_MB:-10}\
      - KNOWLEDGE_MAX_OFFICE_UNCOMPRESSED_MB=${KNOWLEDGE_MAX_OFFICE_UNCOMPRESSED_MB:-100}\
      - KNOWLEDGE_MAX_IMAGE_MEGAPIXELS=${KNOWLEDGE_MAX_IMAGE_MEGAPIXELS:-40}\
      - KNOWLEDGE_MAX_PDF_PAGES=${KNOWLEDGE_MAX_PDF_PAGES:-100}\
      - KNOWLEDGE_EXTRACT_WORKERS=${KNOWLEDGE_EXTRACT_WORKERS:-2}' "$compose_file" || return 1
    fi

    # Backups shipped after the one-click installer: give an existing .env the
    # same commented block a fresh install gets, so the operator sees where to
    # fill in the R2 settings.
    ensure_backup_env_block "$env_file" || return 1

    # Stripe payments shipped after the one-click installer; without these
    # pass-throughs a configured STRIPE_SECRET_KEY never reaches the app and
    # checkout stays disabled.
    for payment_key in STRIPE_SECRET_KEY STRIPE_WEBHOOK_SECRET APP_BASE_URL STRIPE_USD_EXCHANGE_RATE STRIPE_FX_RATE_URL; do
        if ! grep -q "^${payment_key}=" "$env_file"; then
            set_env_value "$payment_key" "" || return 1
        fi
    done
    if ! grep -q "^STRIPE_CURRENCY=" "$env_file"; then
        set_env_value "STRIPE_CURRENCY" "usd" || return 1
    fi
    if ! grep -q "^STRIPE_FX_MARKUP_PERCENT=" "$env_file"; then
        set_env_value "STRIPE_FX_MARKUP_PERCENT" "0" || return 1
    fi
    if ! grep -q 'STRIPE_SECRET_KEY=' "$compose_file"; then
        if ! grep -q 'RAG_MAX_DB_MB=' "$compose_file"; then
            error "Cannot safely add Stripe variables to $compose_file"
            return 1
        fi
        sed -i '/RAG_MAX_DB_MB=/a\
      - STRIPE_SECRET_KEY=${STRIPE_SECRET_KEY:-}\
      - STRIPE_WEBHOOK_SECRET=${STRIPE_WEBHOOK_SECRET:-}\
      - APP_BASE_URL=${APP_BASE_URL:-}' "$compose_file" || return 1
    fi
    # Checkout currency shipped after the Stripe pass-throughs.
    if ! grep -q 'STRIPE_CURRENCY=' "$compose_file"; then
        if ! grep -q 'APP_BASE_URL=' "$compose_file"; then
            error "Cannot safely add Stripe currency variables to $compose_file"
            return 1
        fi
        sed -i '/APP_BASE_URL=/a\
      - STRIPE_CURRENCY=${STRIPE_CURRENCY:-usd}\
      - STRIPE_USD_EXCHANGE_RATE=${STRIPE_USD_EXCHANGE_RATE:-}' "$compose_file" || return 1
    fi
    if ! grep -q 'STRIPE_FX_MARKUP_PERCENT=' "$compose_file"; then
        sed -i '/STRIPE_USD_EXCHANGE_RATE=/a\
      - STRIPE_FX_MARKUP_PERCENT=${STRIPE_FX_MARKUP_PERCENT:-0}\
      - STRIPE_FX_RATE_URL=${STRIPE_FX_RATE_URL:-}' "$compose_file" || return 1
    fi

    # Installer releases before the migration runner relied on
    # docker-entrypoint-initdb.d, which does nothing for an existing volume.
    if ! grep -q '^  migrate:' "$compose_file"; then
        sed -i "/^  app:/i\\
  migrate:\\
    image: ${POSTGRES_IMAGE}\\
    user: postgres\\
    read_only: true\\
    cap_drop:\\
      - ALL\\
    security_opt:\\
      - no-new-privileges:true\\
    environment:\\
      PGHOST: db\\
      PGPORT: \"5432\"\\
      PGDATABASE: \${POSTGRES_DB:-dreamtrans}\\
      PGUSER: \${POSTGRES_USER:-dreamtrans}\\
      PGPASSWORD: \${POSTGRES_PASSWORD:?POSTGRES_PASSWORD must be set}\\
      MIGRATIONS_DIR: /migrations\\
    volumes:\\
      - ./migrations:/migrations:ro\\
      - ./migrate.sh:/migration-tools/migrate.sh:ro\\
    entrypoint:\\
      - /bin/sh\\
      - /migration-tools/migrate.sh\\
    depends_on:\\
      db:\\
        condition: service_healthy\\
    restart: \"no\"\\
" "$compose_file" || return 1
    fi
    if ! grep -q 'condition: service_completed_successfully' "$compose_file"; then
        sed -i '/^  app:/,/^volumes:/ {
            /condition: service_healthy/a\
      migrate:\
        condition: service_completed_successfully
        }' "$compose_file" || return 1
    fi
    if ! sed -n '/^  app:/,/^volumes:/p' "$compose_file" | grep -q '^    healthcheck:'; then
        sed -i '/^  app:/,/^volumes:/ {
            /^    volumes:/i\
    healthcheck:\
      test: ["CMD", "wget", "-q", "-O", "/dev/null", "http://127.0.0.1:8080/readyz"]\
      interval: 10s\
      timeout: 3s\
      start_period: 10s\
      retries: 12
        }' "$compose_file" || return 1
    fi
    if ! sed -n '/^  app:/,/^volumes:/p' "$compose_file" | grep -q '^    stop_grace_period:'; then
        sed -i '/^  app:/,/^volumes:/ {
            /^    volumes:/i\
    stop_grace_period: 30s
        }' "$compose_file" || return 1
    fi

    if ! $COMPOSE_CMD config --quiet; then
        error "Hardened Docker Compose configuration is invalid"
        return 1
    fi
}

run_app_data_permission_helper() {
    local helper_script="$1"
    local helper_image="${APP_IMAGE_ID:-}"

    if [[ ! "$helper_image" =~ ^sha256:[0-9a-f]{64}$ ||
          -z "${APP_DATA_VOLUME_NAME:-}" ]]; then
        error "Immutable app image or application data volume is unresolved"
        return 1
    fi

    docker run --rm \
        --network none \
        --read-only \
        --user "0:0" \
        --cap-drop ALL \
        --cap-add CHOWN \
        --cap-add DAC_OVERRIDE \
        --cap-add DAC_READ_SEARCH \
        --cap-add FOWNER \
        --security-opt no-new-privileges:true \
        --volume "$APP_DATA_VOLUME_NAME:/app/data" \
        --entrypoint /bin/sh \
        "$helper_image" -ec "$helper_script"
}

repair_app_data_permissions_for_update() {
    local compose_file="$INSTALL_DIR/docker-compose.yml"
    local app_service
    local app_data_mount_count
    local managed_app_data_mount_count
    local mount_record
    local mount_type
    local compose_project
    local volume_project
    local volume_key
    local owners
    local owner_count

    # Legacy images used a different numeric runtime identity. Named-volume
    # ownership survives an image upgrade, so the fixed UID used by current
    # releases cannot read legacy 0600 files until the volume is migrated.
    #
    # Only operate on the exact named-volume mount generated by this installer.
    # Refuse custom/bind-mounted layouts instead of recursively changing an
    # arbitrary host path through /app/data.
    app_service="$(sed -n '/^  app:$/,/^[^[:space:]]/p' "$compose_file")"
    app_data_mount_count="$(
        printf '%s\n' "$app_service" |
            grep -Ec '^[[:space:]]*-[[:space:]]*[^[:space:]#]+:/app/data(:[^[:space:]]+)?[[:space:]]*$' ||
            true
    )"
    managed_app_data_mount_count="$(
        printf '%s\n' "$app_service" |
            grep -Ec '^[[:space:]]*-[[:space:]]*appdata:/app/data[[:space:]]*$' ||
            true
    )"
    if [[ "$app_data_mount_count" != "1" ||
          "$managed_app_data_mount_count" != "1" ]]; then
        error "Cannot safely migrate application data permissions"
        echo "  Expected exactly one installer-managed appdata:/app/data volume."
        echo "  Custom volume layouts must be migrated manually to UID:GID"
        echo "  $APP_RUNTIME_UID:$APP_RUNTIME_GID."
        return 1
    fi

    if [[ -z "${PREVIOUS_APP_CONTAINER_ID:-}" ]]; then
        error "Cannot safely locate the existing application's data volume"
        echo "  The previous app container is missing. Recreate or start the"
        echo "  existing installation before updating."
        return 1
    fi
    mount_record="$(docker inspect --format \
        '{{range .Mounts}}{{if eq .Destination "/app/data"}}{{printf "%s|%s\n" .Type .Name}}{{end}}{{end}}' \
        "$PREVIOUS_APP_CONTAINER_ID" 2>/dev/null || true)"
    if [[ "$mount_record" == *$'\n'* ]]; then
        error "Multiple /app/data mounts were found on the existing app container"
        return 1
    fi
    mount_type="${mount_record%%|*}"
    APP_DATA_VOLUME_NAME="${mount_record#*|}"
    if [[ "$mount_type" != "volume" ||
          ! "$APP_DATA_VOLUME_NAME" =~ ^[A-Za-z0-9][A-Za-z0-9_.-]*$ ]] ||
       [[ "$(docker volume inspect --format '{{.Name}}' \
            "$APP_DATA_VOLUME_NAME" 2>/dev/null || true)" != "$APP_DATA_VOLUME_NAME" ]]; then
        error "The existing /app/data mount is not a valid Docker named volume"
        return 1
    fi
    compose_project="$(docker inspect --format \
        '{{index .Config.Labels "com.docker.compose.project"}}' \
        "$PREVIOUS_APP_CONTAINER_ID" 2>/dev/null || true)"
    volume_project="$(docker volume inspect --format \
        '{{index .Labels "com.docker.compose.project"}}' \
        "$APP_DATA_VOLUME_NAME" 2>/dev/null || true)"
    volume_key="$(docker volume inspect --format \
        '{{index .Labels "com.docker.compose.volume"}}' \
        "$APP_DATA_VOLUME_NAME" 2>/dev/null || true)"
    if [[ -z "$compose_project" || "$volume_project" != "$compose_project" ||
          "$volume_key" != "appdata" ]]; then
        error "The /app/data volume is not owned by this Compose installation"
        return 1
    fi

    UPDATE_APP_RECREATE_ATTEMPTED="true"
    info "Stopping the previous application before data migration..."
    $COMPOSE_CMD stop app || {
        error "Unable to stop the previous application safely"
        return 1
    }

    if ! owners="$(run_app_data_permission_helper '
        find /app/data -xdev -exec stat -c "%u:%g" {} + | sort -u
    ')"; then
        error "Unable to inspect existing /app/data ownership"
        return 1
    fi
    owner_count="$(
        printf '%s\n' "$owners" | grep -Ec '^[0-9]+:[0-9]+$' || true
    )"
    if [[ "$owner_count" != "1" || "$owners" == *$'\n'* ]]; then
        error "Application data has mixed or invalid ownership"
        echo "  Refusing a recursive migration that could not be rolled back exactly."
        printf '  Owners found: %s\n' "${owners//$'\n'/, }"
        return 1
    fi
    if [[ "${APP_DATA_PERMISSION_MIGRATION_ATTEMPTED:-false}" == "true" ]]; then
        if [[ ! "${APP_DATA_PREVIOUS_OWNER:-}" =~ ^[0-9]+:[0-9]+$ ||
              "$owners" != "$APP_RUNTIME_UID:$APP_RUNTIME_GID" ]]; then
            error "Application data migration state changed unexpectedly"
            return 1
        fi
        info "Application data permissions are already migrated for this update"
        return 0
    fi
    APP_DATA_PREVIOUS_OWNER="$owners"
    if [[ "$APP_DATA_PREVIOUS_OWNER" == "$APP_RUNTIME_UID:$APP_RUNTIME_GID" ]]; then
        info "Application data permissions already match the selected release"
        return 0
    fi

    info "Migrating application data permissions..."
    APP_DATA_PERMISSION_MIGRATION_ATTEMPTED="true"
    if ! run_app_data_permission_helper "
        test \"\$(id -u dreamtrans)\" = \"$APP_RUNTIME_UID\"
        test \"\$(id -g dreamtrans)\" = \"$APP_RUNTIME_GID\"
        chown -R \"$APP_RUNTIME_UID:$APP_RUNTIME_GID\" /app/data
    "; then
        error "Unable to migrate /app/data to UID:GID $APP_RUNTIME_UID:$APP_RUNTIME_GID"
        return 1
    fi
    if ! owners="$(run_app_data_permission_helper '
        find /app/data -xdev -exec stat -c "%u:%g" {} + | sort -u
    ')" || [[ "$owners" != "$APP_RUNTIME_UID:$APP_RUNTIME_GID" ]]; then
        error "Application data ownership verification failed after migration"
        return 1
    fi
    success "Application data permissions migrated"
}

# Prompt for API keys
prompt_api_keys() {
    info "Please provide your API keys:"
    if has_controlling_tty; then
        echo "" >/dev/tty
    fi

    # Speechmatics API Key
    if [[ -z "$SM_API_KEY" ]]; then
        if ! has_controlling_tty; then
            error "SM_API_KEY is required when no interactive terminal is available"
            echo "  Set SM_API_KEY or pass --sm-key."
            return 1
        fi
        echo -e "${CYAN}Speechmatics API Key${NC} (required for transcription):" >/dev/tty
        echo "  Get yours at: https://www.speechmatics.com/" >/dev/tty
        read_input "  SM_API_KEY: " SM_API_KEY "" true
        if [[ -z "$SM_API_KEY" ]]; then
            error "Speechmatics API Key is required!"
            return 1
        fi
    fi

    # OpenAI API Key
    if [[ -z "$OPENAI_API_KEY" ]]; then
        if has_controlling_tty; then
            echo "" >/dev/tty
            echo -e "${CYAN}OpenAI API Key${NC} (optional, for translation/chat):" >/dev/tty
            echo "  Get yours at: https://platform.openai.com/api-keys" >/dev/tty
            read_input "  OPENAI_API_KEY (press Enter to skip): " OPENAI_API_KEY "" true
        else
            OPENAI_API_KEY=""
        fi
    fi

    # OpenAI Base URL
    if [[ -z "$OPENAI_API_BASE" ]]; then
        if has_controlling_tty; then
            echo "" >/dev/tty
            echo -e "${CYAN}OpenAI Base URL${NC} (optional, for custom endpoints):" >/dev/tty
            read_input "  OPENAI_API_BASE (press Enter for default): " \
                OPENAI_API_BASE "https://api.openai.com/v1" false
        else
            OPENAI_API_BASE="https://api.openai.com/v1"
        fi
    fi

    # Bootstrap administrator. There is deliberately no known default account.
    prompt_admin_credentials || return 1

    local secret_value
    for secret_value in "$SM_API_KEY" "${OPENAI_API_KEY:-}" "${OPENAI_API_BASE:-}"; do
        if [[ "$secret_value" == *$'\n'* || "$secret_value" == *$'\r'* ]]; then
            error "API configuration values must not contain newlines"
            return 1
        fi
    done
}

# Create directory structure
create_directories() {
    info "Creating directory structure..."
    if ! lifecycle_lock_is_held ||
       [[ "${FRESH_INSTALL_LOCK_OWNED:-false}" != "true" ]] ||
       ! fresh_install_target_contains_only_lock; then
        error "Fresh installation target is not exclusively claimed by this process"
        return 1
    fi
    write_install_in_progress_marker || return 1
    mkdir -p "$INSTALL_DIR/data" "$INSTALL_DIR/migrations" || return 1
    success "Directories created at $INSTALL_DIR"
}

# Generate .env file
generate_env_file() {
    info "Generating environment configuration..."

    local jwt_secret
    local jwt_refresh_secret
    local pg_password
    jwt_secret="$(random_string 64)"
    jwt_refresh_secret="$(random_string 64)"
    pg_password="$(random_string 32)"

    cat > "$INSTALL_DIR/.env" << EOF
# DreamTrans Configuration
# Generated on $(date)

# === Required ===
SM_API_KEY=${SM_API_KEY}
# Optional second Speechmatics key from an account with Model Training OFF.
# Setting it offers users the training programme: audio goes through
# SM_API_KEY (training on) only for users who join, in exchange for a
# transcription discount; everyone else uses this key. Leave empty to keep
# routing everything through SM_API_KEY.
SM_API_KEY_NO_TRAINING=

# === Speechmatics billing safety ===
BATCH_BILLING_RESERVATION_MINUTES=10080
ALLOW_UNMETERED_CLASSIC_TOKEN_WITH_BILLING=false
CLASSIC_TOKEN_BILLING_MINUTES=10

# === OpenAI (optional) ===
OPENAI_API_KEY=${OPENAI_API_KEY:-}
OPENAI_API_BASE=${OPENAI_API_BASE:-https://api.openai.com/v1}
# Additional OpenAI-compatible providers (optional). Their models appear in the
# console as "name::model". Example:
#   AI_PROVIDERS=cerebras=https://api.cerebras.ai/v1
#   AI_PROVIDER_KEYS=cerebras=csk-...
#   AI_PROVIDER_OPTIONS=cerebras=chat
#   AI_EMBEDDING_PROVIDER=openai-compatible
AI_PROVIDERS=${AI_PROVIDERS:-}
AI_PROVIDER_KEYS=${AI_PROVIDER_KEYS:-}
AI_PROVIDER_OPTIONS=${AI_PROVIDER_OPTIONS:-}
AI_EMBEDDING_PROVIDER=${AI_EMBEDDING_PROVIDER:-}
# Optional overrides (code defaults apply when unset):
#   OPENAI_MODEL, OPENAI_EMBEDDING_MODEL, OPENAI_USE_RESPONSES,
#   OPENAI_PROMPT_CACHE, OPENAI_PROMPT_CACHE_TTL, AI_MAX_CONTEXT_TOKENS,
#   AI_CONTEXT_OUTPUT_RESERVE_TOKENS, AI_MODEL_CONTEXT_WINDOW_TOKENS,
#   AI_INDEX_WORKERS, KNOWLEDGE_MAX_FILE_MB, KNOWLEDGE_MAX_EXTRACTED_MB,
#   KNOWLEDGE_MAX_OFFICE_UNCOMPRESSED_MB, KNOWLEDGE_MAX_IMAGE_MEGAPIXELS,
#   KNOWLEDGE_MAX_PDF_PAGES, KNOWLEDGE_EXTRACT_WORKERS

# === Server ===
PORT=${PORT}
BIND_ADDRESS=${BIND_ADDRESS}
IMAGE_TAG=${IMAGE_TAG}

# === PostgreSQL ===
POSTGRES_DB=dreamtrans
POSTGRES_USER=dreamtrans
POSTGRES_PASSWORD=${pg_password}

# === JWT Authentication ===
JWT_SECRET=${jwt_secret}
JWT_REFRESH_SECRET=${jwt_refresh_secret}

# === Backups (optional, see docs/BACKUP.md) ===
# Fill these in and run scripts/backup.sh --install-cron for daily encrypted
# database backups to Cloudflare R2.
# R2_ACCOUNT_ID=
# R2_ACCESS_KEY_ID=
# R2_SECRET_ACCESS_KEY=
# R2_BUCKET=
# BACKUP_PASSPHRASE=
# BACKUP_RETENTION_DAYS=30
# BACKUP_HEALTHCHECK_URL=

# === Bootstrap Administrator ===
ADMIN_EMAIL=${ADMIN_EMAIL}
ADMIN_PASSWORD=${ADMIN_PASSWORD}

# === Authentication and Registration ===
DREAMTRANS_API_KEY=
DREAMTRANS_ADMIN_API_KEY=
ALLOW_ANONYMOUS_API=false
ALLOW_WEBSOCKET_QUERY_TOKEN=false
API_RATE_LIMIT_PER_MINUTE=120
WEBSOCKET_MAX_CONNECTIONS=256
WEBSOCKET_MAX_CONNECTIONS_PER_PRINCIPAL=4
REGISTRATION_ENABLED=false
REGISTRATION_INVITE_CODE=
SIGNUP_RISK_SECRET=
TRUSTED_PROXY_CIDRS=
# Sign-ups and verification resends allowed per source address per hour
REGISTRATION_RATE_LIMIT_PER_HOUR=5
# Comma separated. Allowed list (when set) restricts sign-ups to those
# domains and their subdomains; blocked list extends the built-in
# disposable-mailbox list.
REGISTRATION_ALLOWED_EMAIL_DOMAINS=
REGISTRATION_BLOCKED_EMAIL_DOMAINS=
CORS_ALLOWED_ORIGINS=

# === Email (verification links) ===
# New sign-ups must click an emailed link before they can log in and receive
# trial credit. With REGISTRATION_ENABLED=true you must configure one of the
# transports below, or sign-ups are refused (EMAIL_VERIFICATION_REQUIRED=false
# disables verification; not recommended for public deployments).
# Transport A: Resend HTTP API (key from https://resend.com/api-keys)
RESEND_API_KEY=
# Sender address for either transport, e.g. "DreamTrans <no-reply@yourdomain.com>"
MAIL_FROM=
# Transport B: SMTP relay. SMTP_HOST=log prints the mail to the container log.
SMTP_HOST=
SMTP_PORT=
SMTP_USERNAME=
SMTP_PASSWORD=
# starttls (default, 587) | tls (465) | none
SMTP_TLS=
# Default true; only set to false to allow sign-up without a mail transport
EMAIL_VERIFICATION_REQUIRED=

# === System Settings ===
ALLOW_USER_API_KEY=false
RAG_MAX_DB_MB=${RAG_MAX_DB_MB}

# === Stripe payments (optional) ===
# Leave empty to run without online top-ups and membership checkout.
# Webhook endpoint: POST /api/billing/stripe/webhook
STRIPE_SECRET_KEY=
STRIPE_WEBHOOK_SECRET=
# Public site URL Stripe redirects back to after checkout
APP_BASE_URL=
# Currency Stripe charges in (ISO 4217). The ledger stays in USD; set the rate
# as units of that currency per 1 USD (required when not usd).
STRIPE_CURRENCY=usd
# A fixed number, or "auto" to fetch the ECB reference rate hourly
STRIPE_USD_EXCHANGE_RATE=
# Auto mode: safety margin (percent) on top of the reference rate
STRIPE_FX_MARKUP_PERCENT=0
STRIPE_FX_RATE_URL=
EOF

    chmod 600 "$INSTALL_DIR/.env"
    success "Environment file created"
}

# Generate docker-compose.yml
generate_compose_file() {
    info "Generating Docker Compose configuration..."

    cat > "$INSTALL_DIR/docker-compose.yml" << EOF
# DreamTrans Docker Compose
# Generated by install.sh
#
# Access:
#   - Workspace:          http://localhost:${PORT}
#   - Authenticated entry: http://localhost:${PORT}/pro
#

services:
  db:
    image: ${POSTGRES_IMAGE}
    restart: unless-stopped
    environment:
      POSTGRES_USER: \${POSTGRES_USER:-dreamtrans}
      POSTGRES_PASSWORD: \${POSTGRES_PASSWORD:?POSTGRES_PASSWORD must be set}
      POSTGRES_DB: \${POSTGRES_DB:-dreamtrans}
    volumes:
      - pgdata:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U \${POSTGRES_USER:-dreamtrans} -d \${POSTGRES_DB:-dreamtrans}"]
      interval: 5s
      timeout: 5s
      retries: 5

  migrate:
    image: ${POSTGRES_IMAGE}
    user: postgres
    read_only: true
    cap_drop:
      - ALL
    security_opt:
      - no-new-privileges:true
    environment:
      PGHOST: db
      PGPORT: "5432"
      PGDATABASE: \${POSTGRES_DB:-dreamtrans}
      PGUSER: \${POSTGRES_USER:-dreamtrans}
      PGPASSWORD: \${POSTGRES_PASSWORD:?POSTGRES_PASSWORD must be set}
      MIGRATIONS_DIR: /migrations
    volumes:
      - ./migrations:/migrations:ro
      - ./migrate.sh:/migration-tools/migrate.sh:ro
    entrypoint:
      - /bin/sh
      - /migration-tools/migrate.sh
    depends_on:
      db:
        condition: service_healthy
    restart: "no"

  app:
    image: ghcr.io/coyumelabs/dreamtrans:\${IMAGE_TAG:-latest}
    restart: unless-stopped
    ports:
      - "\${BIND_ADDRESS:-127.0.0.1}:\${PORT:-16002}:8080"
    environment:
      - SM_API_KEY=\${SM_API_KEY:?SM_API_KEY must be set}
      - SM_API_KEY_NO_TRAINING=\${SM_API_KEY_NO_TRAINING:-}
      - BATCH_BILLING_RESERVATION_MINUTES=\${BATCH_BILLING_RESERVATION_MINUTES:-10080}
      - ALLOW_UNMETERED_CLASSIC_TOKEN_WITH_BILLING=\${ALLOW_UNMETERED_CLASSIC_TOKEN_WITH_BILLING:-false}
      - CLASSIC_TOKEN_BILLING_MINUTES=\${CLASSIC_TOKEN_BILLING_MINUTES:-10}
      - OPENAI_API_KEY=\${OPENAI_API_KEY:-}
      - OPENAI_API_BASE=\${OPENAI_API_BASE:-https://api.openai.com/v1}
      - AI_PROVIDERS=\${AI_PROVIDERS:-}
      - AI_PROVIDER_KEYS=\${AI_PROVIDER_KEYS:-}
      - AI_PROVIDER_OPTIONS=\${AI_PROVIDER_OPTIONS:-}
      - AI_EMBEDDING_PROVIDER=\${AI_EMBEDDING_PROVIDER:-}
      - OPENAI_MODEL=\${OPENAI_MODEL:-gpt-5.6-sol}
      - OPENAI_EMBEDDING_MODEL=\${OPENAI_EMBEDDING_MODEL:-text-embedding-3-small}
      - OPENAI_USE_RESPONSES=\${OPENAI_USE_RESPONSES:-}
      - OPENAI_PROMPT_CACHE=\${OPENAI_PROMPT_CACHE:-}
      - OPENAI_PROMPT_CACHE_TTL=\${OPENAI_PROMPT_CACHE_TTL:-1800}
      - AI_MAX_CONTEXT_TOKENS=\${AI_MAX_CONTEXT_TOKENS:-256000}
      - AI_CONTEXT_OUTPUT_RESERVE_TOKENS=\${AI_CONTEXT_OUTPUT_RESERVE_TOKENS:-4096}
      - AI_MODEL_CONTEXT_WINDOW_TOKENS=\${AI_MODEL_CONTEXT_WINDOW_TOKENS:-260096}
      - AI_INDEX_WORKERS=\${AI_INDEX_WORKERS:-2}
      - KNOWLEDGE_DATA_PATH=/app/data/knowledge
      - KNOWLEDGE_MAX_FILE_MB=\${KNOWLEDGE_MAX_FILE_MB:-50}
      - KNOWLEDGE_MAX_EXTRACTED_MB=\${KNOWLEDGE_MAX_EXTRACTED_MB:-10}
      - KNOWLEDGE_MAX_OFFICE_UNCOMPRESSED_MB=\${KNOWLEDGE_MAX_OFFICE_UNCOMPRESSED_MB:-100}
      - KNOWLEDGE_MAX_IMAGE_MEGAPIXELS=\${KNOWLEDGE_MAX_IMAGE_MEGAPIXELS:-40}
      - KNOWLEDGE_MAX_PDF_PAGES=\${KNOWLEDGE_MAX_PDF_PAGES:-100}
      - KNOWLEDGE_EXTRACT_WORKERS=\${KNOWLEDGE_EXTRACT_WORKERS:-2}
      - DATABASE_URL=postgres://\${POSTGRES_USER:-dreamtrans}:\${POSTGRES_PASSWORD:?POSTGRES_PASSWORD must be set}@db:5432/\${POSTGRES_DB:-dreamtrans}?sslmode=disable
      - JWT_SECRET=\${JWT_SECRET:?JWT_SECRET must be set}
      - JWT_REFRESH_SECRET=\${JWT_REFRESH_SECRET:?JWT_REFRESH_SECRET must be set}
      - ADMIN_EMAIL=\${ADMIN_EMAIL:-}
      - ADMIN_PASSWORD=\${ADMIN_PASSWORD:-}
      - DREAMTRANS_API_KEY=\${DREAMTRANS_API_KEY:-}
      - DREAMTRANS_ADMIN_API_KEY=\${DREAMTRANS_ADMIN_API_KEY:-}
      - ALLOW_ANONYMOUS_API=\${ALLOW_ANONYMOUS_API:-false}
      - ALLOW_WEBSOCKET_QUERY_TOKEN=\${ALLOW_WEBSOCKET_QUERY_TOKEN:-false}
      - API_RATE_LIMIT_PER_MINUTE=\${API_RATE_LIMIT_PER_MINUTE:-120}
      - WEBSOCKET_MAX_CONNECTIONS=\${WEBSOCKET_MAX_CONNECTIONS:-256}
      - WEBSOCKET_MAX_CONNECTIONS_PER_PRINCIPAL=\${WEBSOCKET_MAX_CONNECTIONS_PER_PRINCIPAL:-4}
      - REGISTRATION_ENABLED=\${REGISTRATION_ENABLED:-false}
      - REGISTRATION_INVITE_CODE=\${REGISTRATION_INVITE_CODE:-}
      - SIGNUP_RISK_SECRET=\${SIGNUP_RISK_SECRET:-}
      - TRUSTED_PROXY_CIDRS=\${TRUSTED_PROXY_CIDRS:-}
      - REGISTRATION_RATE_LIMIT_PER_HOUR=\${REGISTRATION_RATE_LIMIT_PER_HOUR:-5}
      - REGISTRATION_ALLOWED_EMAIL_DOMAINS=\${REGISTRATION_ALLOWED_EMAIL_DOMAINS:-}
      - REGISTRATION_BLOCKED_EMAIL_DOMAINS=\${REGISTRATION_BLOCKED_EMAIL_DOMAINS:-}
      - EMAIL_VERIFICATION_REQUIRED=\${EMAIL_VERIFICATION_REQUIRED:-}
      - RESEND_API_KEY=\${RESEND_API_KEY:-}
      - MAIL_FROM=\${MAIL_FROM:-}
      - SMTP_HOST=\${SMTP_HOST:-}
      - SMTP_PORT=\${SMTP_PORT:-}
      - SMTP_USERNAME=\${SMTP_USERNAME:-}
      - SMTP_PASSWORD=\${SMTP_PASSWORD:-}
      - SMTP_FROM=\${SMTP_FROM:-}
      - SMTP_TLS=\${SMTP_TLS:-}
      - CORS_ALLOWED_ORIGINS=\${CORS_ALLOWED_ORIGINS:-}
      - ALLOW_USER_API_KEY=\${ALLOW_USER_API_KEY:-false}
      - RAG_MAX_DB_MB=\${RAG_MAX_DB_MB:-102400}
      - STRIPE_SECRET_KEY=\${STRIPE_SECRET_KEY:-}
      - STRIPE_WEBHOOK_SECRET=\${STRIPE_WEBHOOK_SECRET:-}
      - APP_BASE_URL=\${APP_BASE_URL:-}
      - STRIPE_CURRENCY=\${STRIPE_CURRENCY:-usd}
      - STRIPE_USD_EXCHANGE_RATE=\${STRIPE_USD_EXCHANGE_RATE:-}
      - STRIPE_FX_MARKUP_PERCENT=\${STRIPE_FX_MARKUP_PERCENT:-0}
      - STRIPE_FX_RATE_URL=\${STRIPE_FX_RATE_URL:-}
    healthcheck:
      test: ["CMD", "wget", "-q", "-O", "/dev/null", "http://127.0.0.1:8080/readyz"]
      interval: 10s
      timeout: 3s
      start_period: 10s
      retries: 12
    stop_grace_period: 30s
    volumes:
      - appdata:/app/data
    depends_on:
      db:
        condition: service_healthy
      migrate:
        condition: service_completed_successfully

volumes:
  pgdata:
  appdata:
EOF

    success "Docker Compose file created"
}

compose_service_container_id() {
    $COMPOSE_CMD ps -q "$1" 2>/dev/null | sed -n '1p'
}

compose_service_container_id_any_state() {
    local container_id
    local canonical_container_id=""
    local oneoff

    while IFS= read -r container_id; do
        [[ -n "$container_id" ]] || continue
        oneoff="$(docker inspect --format \
            '{{index .Config.Labels "com.docker.compose.oneoff"}}' \
            "$container_id" 2>/dev/null || true)"
        case "${oneoff,,}" in
            true)
                continue
                ;;
            false)
                if [[ -n "$canonical_container_id" ]]; then
                    error "Multiple canonical containers found for Compose service $1"
                    return 1
                fi
                canonical_container_id="$container_id"
                ;;
            *)
                error "Container $container_id has an invalid Compose one-off label"
                return 1
                ;;
        esac
    done < <($COMPOSE_CMD ps -a -q "$1" 2>/dev/null)
    if [[ -n "$canonical_container_id" ]]; then
        printf '%s\n' "$canonical_container_id"
    fi
}

# Wait for PostgreSQL to be ready
wait_for_db() {
    local max_attempts=30
    local attempt=0
    local db_container_id
    while true; do
        db_container_id="$(compose_service_container_id db)"
        if [[ -n "$db_container_id" ]] &&
           docker exec "$db_container_id" /bin/sh -c '
               exec pg_isready \
                   -U "${POSTGRES_USER:-dreamtrans}" \
                   -d "${POSTGRES_DB:-dreamtrans}"
           ' >/dev/null 2>&1; then
            return 0
        fi
        attempt=$((attempt + 1))
        if [[ $attempt -ge $max_attempts ]]; then
            error "PostgreSQL failed to start for this Compose project"
            return 1
        fi
        sleep 1
    done
}

# Run all database migrations
run_migrations() {
    info "Running database migrations..."

    wait_for_db || return 1
    local db_container_id
    db_container_id="$(compose_service_container_id db)"
    if [[ -z "$db_container_id" ]]; then
        error "Unable to resolve the database container for this Compose project"
        return 1
    fi

    local migrations_dir="$INSTALL_DIR/migrations"
    if [[ ! -x "$INSTALL_DIR/migrate.sh" ]] ||
       ! find "$migrations_dir" -maxdepth 1 -type f \
            -name '[0-9][0-9][0-9]_*.sql' -print -quit | grep -q .; then
        error "Matching migration assets are not installed"
        echo "  Pull the selected app image and extract its release bundle first."
        return 1
    fi

    local container_migrations="/tmp/dreamtrans-migrations-$$"
    if ! docker exec "$db_container_id" mkdir -p "$container_migrations"; then
        error "Unable to create the database migration staging directory"
        return 1
    fi
    if ! docker cp "$migrations_dir/." "$db_container_id:$container_migrations"; then
        docker exec "$db_container_id" rm -rf -- "$container_migrations" >/dev/null 2>&1 || true
        error "Unable to copy migrations into the database container"
        return 1
    fi

    if ! docker exec -i -e MIGRATIONS_DIR="$container_migrations" "$db_container_id" /bin/sh -c '
        export PGHOST=127.0.0.1
        export PGPORT=5432
        export PGDATABASE="${POSTGRES_DB:-dreamtrans}"
        export PGUSER="${POSTGRES_USER:-dreamtrans}"
        export PGPASSWORD="${POSTGRES_PASSWORD:?POSTGRES_PASSWORD must be set}"
        exec /bin/sh
    ' < "$INSTALL_DIR/migrate.sh"; then
        docker exec "$db_container_id" rm -rf -- "$container_migrations" >/dev/null 2>&1 || true
        error "Database migration failed; the failing version was rolled back and not marked as applied"
        return 1
    fi
    docker exec "$db_container_id" rm -rf -- "$container_migrations" >/dev/null 2>&1 || true

    success "Database migrations complete"
}

# Initialize database schema (alias for backwards compatibility)
init_database() {
    run_migrations
}

run_database_maintenance() {
    local maintenance_mode="$1"
    local operation_status=0

    case "$maintenance_mode" in
        init|migrate)
            ;;
        *)
            error "Unknown database maintenance mode: $maintenance_mode"
            return 1
            ;;
    esac

    acquire_update_lock || return 1
    require_installation "false" || operation_status=$?
    if [[ "$operation_status" == "0" ]]; then
        cd "$INSTALL_DIR" || operation_status=$?
    fi
    if [[ "$operation_status" == "0" ]]; then
        prepare_release_migrations || operation_status=$?
    fi
    if [[ "$operation_status" == "0" ]]; then
        $COMPOSE_CMD up -d db || operation_status=$?
    fi
    if [[ "$operation_status" == "0" ]]; then
        if [[ "$maintenance_mode" == "init" ]]; then
            init_database || operation_status=$?
        else
            run_migrations || operation_status=$?
        fi
    fi
    if ! release_update_lock; then
        error "Unable to release the DreamTrans lifecycle lock"
        operation_status=1
    fi
    if [[ "$operation_status" != "0" ]]; then
        return "$operation_status"
    fi
}

wait_for_app_ready() {
    local max_attempts=75
    local attempt=0
    local state
    local health
    local app_container_id
    local running_image_id
    local expected_image_id="${APP_IMAGE_ID:-}"

    if [[ -z "$expected_image_id" ]]; then
        local expected_image_ref
        expected_image_ref="$(compose_app_image_ref)"
        expected_image_id="$(docker image inspect --format '{{.Id}}' \
            "$expected_image_ref" 2>/dev/null || true)"
    fi

    info "Waiting for DreamTrans readiness..."
    while ((attempt < max_attempts)); do
        app_container_id="$(compose_service_container_id app)"
        state="$(docker inspect --format '{{.State.Status}}' "$app_container_id" 2>/dev/null || true)"
        health="$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{end}}' \
            "$app_container_id" 2>/dev/null || true)"
        if [[ "$state" == "running" && "$health" == "healthy" ]]; then
            running_image_id="$(docker inspect --format '{{.Image}}' \
                "$app_container_id" 2>/dev/null || true)"
            if [[ -n "$expected_image_id" && "$running_image_id" != "$expected_image_id" ]]; then
                error "Ready container image does not match the selected release"
                echo "  Expected: $expected_image_id"
                echo "  Running:  ${running_image_id:-unknown}"
                return 1
            fi
            success "DreamTrans readiness check passed"
            return 0
        fi
        if [[ "$state" == "exited" || "$state" == "dead" ]]; then
            error "DreamTrans stopped before becoming ready (status: $state)"
            $COMPOSE_CMD logs --tail=100 app 2>/dev/null || true
            return 1
        fi
        attempt=$((attempt + 1))
        sleep 2
    done

    error "DreamTrans did not become ready (container: ${state:-missing}, health: ${health:-missing})"
    $COMPOSE_CMD logs --tail=100 app 2>/dev/null || true
    return 1
}

# Start services
start_services() {
    info "Starting DreamTrans..."
    cd "$INSTALL_DIR"

    # Keep the fresh-install behavior of pulling both PostgreSQL-backed
    # services, while isolating the public application-image retry from any
    # stale GHCR login in the operator's Docker configuration.
    info "Pulling database images..."
    $COMPOSE_CMD pull db migrate || return 1
    info "Pulling latest application image..."
    pull_app_image || return 1
    prepare_release_migrations || return 1

    # Bring up PostgreSQL first. The application requires the schema (including
    # the bootstrap-admin tables) to exist before its first start.
    $COMPOSE_CMD up -d db || return 1
    init_database || return 1

    # Start the application only after every migration committed successfully.
    $COMPOSE_CMD up -d app || return 1

    wait_for_app_ready || return 1
    ADMIN_BOOTSTRAP_PENDING_THIS_RUN="true"
    ADMIN_BOOTSTRAP_SECURED_THIS_RUN="false"
    ADMIN_DISPLAY_EMAIL="$ADMIN_EMAIL"
    ADMIN_DISPLAY_PASSWORD="$ADMIN_PASSWORD"
    if ! retire_admin_bootstrap_credentials; then
        error "DreamTrans started, but its bootstrap credentials could not be retired"
        warn "Attempting to restore the fresh-install configuration for a safe retry..."
        restore_fresh_admin_bootstrap_credentials || true
        return 1
    fi
    success "DreamTrans is running!"
}

# Update installation
update_installation() {
    info "Updating DreamTrans..."

    if [[ ! -d "$INSTALL_DIR" ]]; then
        error "Installation not found at $INSTALL_DIR"
        exit 1
    fi

    cd "$INSTALL_DIR"
    acquire_update_lock || return 1
    begin_update_transaction || {
        release_update_lock || true
        return 1
    }
    trap 'rollback_update_on_error "$?"' ERR
    trap 'rollback_update_on_error 130' INT
    trap 'rollback_update_on_error 143' TERM
    ensure_app_container_for_update_discovery || {
        rollback_update_deployment
        return 1
    }

    # New releases require separate access/refresh signing keys. Repair legacy
    # installer environments before Compose evaluates required variables.
    ensure_jwt_secrets_for_update || { rollback_update_deployment; return 1; }
    sync_image_tag_for_update || { rollback_update_deployment; return 1; }
    harden_existing_compose || { rollback_update_deployment; return 1; }

    # Pull only the application release by default. When harden_existing_compose
    # switches the database to the pinned pgvector PG16 image (required by
    # migration 019), also pull that exact image so CREATE EXTENSION vector
    # can load the extension control files from the container filesystem.
    info "Pulling latest application image..."
    pull_app_image || { rollback_update_deployment; return 1; }
    if grep -Fq "image: ${POSTGRES_IMAGE}" "$INSTALL_DIR/docker-compose.yml"; then
        info "Pulling pinned PostgreSQL/pgvector image..."
        if ! $COMPOSE_CMD pull db migrate; then
            error "Unable to pull ${POSTGRES_IMAGE}"
            rollback_update_deployment
            return 1
        fi
    fi
    prepare_release_migrations || { rollback_update_deployment; return 1; }

    # Apply migrations before recreating the application container, so a failed
    # migration leaves the currently running version untouched. Each committed
    # migration is forward-only and therefore must remain backward-compatible
    # with the previous application image used by rollback.
    UPDATE_DB_RUNTIME_TOUCHED="true"
    # Compose recreates the database container when the image pin changes
    # (for example postgres:16-alpine -> pgvector/pgvector:...).
    $COMPOSE_CMD up -d db || { rollback_update_deployment; return 1; }
    wait_for_db || { rollback_update_deployment; return 1; }
    UPDATE_DATABASE_MIGRATION_ATTEMPTED="true"
    run_migrations || { rollback_update_deployment; return 1; }
    configure_admin_for_update || { rollback_update_deployment; return 1; }
    repair_app_data_permissions_for_update || {
        rollback_update_deployment
        return 1
    }

    info "Restarting application..."
    UPDATE_APP_RECREATE_ATTEMPTED="true"
    $COMPOSE_CMD up -d app || { rollback_update_deployment; return 1; }

    wait_for_app_ready || { rollback_update_deployment; return 1; }
    retire_admin_bootstrap_credentials || {
        rollback_update_deployment
        return 1
    }
    set_env_value "IMAGE_TAG" "$IMAGE_TAG" || { rollback_update_deployment; return 1; }
    chmod 600 "$INSTALL_DIR/.env" || { rollback_update_deployment; return 1; }
    commit_update_transaction || { rollback_update_deployment; return 1; }
    configure_backup_cron
    trap - ERR INT TERM
    release_update_lock || warn "Update completed, but the update lock descriptor could not be released early"

    if [[ "$ADMIN_CREDENTIALS_ADDED" == "true" ]]; then
        warn "A bootstrap administrator was added during this security update:"
        echo "  Email: $ADMIN_DISPLAY_EMAIL"
        if [[ "$ADMIN_PASSWORD_GENERATED" == "true" ]]; then
            echo "  Password: $ADMIN_DISPLAY_PASSWORD"
            echo "  This generated password is shown only once; store it securely."
        else
            echo "  Password: (the password supplied during the update)"
        fi
    fi

    success "DreamTrans updated successfully!"
}

# Stop services
stop_services() {
    info "Stopping DreamTrans..."

    if [[ ! -d "$INSTALL_DIR" ]]; then
        error "Installation not found at $INSTALL_DIR"
        return 1
    fi

    acquire_update_lock || return 1
    local operation_status=0
    require_installation "false" || operation_status=$?
    cd "$INSTALL_DIR"
    if [[ "$operation_status" == "0" ]]; then
        $COMPOSE_CMD down || operation_status=$?
    fi
    if ! release_update_lock; then
        error "Unable to release the DreamTrans lifecycle lock"
        operation_status=1
    fi
    if [[ "$operation_status" != "0" ]]; then
        return "$operation_status"
    fi

    success "DreamTrans stopped"
}

# Start services
start_services_only() {
    info "Starting DreamTrans..."

    if [[ ! -d "$INSTALL_DIR" ]]; then
        error "Installation not found at $INSTALL_DIR"
        return 1
    fi

    acquire_update_lock || return 1
    local operation_status=0
    require_installation "false" || operation_status=$?
    cd "$INSTALL_DIR"
    if [[ "$operation_status" == "0" ]]; then
        $COMPOSE_CMD up -d || operation_status=$?
    fi
    if [[ "$operation_status" == "0" ]]; then
        wait_for_app_ready || operation_status=$?
    fi
    if ! release_update_lock; then
        error "Unable to release the DreamTrans lifecycle lock"
        operation_status=1
    fi
    if [[ "$operation_status" != "0" ]]; then
        return "$operation_status"
    fi
    success "DreamTrans started"
    show_access_info
}

# Restart services
restart_services() {
    info "Restarting DreamTrans..."

    if [[ ! -d "$INSTALL_DIR" ]]; then
        error "Installation not found at $INSTALL_DIR"
        return 1
    fi

    acquire_update_lock || return 1
    local operation_status=0
    require_installation "false" || operation_status=$?
    cd "$INSTALL_DIR"
    if [[ "$operation_status" == "0" ]]; then
        $COMPOSE_CMD restart || operation_status=$?
    fi
    if [[ "$operation_status" == "0" ]]; then
        wait_for_app_ready || operation_status=$?
    fi
    if ! release_update_lock; then
        error "Unable to release the DreamTrans lifecycle lock"
        operation_status=1
    fi
    if [[ "$operation_status" != "0" ]]; then
        return "$operation_status"
    fi
    success "DreamTrans restarted"
    show_access_info
}

# Show status
show_status() {
    info "DreamTrans Status:"

    if [[ ! -d "$INSTALL_DIR" ]]; then
        error "Installation not found at $INSTALL_DIR"
        exit 1
    fi

    cd "$INSTALL_DIR"
    echo ""
    $COMPOSE_CMD ps
    echo ""
}

# Show logs
show_logs() {
    if [[ ! -d "$INSTALL_DIR" ]]; then
        error "Installation not found at $INSTALL_DIR"
        exit 1
    fi

    cd "$INSTALL_DIR"
    $COMPOSE_CMD logs -f
}

# Show access info
show_access_info() {
    # Try to get PORT from .env file
    if [[ -f "$INSTALL_DIR/.env" ]]; then
        local env_port=$(grep "^PORT=" "$INSTALL_DIR/.env" 2>/dev/null | cut -d= -f2)
        if [[ -n "$env_port" ]]; then
            PORT="$env_port"
        fi
    fi
    # Prefer Docker's resolved published port; this handles bind addresses,
    # variable interpolation, and legacy installer files.
    if [[ -f "$INSTALL_DIR/docker-compose.yml" ]]; then
        local published_endpoint
        local compose_port
        published_endpoint="$($COMPOSE_CMD port app 8080 2>/dev/null | head -n 1 || true)"
        compose_port="${published_endpoint##*:}"
        if [[ "$compose_port" =~ ^[0-9]+$ ]]; then
            PORT="$compose_port"
        fi
    fi
    echo ""
    echo -e "  ${CYAN}Workspace:${NC}          http://localhost:${PORT}"
    echo -e "  ${CYAN}Authenticated entry:${NC} http://localhost:${PORT}/pro"
    echo ""
}

# Uninstall
uninstall() {
    require_installation "false"
    warn "This will remove DreamTrans and all its data!"
    warn "Uninstall target: $INSTALL_DIR"
    read_input "Are you sure? (yes/no): " confirm "" false

    if [[ "$confirm" != "yes" ]]; then
        info "Uninstall cancelled"
        exit 0
    fi

    acquire_update_lock || return 1
    local operation_status=0
    require_installation "false" || operation_status=$?
    if [[ "$operation_status" != "0" ]]; then
        release_update_lock || true
        return "$operation_status"
    fi

    info "Stopping and removing containers..."
    cd "$INSTALL_DIR"
    if ! $COMPOSE_CMD down -v --remove-orphans; then
        error "Docker Compose cleanup failed; configuration and data were preserved"
        release_update_lock || true
        return 1
    fi

    # Revalidate immediately before deletion. Only installer-owned paths are
    # removed; an accidental --dir pointing at an existing project can never
    # erase unknown files.
    if ! normalize_install_dir || ! require_installation "false"; then
        release_update_lock || true
        return 1
    fi
    info "Removing installer-owned files..."
    rm -f -- \
        "$INSTALL_DIR/.env" \
        "$INSTALL_DIR/docker-compose.yml" \
        "$INSTALL_DIR/migrate.sh" \
        "$INSTALL_DIR/backup.sh" \
        "$INSTALL_DIR/.backup.sh.new" \
        "$INSTALL_DIR/$INSTALL_SENTINEL"
    rm -rf --one-file-system -- \
        "$INSTALL_DIR/migrations" \
        "$INSTALL_DIR/data"
    find "$INSTALL_DIR" -mindepth 1 -maxdepth 1 \
        \( -name '.migration-assets.*' -o -name '.migrations.previous.*' -o \
           -name '.update-backup.*' -o -name '.migrate.sh.new.*' -o \
           -name '.dreamtrans-update.lock.tmp.*' \) \
        -exec rm -rf --one-file-system -- {} +

    if ! remove_update_lock_file; then
        release_update_lock || true
        error "DreamTrans was stopped, but its lifecycle lock could not be removed safely"
        return 1
    fi
    if ! release_update_lock; then
        error "DreamTrans was stopped, but its lifecycle lock could not be released"
        return 1
    fi

    if rmdir -- "$INSTALL_DIR" 2>/dev/null; then
        success "DreamTrans and its installation directory have been removed"
    else
        success "DreamTrans has been uninstalled"
        warn "Unknown files remain in $INSTALL_DIR, so the directory was preserved"
    fi
}

# Print completion message
ensure_backup_env_block() {
    local env_file="$1"
    if grep -q '^#\? *R2_ACCOUNT_ID=' "$env_file"; then
        return 0
    fi
    cat >> "$env_file" <<'EOF_BACKUP' || return 1

# === Backups (optional, see docs/BACKUP.md) ===
# Fill in the R2_* values and run the installer --update again (or
# ./backup.sh --install-cron): the passphrase is generated and the daily
# 03:15 backup is scheduled automatically.
# R2_ACCOUNT_ID=
# R2_ACCESS_KEY_ID=
# R2_SECRET_ACCESS_KEY=
# R2_BUCKET=
# BACKUP_PASSPHRASE=
# BACKUP_RETENTION_DAYS=30
# BACKUP_HEALTHCHECK_URL=
EOF_BACKUP
}

# configure_backup_cron turns backups on as soon as the operator has filled in
# the R2 settings: it generates the passphrase if none exists (printing it
# once) and schedules the daily run. Without R2 settings it only points at
# the documentation, so a fresh install never touches the host crontab.
configure_backup_cron() {
    local env_file="$INSTALL_DIR/.env" helper="$INSTALL_DIR/backup.sh"
    [[ -f "$env_file" ]] || return 0
    local bucket account
    bucket="$(sed -n 's/^R2_BUCKET=//p' "$env_file" | tail -n 1)"
    account="$(sed -n 's/^R2_ACCOUNT_ID=//p' "$env_file" | tail -n 1)"
    if [[ -z "$bucket" || -z "$account" ]]; then
        if grep -q '^# *R2_BUCKET=..*' "$env_file"; then
            warn "R2_BUCKET is still commented out in $env_file; remove the leading '#' from the R2_* lines and run --update again"
        else
            info "Backups are not configured yet: fill in the R2_* values in $env_file (docs/BACKUP.md)"
        fi
        return 0
    fi
    if [[ ! -x "$helper" ]]; then
        warn "R2 is configured but $helper is missing: the pulled image predates the backup helper. Run --update again once a newer image is published."
        return 0
    fi
    if ! grep -q '^BACKUP_PASSPHRASE=..*' "$env_file"; then
        warn "No BACKUP_PASSPHRASE yet; generating one. Copy it to a password manager now:"
        if ! INSTALL_DIR="$INSTALL_DIR" "$helper" --init; then
            warn "Could not generate the backup passphrase. Retry with:"
            printf '  INSTALL_DIR=%q %q --init\n' "$INSTALL_DIR" "$helper"
            return 0
        fi
    fi
    if ! command -v crontab >/dev/null 2>&1; then
        warn "crontab is not available. Run a backup with the command below, or install cron to schedule daily backups:"
        printf '  INSTALL_DIR=%q %q\n' "$INSTALL_DIR" "$helper"
        return 0
    fi
    if INSTALL_DIR="$INSTALL_DIR" "$helper" --install-cron >/dev/null; then
        success "Daily encrypted backup to R2 scheduled at 03:15 (log: $INSTALL_DIR/backups/backup.log)"
    else
        warn "Could not schedule the daily backup. Retry with:"
        printf '  INSTALL_DIR=%q %q --install-cron\n' "$INSTALL_DIR" "$helper"
    fi
}

print_completion() {
    local quoted_install_dir
    printf -v quoted_install_dir '%q' "$INSTALL_DIR"
    echo ""
    echo -e "${GREEN}╔═══════════════════════════════════════════════════════════════╗${NC}"
    echo -e "${GREEN}║                                                               ║${NC}"
    echo -e "${GREEN}║              DreamTrans installed successfully!              ║${NC}"
    echo -e "${GREEN}║                                                               ║${NC}"
    echo -e "${GREEN}╚═══════════════════════════════════════════════════════════════╝${NC}"
    echo ""
    echo -e "  ${CYAN}Workspace:${NC}          http://localhost:${PORT}"
    echo -e "  ${CYAN}Authenticated entry:${NC} http://localhost:${PORT}/pro"
    echo -e "  ${CYAN}Install Dir:${NC}        $INSTALL_DIR"
    echo ""
    echo -e "  ${YELLOW}Useful Commands:${NC}"
    echo "    cd -- $quoted_install_dir"
    echo "    $COMPOSE_CMD logs -f        # View logs"
    echo "    $COMPOSE_CMD restart        # Restart services"
    echo "    $COMPOSE_CMD down           # Stop services"
    echo ""
    echo -e "  ${YELLOW}Update:${NC}"
    echo "    curl -fsSL https://raw.githubusercontent.com/CoYumeLabs/DreamTrans/main/scripts/install.sh | bash -s -- --update --dir $quoted_install_dir"
    echo ""
    echo -e "  ${YELLOW}Administrator:${NC}"
    echo "    Email:    ${ADMIN_DISPLAY_EMAIL:-$ADMIN_EMAIL}"
    if [[ "$ADMIN_PASSWORD_GENERATED" == "true" ]]; then
        echo "    Password: ${ADMIN_DISPLAY_PASSWORD:-$ADMIN_PASSWORD}"
        echo "    This generated password is shown only once; store it securely."
    else
        echo "    Password: (the password supplied during installation)"
    fi
    echo ""
    echo -e "  ${YELLOW}Notes:${NC}"
    echo "    - The unified responsive workspace is available at / and /pro"
    echo "    - /pro requires sign-in; use the administrator credentials above"
    echo "    - Self-registration is disabled by default"
    echo ""
}

# Main function
main() {
    print_banner
    parse_args "$@"
    validate_command_selection
    normalize_install_dir
    validate_deployment_options

    # Handle special modes
    if [[ "$UNINSTALL_MODE" == "true" ]]; then
        check_prerequisites
        require_installation "false"
        uninstall
        exit 0
    fi

    if [[ "$STOP_MODE" == "true" ]]; then
        check_prerequisites
        require_installation "false"
        stop_services
        exit 0
    fi

    if [[ "$START_MODE" == "true" ]]; then
        check_prerequisites
        require_installation "false"
        start_services_only
        exit 0
    fi

    if [[ "$RESTART_MODE" == "true" ]]; then
        check_prerequisites
        require_installation "false"
        restart_services
        exit 0
    fi

    if [[ "$STATUS_MODE" == "true" ]]; then
        check_prerequisites
        require_installation "false"
        show_status
        exit 0
    fi

    if [[ "$LOGS_MODE" == "true" ]]; then
        check_prerequisites
        require_installation "false"
        show_logs
        exit 0
    fi

    if [[ "$INIT_DB_MODE" == "true" ]]; then
        check_prerequisites
        require_installation "false"
        run_database_maintenance init
        exit 0
    fi

    if [[ "$MIGRATE_MODE" == "true" ]]; then
        check_prerequisites
        require_installation "false"
        run_database_maintenance migrate
        exit 0
    fi

    if [[ "$UPDATE_MODE" == "true" ]]; then
        check_prerequisites
        require_installation "true"
        update_installation
        show_access_info
        exit 0
    fi

    # Normal installation
    check_prerequisites
    # Fail fast on an already-installed or unrelated non-empty target before
    # asking for secrets. claim_fresh_install_target repeats this validation
    # under the lifecycle lock before any installation file is written.
    validate_fresh_install_target_for_claim
    prompt_api_keys
    begin_fresh_install_transaction
    claim_fresh_install_target
    create_directories
    generate_env_file
    generate_compose_file
    start_services
    finalize_fresh_install
    configure_backup_cron
    print_completion
}

# Run main function
main "$@"
