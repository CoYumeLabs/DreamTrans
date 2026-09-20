#!/usr/bin/env bash
# DreamTrans database backup to Cloudflare R2.
#
#   scripts/backup.sh                 dump, encrypt, upload, prune
#   scripts/backup.sh --init          generate BACKUP_PASSPHRASE into .env
#   scripts/backup.sh --install-cron  run every day at 03:15 host time
#   scripts/backup.sh --list          show what the bucket holds
#   scripts/backup.sh --dry-run       print the plan without touching anything
#
# Reads INSTALL_DIR/.env as data for POSTGRES_USER/POSTGRES_DB and these settings:
#   R2_ACCOUNT_ID          Cloudflare account id (from the R2 overview page)
#   R2_ACCESS_KEY_ID       R2 API token key with Object Read & Write
#   R2_SECRET_ACCESS_KEY   its secret
#   R2_BUCKET              bucket name
#   BACKUP_PASSPHRASE      every upload is AES-256 encrypted with it; losing
#                          it makes the backups unreadable, so keep a copy
#                          outside this server
#   BACKUP_RETENTION_DAYS  delete remote backups older than this (default 30)
#   BACKUP_HEALTHCHECK_URL optional ping URL (healthchecks.io etc.) hit on
#                          success, and with /fail appended on failure
#
# The database dump uses pg_dump inside the db container, so nothing needs
# installing on the host. Uploads go through the rclone container image, so
# neither rclone nor aws-cli is needed either. Restore steps: docs/BACKUP.md.
set -Eeuo pipefail

INSTALL_DIR="${INSTALL_DIR:-$HOME/dreamtrans}"
BACKUP_DIR="${BACKUP_DIR:-$INSTALL_DIR/backups}"
LOCAL_KEEP="${BACKUP_LOCAL_KEEP:-7}"
RCLONE_IMAGE="${RCLONE_IMAGE:-rclone/rclone:1.68}"
REMOTE_PREFIX="${BACKUP_REMOTE_PREFIX:-dreamtrans}"
DOCKER="${DOCKER:-docker}"
MODE="backup"

log() { printf '%s %s\n' "$(date -u +%FT%TZ)" "$*"; }
fail() { log "ERROR: $*" >&2; ping_healthcheck fail; exit 1; }

ping_healthcheck() {
    [[ -n "${BACKUP_HEALTHCHECK_URL:-}" ]] || return 0
    local url="$BACKUP_HEALTHCHECK_URL"
    [[ "${1:-}" == "fail" ]] && url="${url%/}/fail"
    curl -fsS -m 10 --retry 3 -o /dev/null "$url" || log "healthcheck ping failed (ignored)"
}

case "${1:-}" in
    --init) MODE="init" ;;
    --install-cron) MODE="install-cron" ;;
    --list) MODE="list" ;;
    --dry-run) MODE="dry-run" ;;
    "") ;;
    *) fail "unknown option: $1" ;;
esac

