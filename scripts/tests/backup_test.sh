#!/usr/bin/env bash
# Exercises scripts/backup.sh with a fake docker so no database or bucket is
# needed: the dry run must describe the plan, a real run must dump, encrypt,
# upload both files, prune, and rotate local copies.
set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "$SCRIPT_DIR/../.." && pwd)"
FIXTURE="$(mktemp -d /tmp/dreamtrans-backup-test.XXXXXX)"
trap 'rm -rf -- "$FIXTURE"' EXIT

export INSTALL_DIR="$FIXTURE/install"
mkdir -p "$INSTALL_DIR"
printf '%s\n' 'POSTGRES_USER=dreamtrans' 'POSTGRES_DB=dreamtrans' 'POSTGRES_PASSWORD=pw' \
    'R2_ACCOUNT_ID=acct' 'R2_ACCESS_KEY_ID=key' 'R2_SECRET_ACCESS_KEY=secret' \
    'R2_BUCKET=bucket' 'BACKUP_PASSPHRASE=sixteen-characters-long' 'BACKUP_RETENTION_DAYS=14' \
    > "$INSTALL_DIR/.env"
printf 'services: {}\n' > "$INSTALL_DIR/docker-compose.yml"

# Fake docker: `compose exec ... db <cmd>` echoes a payload so the pipeline
# produces bytes; `run ... rclone ...` records the rclone invocation.
DOCKER_LOG="$FIXTURE/docker.log"
cat > "$FIXTURE/docker" <<'MOCK'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$DOCKER_LOG"
if [[ -n "${EXPECTED_PASSPHRASE:-}" && "${BACKUP_PASSPHRASE:-}" != "$EXPECTED_PASSPHRASE" ]]; then
    echo 'backup passphrase was altered or not exported' >&2
    exit 1
fi
case "$1" in
    compose)
        if [[ "$2" == "version" ]]; then exit 0; fi
        # stdin may carry a tar stream for the config archive; drain it.
        cat >/dev/null
        printf 'encrypted-bytes'
        ;;
    exec) cat >/dev/null; printf 'encrypted-managed-bytes' ;;
    run) exit 0 ;;
esac
MOCK
chmod +x "$FIXTURE/docker"
export DOCKER="$FIXTURE/docker" DOCKER_LOG

# Missing settings are refused before anything runs.
sed -i '/^R2_BUCKET=/d' "$INSTALL_DIR/.env"
if bash "$REPO_ROOT/scripts/backup.sh" --dry-run >"$FIXTURE/missing.log" 2>&1; then
    echo "backup ran without R2_BUCKET" >&2; exit 1
fi
grep -q 'R2_BUCKET is not set' "$FIXTURE/missing.log"
printf 'R2_BUCKET=bucket\n' >> "$INSTALL_DIR/.env"

# --init fills in a missing passphrase once and never overwrites one.
sed -i '/^BACKUP_PASSPHRASE=/d' "$INSTALL_DIR/.env"
bash "$REPO_ROOT/scripts/backup.sh" --init > "$FIXTURE/init.log"
generated="$(sed -n 's/^BACKUP_PASSPHRASE=//p' "$INSTALL_DIR/.env")"
[[ "${#generated}" -eq 40 ]] || { echo "generated passphrase has length ${#generated}" >&2; exit 1; }
grep -q "BACKUP_PASSPHRASE=$generated" "$FIXTURE/init.log"
bash "$REPO_ROOT/scripts/backup.sh" --init | grep -q 'already set'
[[ "$(sed -n 's/^BACKUP_PASSPHRASE=//p' "$INSTALL_DIR/.env")" == "$generated" ]]
sed -i 's/^BACKUP_PASSPHRASE=.*/BACKUP_PASSPHRASE=sixteen-characters-long/' "$INSTALL_DIR/.env"

# Dry run explains the plan and touches nothing.
bash "$REPO_ROOT/scripts/backup.sh" --dry-run > "$FIXTURE/dry.log"
grep -q 'would upload both to r2:bucket/dreamtrans/' "$FIXTURE/dry.log"
grep -q 'older than 14 days' "$FIXTURE/dry.log"
test ! -e "$DOCKER_LOG"

# Application dotenv values must not be interpreted as shell syntax. Exercise
# the production failure and ensure even explicit shell commands are ignored.
cp "$INSTALL_DIR/.env" "$FIXTURE/plain.env"
cat >> "$INSTALL_DIR/.env" <<'ENV'
MAIL_FROM=DreamTrans <no-reply@example.test>
UNRELATED=$(touch "$INSTALL_DIR/executed")
touch "$INSTALL_DIR/executed"
ENV
bash "$REPO_ROOT/scripts/backup.sh" --dry-run > "$FIXTURE/dotenv.log"
test ! -e "$INSTALL_DIR/executed"
test ! -e "$DOCKER_LOG"

