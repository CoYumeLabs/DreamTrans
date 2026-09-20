#!/usr/bin/env bash
# Production bootstrap uses Docker + the extracted Go executable, never Python.
set -Eeuo pipefail
repository=$(cd "$(dirname "$0")/../.." && pwd)
: "${DREAMTRANS_CTL:?Build dreamtransctl first}"
work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT
mkdir -p "$work/bin" "$work/install"
export FIXTURE_CTL="$DREAMTRANS_CTL" FIXTURE_ROOT="$work"
cat > "$work/bin/id" <<'SH'
#!/usr/bin/env bash
echo 0
SH
cat > "$work/bin/docker" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$FIXTURE_ROOT/docker.log"
case "$1" in
    pull) [[ ${FIXTURE_PULL_FAIL:-0} == 0 ]] ;;
    image) printf 'sha256:%064d\n' 1 ;;
    create) echo fixture-extract ;;
    cp)
        case "$2" in
            */dreamtransctl) cp "$FIXTURE_CTL" "$3" ;;
            */backup.sh) printf '#!/bin/sh\necho go-backup-fixture\n' > "$3" ;;
            *) exit 1 ;;
        esac ;;
    rm) : ;;
    *) exit 1 ;;
esac
SH
chmod 0700 "$work/bin/"*
export PATH="$work/bin:$PATH"
printf 'retained-config\n' > "$work/install/.env"
printf 'previous-backup\n' > "$work/install/backup.sh"
reference="ghcr.io/coyumelabs/dreamtrans@sha256:$(printf '%064d' 1)"
bash "$repository/scripts/install-ops.sh" "$reference" --dir "$work/install"
cmp "$FIXTURE_CTL" "$work/install/dreamtransctl"
test "$(cat "$work/install/.env")" = retained-config
test "$(cat "$work/install/backup.sh.previous")" = previous-backup
bash "$repository/scripts/install-ops.sh" "$reference" --dir "$work/install"
cmp "$FIXTURE_CTL" "$work/install/dreamtransctl.previous"
before=$(sha256sum "$work/install/backup.sh")
if FIXTURE_PULL_FAIL=1 bash "$repository/scripts/install-ops.sh" "$reference" --dir "$work/install"; then
    echo 'Bootstrap accepted failed image pull' >&2
    exit 1
fi
test "$(sha256sum "$work/install/backup.sh")" = "$before"
if grep -Eq '(python|docker run|volume create|network create)' "$work/docker.log"; then
    echo 'Bootstrap unexpectedly started production services' >&2
    exit 1
fi
echo 'Go bootstrap verified: immutable extraction, repeated adoption, preserved config and failed-pull isolation'