read_backup_settings() {
    # Compose dotenv files are not shell scripts (for example, MAIL_FROM may
    # contain an unquoted display name and <address>). Read only our settings;
    # never execute substitutions or unrelated application configuration.
    local line name raw value quote char next rest closed index line_number=0
    while IFS= read -r line || [[ -n "$line" ]]; do
        line_number=$((line_number + 1))
        line="${line%$'\r'}"
        [[ "$line" =~ ^[[:space:]]*(export[[:space:]]+)?([A-Za-z_][A-Za-z_0-9]*)[[:space:]]*=(.*)$ ]] || continue
        name="${BASH_REMATCH[2]}"
        raw="${BASH_REMATCH[3]}"
        case "$name" in
            POSTGRES_USER|POSTGRES_DB|R2_ACCOUNT_ID|R2_ACCESS_KEY_ID|R2_SECRET_ACCESS_KEY|R2_BUCKET|BACKUP_PASSPHRASE|BACKUP_RETENTION_DAYS|BACKUP_HEALTHCHECK_URL) ;;
            *) continue ;;
        esac
        raw="${raw#"${raw%%[![:space:]]*}"}"
        case "${raw:0:1}" in
            \"|\')
                quote="${raw:0:1}"; value=""; closed=false
                for ((index = 1; index < ${#raw}; index++)); do
                    char="${raw:index:1}"
                    if [[ "$char" == "$quote" ]]; then
                        rest="${raw:index+1}"
                        [[ "$rest" =~ ^[[:space:]]*(#.*)?$ ]] || fail "invalid $name at .env line $line_number (unexpected text after quote)"
                        closed=true
                        break
                    fi
                    if [[ "$char" == \\ ]]; then
                        next="${raw:index+1:1}"
                        if [[ "$next" == "$quote" || ( "$quote" == \" && ( "$next" == \\ || "$next" == '$' ) ) ]]; then
                            char="$next"; index=$((index + 1))
                        elif [[ "$quote" == \" ]]; then
                            case "$next" in
                                n) char=$'\n'; index=$((index + 1)) ;;
                                r) char=$'\r'; index=$((index + 1)) ;;
                                t) char=$'\t'; index=$((index + 1)) ;;
                            esac
                        fi
                    fi
                    value+="$char"
                done
                [[ "$closed" == true ]] || fail "invalid $name at .env line $line_number (unterminated quote; backup settings must use one line)"
                ;;
            *)
                value="${raw%%[[:space:]]#*}"
                value="${value%"${value##*[![:space:]]}"}"
                ;;
        esac
        export "$name=$value"
    done < "$INSTALL_DIR/.env"
}

load_env() {
    [[ -f "$INSTALL_DIR/.env" ]] || fail "no .env in $INSTALL_DIR (set INSTALL_DIR)"
    read_backup_settings
    for name in R2_ACCOUNT_ID R2_ACCESS_KEY_ID R2_SECRET_ACCESS_KEY R2_BUCKET BACKUP_PASSPHRASE; do
        [[ -n "${!name:-}" ]] || fail "$name is not set in $INSTALL_DIR/.env"
    done
    [[ "${#BACKUP_PASSPHRASE}" -ge 16 ]] || fail "BACKUP_PASSPHRASE must be at least 16 characters"
    RETENTION_DAYS="${BACKUP_RETENTION_DAYS:-30}"
    [[ "$RETENTION_DAYS" =~ ^[0-9]+$ && "$RETENTION_DAYS" -ge 1 ]] || fail "BACKUP_RETENTION_DAYS must be a positive integer"
}

compose() {
    if $DOCKER compose version >/dev/null 2>&1; then
        (cd "$INSTALL_DIR" && $DOCKER compose "$@")
    else
        (cd "$INSTALL_DIR" && docker-compose "$@")
    fi
}

# rclone runs in a container with the R2 remote defined purely through
# environment variables, so no config file ever holds the credentials.
rclone() {
    $DOCKER run --rm \
        -e RCLONE_CONFIG_R2_TYPE=s3 \
        -e RCLONE_CONFIG_R2_PROVIDER=Cloudflare \
        -e RCLONE_CONFIG_R2_ACCESS_KEY_ID="$R2_ACCESS_KEY_ID" \
        -e RCLONE_CONFIG_R2_SECRET_ACCESS_KEY="$R2_SECRET_ACCESS_KEY" \
        -e RCLONE_CONFIG_R2_ENDPOINT="https://${R2_ACCOUNT_ID}.r2.cloudflarestorage.com" \
        -e RCLONE_CONFIG_R2_ACL=private \
        -e RCLONE_CONFIG_R2_NO_CHECK_BUCKET=true \
        -v "$BACKUP_DIR:/backups:ro" \
        "$RCLONE_IMAGE" "$@"
}

verify_remote() {
    local file="$1" remote_digest local_digest
    # Download the encrypted object rather than trusting a multipart ETag.
    remote_digest="$(rclone cat "r2:${R2_BUCKET}/${REMOTE_PREFIX}/$(basename "$file")" | sha256sum)" \
        || fail "remote backup verification download failed"
    local_digest="$(sha256sum "$file")" || fail "local backup checksum failed"
    [[ "${remote_digest%% *}" == "${local_digest%% *}" ]] || fail "remote backup checksum mismatch"
    log "remote download checksum verified: $(basename "$file")"
}

# The Go controller is installed beside this helper from a fixed image bundle.
# Never fall back to a Python controller or choose a different production volume.
ops_cli() {
    local controller="${DREAMTRANS_CTL:-$INSTALL_DIR/dreamtransctl}"
    [[ -x "$controller" ]] || fail "Go controller missing; install the verified DreamTrans operations bundle first"
    "$controller" --dir "$INSTALL_DIR" "$@"
}

managed_snapshot() {
    ops_cli snapshot --output "$1"
}

run_backup() {
    load_env
    mkdir -p "$BACKUP_DIR"
    chmod 700 "$BACKUP_DIR"
    local stamp name dump conf
    stamp="$(date -u +%Y%m%d-%H%M%S)"
    name="dreamtrans-${stamp}"
    dump="$BACKUP_DIR/${name}.dump.enc"
    conf="$BACKUP_DIR/${name}.config.tar.enc"

    if [[ "$MODE" == "dry-run" ]]; then
        if [[ -f "$INSTALL_DIR/.bluegreen/state.json" ]]; then
            log "would snapshot the recorded production database, complete application volume, main/YuAction deployment configuration and blue-green state"
            log "would encrypt and upload ${name}.full.tar.enc to r2:${R2_BUCKET}/${REMOTE_PREFIX}/"
        else
            log "would dump ${POSTGRES_DB:-dreamtrans} as ${POSTGRES_USER:-dreamtrans} to $dump"
            log "would archive .env and docker-compose.yml to $conf"
            log "would upload both to r2:${R2_BUCKET}/${REMOTE_PREFIX}/"
            log "legacy mode does not include the application volume; complete blue-green conversion before relying on full automatic backups"
        fi
        log "would download and verify encrypted bytes before deleting remote files older than ${RETENTION_DAYS} days"
        log "would keep the newest ${LOCAL_KEEP} local backups"
        return 0
    fi

    # Manual verification and cron must not overwrite the same timestamped
    # artifact or prune while the other invocation is still uploading.
    exec 9> "$BACKUP_DIR/.backup.lock"
    flock -n 9 || fail "another backup is already running"

    if [[ -f "$INSTALL_DIR/.bluegreen/state.json" ]]; then
        local full="$BACKUP_DIR/${name}.full.tar" encrypted="$BACKUP_DIR/${name}.full.tar.enc"
        managed_snapshot "$full" || fail "full deployment snapshot failed"
        local database_id
        database_id="$(ops_cli state-field database_id)"
        "$DOCKER" exec -i -e BACKUP_PASSPHRASE "$database_id" openssl enc -aes-256-cbc -pbkdf2 -iter 200000 -salt -pass env:BACKUP_PASSPHRASE \
            < "$full" > "$encrypted" || fail "full snapshot encryption failed"
        rm -f -- "$full"
        rclone copyto "/backups/$(basename "$encrypted")" "r2:${R2_BUCKET}/${REMOTE_PREFIX}/$(basename "$encrypted")" || fail "full snapshot upload failed"
        verify_remote "$encrypted"
        rclone delete "r2:${R2_BUCKET}/${REMOTE_PREFIX}" --min-age "${RETENTION_DAYS}d" || log "remote prune failed (ignored)"
        ops_cli backup-prune "$BACKUP_DIR" "$LOCAL_KEEP"

        log "full deployment backup complete"
        ping_healthcheck ok
        return
    fi

    log "dumping database"
    # pg_dump custom format is already compressed. The passphrase is exported
    # by load_env and forwarded by name (-e BACKUP_PASSPHRASE), so it never
    # appears in a process argument list.
    compose exec -T -e BACKUP_PASSPHRASE db sh -c \
        'pg_dump -U "${POSTGRES_USER:-dreamtrans}" -d "${POSTGRES_DB:-dreamtrans}" -Fc --no-owner \
         | openssl enc -aes-256-cbc -pbkdf2 -iter 200000 -salt -pass env:BACKUP_PASSPHRASE' \
        < /dev/null > "$dump" || fail "pg_dump failed"
    [[ -s "$dump" ]] || fail "dump is empty"

    log "archiving configuration"
    tar -C "$INSTALL_DIR" -cf - .env docker-compose.yml \
        | compose exec -T -e BACKUP_PASSPHRASE db \
            openssl enc -aes-256-cbc -pbkdf2 -iter 200000 -salt -pass env:BACKUP_PASSPHRASE \
        > "$conf" || fail "config archive failed"

    log "uploading to r2:${R2_BUCKET}/${REMOTE_PREFIX}/"
    rclone copyto "/backups/$(basename "$dump")" "r2:${R2_BUCKET}/${REMOTE_PREFIX}/$(basename "$dump")" || fail "upload of dump failed"
    rclone copyto "/backups/$(basename "$conf")" "r2:${R2_BUCKET}/${REMOTE_PREFIX}/$(basename "$conf")" || fail "upload of config failed"
    verify_remote "$dump"
    verify_remote "$conf"

    log "pruning remote backups older than ${RETENTION_DAYS} days"
    rclone delete "r2:${R2_BUCKET}/${REMOTE_PREFIX}" --min-age "${RETENTION_DAYS}d" || log "remote prune failed (ignored)"

    # Keep a few local copies for fast restores; the bucket is the archive.
    ls -1t "$BACKUP_DIR"/dreamtrans-*.dump.enc 2>/dev/null | tail -n +"$((LOCAL_KEEP + 1))" | xargs -r rm -f --
    ls -1t "$BACKUP_DIR"/dreamtrans-*.config.tar.enc 2>/dev/null | tail -n +"$((LOCAL_KEEP + 1))" | xargs -r rm -f --

    log "backup complete: $(basename "$dump") ($(du -h "$dump" | cut -f1))"
    ping_healthcheck ok
}

# init writes a random passphrase into .env when none is set. It is printed
# exactly once: the copy in .env dies with the server, so the operator must
# store it somewhere else before trusting the backups.
init_passphrase() {
    [[ -f "$INSTALL_DIR/.env" ]] || fail "no .env in $INSTALL_DIR (set INSTALL_DIR)"
    read_backup_settings
    if [[ -n "${BACKUP_PASSPHRASE:-}" ]]; then
        log "BACKUP_PASSPHRASE is already set in $INSTALL_DIR/.env; nothing changed"
        return 0
    fi
    local passphrase
    passphrase="$(head -c 48 /dev/urandom | base64 | tr -d '/+=\n' | head -c 40)"
    [[ "${#passphrase}" -eq 40 ]] || fail "could not generate a passphrase"
    if grep -Eq '^[[:space:]]*(export[[:space:]]+)?BACKUP_PASSPHRASE[[:space:]]*=' "$INSTALL_DIR/.env"; then
        sed -i -E "s|^[[:space:]]*(export[[:space:]]+)?BACKUP_PASSPHRASE[[:space:]]*=.*|BACKUP_PASSPHRASE=${passphrase}|" "$INSTALL_DIR/.env"
    else
        printf '\nBACKUP_PASSPHRASE=%s\n' "$passphrase" >> "$INSTALL_DIR/.env"
    fi
    chmod 600 "$INSTALL_DIR/.env"
    log "wrote BACKUP_PASSPHRASE to $INSTALL_DIR/.env"
    printf '\n  BACKUP_PASSPHRASE=%s\n\n' "$passphrase"
    printf 'Store this passphrase outside this server now (password manager). Without it every backup is unreadable.\n'
}

install_cron() {
    load_env
    local script entry
    script="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/$(basename -- "${BASH_SOURCE[0]}")"
    entry="15 3 * * * INSTALL_DIR=$(printf '%q' "$INSTALL_DIR") $(printf '%q' "$script") >> $(printf '%q' "$BACKUP_DIR")/backup.log 2>&1"
    mkdir -p "$BACKUP_DIR"
    ( crontab -l 2>/dev/null | grep -v -F "$script" || true; printf '%s\n' "$entry" ) | crontab -
    log "installed cron entry: $entry"
}

case "$MODE" in
    init) init_passphrase ;;
    backup|dry-run) run_backup ;;
    install-cron) install_cron ;;
    list) load_env; rclone ls "r2:${R2_BUCKET}/${REMOTE_PREFIX}" ;;
esac