# Quoted secrets and shell metacharacters reach the backup process literally.
# Include CRLF, whitespace, comments, duplicate keys and no final newline.
cat >> "$INSTALL_DIR/.env" <<'ENV'
  export R2_ACCOUNT_ID = "acct" # account
R2_BUCKET='bucket' # bucket
R2_SECRET_ACCESS_KEY="secret#with=equals\"and\\slash"
  export BACKUP_PASSPHRASE = 'literal $HOME ${USER} $(touch "$INSTALL_DIR/executed") `id` #secret'
ENV
printf 'BACKUP_RETENTION_DAYS = 14 # retention # comment\r\nPOSTGRES_DB="dreamtrans"' >> "$INSTALL_DIR/.env"
cp "$INSTALL_DIR/.env" "$FIXTURE/literal.env"
bash "$REPO_ROOT/scripts/backup.sh" --init | grep -q 'already set'
cmp "$INSTALL_DIR/.env" "$FIXTURE/literal.env"
export EXPECTED_PASSPHRASE='literal $HOME ${USER} $(touch "$INSTALL_DIR/executed") `id` #secret'
bash "$REPO_ROOT/scripts/backup.sh" > "$FIXTURE/literal.log"
test ! -e "$INSTALL_DIR/executed"
grep -Fq 'RCLONE_CONFIG_R2_SECRET_ACCESS_KEY=secret#with=equals"and\slash' "$DOCKER_LOG"
grep -q 'delete r2:bucket/dreamtrans --min-age 14d' "$DOCKER_LOG"
if grep -Fq "$EXPECTED_PASSPHRASE" "$DOCKER_LOG"; then
    echo 'literal passphrase leaked into a docker argument' >&2; exit 1
fi
unset EXPECTED_PASSPHRASE
rm "$DOCKER_LOG"

# A leading hash is part of an unquoted dotenv value; escaped dollars inside
# double quotes must keep the same passphrase bytes as existing installations.
for assignment in 'BACKUP_PASSPHRASE=#literal$secret-with=equals' 'BACKUP_PASSPHRASE="#literal\$secret-with=equals"'; do
    cp "$FIXTURE/plain.env" "$INSTALL_DIR/.env"
    printf '%s\n' "$assignment" >> "$INSTALL_DIR/.env"
    EXPECTED_PASSPHRASE='#literal$secret-with=equals' bash "$REPO_ROOT/scripts/backup.sh" >/dev/null
done
cp "$FIXTURE/literal.env" "$INSTALL_DIR/.env"

# Install cron against the same dotenv fixture, without touching host cron.
export CRONTAB_FILE="$FIXTURE/crontab"
cat > "$FIXTURE/crontab" <<'MOCK'
#!/usr/bin/env bash
if [[ "${CRONTAB_FAIL:-}" == true ]]; then
    echo 'fixture: crontab installation refused' >&2
    exit 1
fi
if [[ "${1:-}" == -l ]]; then
    cat "$CRONTAB_FILE" 2>/dev/null
else
    cat > "$CRONTAB_FILE.next"
    mv "$CRONTAB_FILE.next" "$CRONTAB_FILE"
fi
MOCK
# Keep the executable separate from the stored crontab contents.
mkdir "$FIXTURE/bin"
mv "$FIXTURE/crontab" "$FIXTURE/bin/crontab"
chmod +x "$FIXTURE/bin/crontab"
export PATH="$FIXTURE/bin:$PATH"
printf '0 0 * * * unrelated-command\n' > "$CRONTAB_FILE"
bash "$REPO_ROOT/scripts/backup.sh" --install-cron > "$FIXTURE/cron.log"
bash "$REPO_ROOT/scripts/backup.sh" --install-cron >> "$FIXTURE/cron.log"
test "$(grep -c '15 3 \* \* \*' "$CRONTAB_FILE")" -eq 1
grep -q 'unrelated-command' "$CRONTAB_FILE"
test ! -e "$INSTALL_DIR/executed"

# Malformed backup values fail without disclosing the secret or replacing cron.
cp "$CRONTAB_FILE" "$FIXTURE/original-crontab"
printf '\nBACKUP_PASSPHRASE="private-secret-without-closing-quote\n' >> "$INSTALL_DIR/.env"
if bash "$REPO_ROOT/scripts/backup.sh" --install-cron > "$FIXTURE/invalid.log" 2>&1; then
    echo 'unterminated backup quote was accepted' >&2; exit 1
fi
grep -q 'invalid BACKUP_PASSPHRASE at .env line' "$FIXTURE/invalid.log"
if grep -q 'private-secret' "$FIXTURE/invalid.log"; then
    echo 'parse error disclosed a secret' >&2; exit 1
