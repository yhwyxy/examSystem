#!/usr/bin/env bash
# Backup a quiesced deployment before migration. Credentials remain in env.
# Default: Docker Compose; BACKUP_DATABASE_URL enables native PostgreSQL.
# Only atomically completed, owned backup directories are eligible for retention.
set -euo pipefail
umask 077

COMPOSE_FILE="${COMPOSE_FILE:-docker-compose.prod.yml}"
BACKUP_ROOT="${1:-backups}"
BACKUP_KEEP="${BACKUP_KEEP:-14}"
BACKUP_CONFIG_FILE="${BACKUP_CONFIG_FILE:-${EXAM_CONFIG_FILE:-config.production.yaml}}"
if [[ ! "$BACKUP_KEEP" =~ ^[1-9][0-9]{0,3}$ ]] || ((BACKUP_KEEP > 1000)); then
  echo "ERROR: BACKUP_KEEP must be 1..1000" >&2
  exit 1
fi
[[ -d data ]] || { echo "ERROR: data/ is missing" >&2; exit 1; }
[[ -f "$BACKUP_CONFIG_FILE" ]] || { echo "ERROR: deployment config is missing" >&2; exit 1; }
if command -v sha256sum >/dev/null 2>&1; then
  HASH_KIND=sha256sum
elif command -v shasum >/dev/null 2>&1; then
  HASH_KIND=shasum
else
  echo "ERROR: SHA-256 tool is required" >&2
  exit 1
fi
mkdir -p "$BACKUP_ROOT"
LOCK_DIR="$BACKUP_ROOT/.backup.lock"
if ! mkdir "$LOCK_DIR" 2>/dev/null; then
  echo "ERROR: backup root is locked; inspect its recorded process before removing a stale lock" >&2
  exit 1
fi
STAGING=""
trap 'if [[ -n "$STAGING" && -d "$STAGING" ]]; then rm -rf -- "$STAGING"; fi; rm -f -- "$LOCK_DIR/pid"; rmdir "$LOCK_DIR"' EXIT
printf '%s\n' "$$" > "$LOCK_DIR/pid"
STAGING="$(mktemp -d "$BACKUP_ROOT/.incomplete-XXXXXXXX")"
DEST="$BACKUP_ROOT/$(date +%Y%m%d-%H%M%S)-${STAGING##*.incomplete-}"
[[ ! -e "$DEST" ]] || { echo "ERROR: backup destination exists" >&2; exit 1; }

if [[ -n "${BACKUP_DATABASE_URL:-}" ]]; then
  "${BACKUP_PYTHON:-python3}" "$(dirname "${BASH_SOURCE[0]}")/pg_native.py" \
    "${PG_DUMP_BIN:-pg_dump}" --no-password | gzip > "$STAGING/exam_system.sql.gz"
else
  docker compose -f "$COMPOSE_FILE" exec -T postgres \
    sh -c 'exec pg_dump -U "$POSTGRES_USER" "$POSTGRES_DB"' | gzip > "$STAGING/exam_system.sql.gz"
fi
COPYFILE_DISABLE=1 tar czf "$STAGING/data.tar.gz" data/
cp "$BACKUP_CONFIG_FILE" "$STAGING/config.production.yaml"
if [[ "$HASH_KIND" = sha256sum ]]; then
  (cd "$STAGING" && sha256sum exam_system.sql.gz data.tar.gz config.production.yaml > SHA256SUMS)
else
  (cd "$STAGING" && shasum -a 256 exam_system.sql.gz data.tar.gz config.production.yaml > SHA256SUMS)
fi
printf '%s\n' 'examSystem-backup-v1' > "$STAGING/BACKUP_COMPLETE"
mv "$STAGING" "$DEST"

completed=()
for candidate in "$BACKUP_ROOT"/*; do
  [[ "$candidate" != "$DEST" ]] || continue
  [[ -d "$candidate" && -f "$candidate/BACKUP_COMPLETE" ]] || continue
  [[ "${candidate##*/}" =~ ^[0-9]{8}-[0-9]{6}- ]] || continue
  IFS= read -r marker < "$candidate/BACKUP_COMPLETE"
  [[ "$marker" = examSystem-backup-v1 && -f "$candidate/SHA256SUMS" ]] || continue
  completed+=("$candidate")
done
remove_count=$((${#completed[@]} + 1 - BACKUP_KEEP))
for ((index=0; index<remove_count; index++)); do
  rm -rf -- "${completed[index]}"
done
printf 'backup complete: %s\n' "$DEST"