fi
cmp "$CRONTAB_FILE" "$FIXTURE/original-crontab"
cp "$FIXTURE/plain.env" "$INSTALL_DIR/.env"

# Installer retries expose the failure and print a standalone, quoted command
# that works with a custom installation directory containing spaces.
(
    backup_install_dir="$INSTALL_DIR with spaces"
    mkdir -p "$backup_install_dir"
    cp "$INSTALL_DIR/.env" "$backup_install_dir/.env"
    cp "$REPO_ROOT/scripts/backup.sh" "$backup_install_dir/backup.sh"
    chmod +x "$backup_install_dir/backup.sh"
    # shellcheck source=/dev/null
    source <(sed '$d' "$REPO_ROOT/scripts/install.sh")
    INSTALL_DIR="$backup_install_dir"
    export CRONTAB_FAIL=true
    configure_backup_cron > "$FIXTURE/installer.log" 2>&1
    grep -q 'fixture: crontab installation refused' "$FIXTURE/installer.log"
    grep -q 'Could not schedule the daily backup. Retry with:' "$FIXTURE/installer.log"
    printf '  INSTALL_DIR=%q %q --install-cron\n' "$INSTALL_DIR" "$INSTALL_DIR/backup.sh" > "$FIXTURE/retry-command"
    grep -Fxf "$FIXTURE/retry-command" "$FIXTURE/installer.log" >/dev/null
)

# A real run produces both encrypted artefacts and uploads them.
bash "$REPO_ROOT/scripts/backup.sh" > "$FIXTURE/run.log"
grep -q 'backup complete' "$FIXTURE/run.log"
ls "$INSTALL_DIR"/backups/dreamtrans-*.dump.enc >/dev/null
ls "$INSTALL_DIR"/backups/dreamtrans-*.config.tar.enc >/dev/null
grep -q 'pg_dump -U' "$DOCKER_LOG"
grep -q 'openssl enc -aes-256-cbc' "$DOCKER_LOG"
grep -q 'RCLONE_CONFIG_R2_ENDPOINT=https://acct.r2.cloudflarestorage.com' "$DOCKER_LOG"
grep -q 'copyto /backups/dreamtrans-.*\.dump\.enc r2:bucket/dreamtrans/' "$DOCKER_LOG"
grep -q 'copyto /backups/dreamtrans-.*\.config\.tar\.enc r2:bucket/dreamtrans/' "$DOCKER_LOG"
grep -q 'delete r2:bucket/dreamtrans --min-age 14d' "$DOCKER_LOG"
if grep -q 'sixteen-characters-long' "$DOCKER_LOG"; then
    echo "passphrase leaked into a docker argument" >&2; exit 1
fi

# Local rotation keeps only the newest BACKUP_LOCAL_KEEP dumps.
for i in 1 2 3; do
    touch -d "-$((i * 60)) minutes" "$INSTALL_DIR/backups/dreamtrans-old$i.dump.enc"
done
BACKUP_LOCAL_KEEP=2 bash "$REPO_ROOT/scripts/backup.sh" >/dev/null
count="$(ls "$INSTALL_DIR"/backups/dreamtrans-*.dump.enc | wc -l)"
if [[ "$count" -ne 2 ]]; then
    echo "expected 2 local dumps after rotation, found $count" >&2; exit 1
fi

echo "Backup script checks passed"

# Managed conversion keeps the recorded database identity even when the Compose
# database service is named postgres instead of db. Local full snapshots rotate.
mkdir -p "$INSTALL_DIR/.bluegreen"
printf '%s\n' '{"database_id":"restored-postgres-id"}' > "$INSTALL_DIR/.bluegreen/state.json"
cat > "$INSTALL_DIR/release.py" <<'PYTHON'
import pathlib, sys
pathlib.Path(sys.argv[sys.argv.index('--output')+1]).write_bytes(b'full snapshot fixture')
PYTHON
printf old > "$INSTALL_DIR/backups/dreamtrans-20000101-000000.full.tar.enc"
BACKUP_LOCAL_KEEP=1 bash "$REPO_ROOT/scripts/backup.sh" > "$FIXTURE/managed.log"
grep -q 'exec -i -e BACKUP_PASSPHRASE restored-postgres-id openssl enc' "$DOCKER_LOG"
grep -q 'full.tar.enc r2:bucket/dreamtrans/' "$DOCKER_LOG"
test ! -e "$INSTALL_DIR/backups/dreamtrans-20000101-000000.full.tar.enc"
test "$(find "$INSTALL_DIR/backups" -name '*.full.tar.enc' | wc -l)" -eq 1
printf 'Managed backup identity and retention checks passed\n'
