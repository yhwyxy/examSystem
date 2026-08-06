#!/usr/bin/env bash
# scripts/backup-prod.sh - 生产备份 (PG dump + 业务数据 + 生产配置 + SHA-256 清单)
#
# 用法:
#   ./scripts/backup-prod.sh [备份根目录]     # 默认 backups/
#   COMPOSE_FILE=docker-compose.prod.yml ./scripts/backup-prod.sh
#
# 在部署机运行 (需要 docker compose 与宿主机 tar/gzip/sha256sum).
# 每份备份目录含:
#   exam_system.sql.gz      PG 逻辑备份
#   data.tar.gz             业务数据 (data/papers + data/exam_runs + token)
#   config.production.yaml  生产配置副本 (不含 .env; 密钥另存 .env, 单独保管)
#   SHA256SUMS              上述文件的 sha256 清单 (校验完整性)
#
# 保留最近 BACKUP_KEEP (默认 14) 份. 计划任务示例:
#   0 2 * * * cd /path/to/examSystem && ./scripts/backup-prod.sh >> backups/backup.log 2>&1
set -euo pipefail

COMPOSE_FILE="${COMPOSE_FILE:-docker-compose.prod.yml}"
BACKUP_ROOT="${1:-backups}"
BACKUP_KEEP="${BACKUP_KEEP:-14}"
TS="$(date +%Y%m%d-%H%M%S)"
DEST="$BACKUP_ROOT/$TS"
mkdir -p "$DEST"

echo "==> [backup] 目标: $DEST"

# 1) PG 逻辑备份 (容器内 pg_dump, 不经宿主端口)
docker compose -f "$COMPOSE_FILE" exec -T postgres \
  pg_dump -U "${PG_USER:-exam_super}" "${PG_DB:-exam_system}" | gzip > "$DEST/exam_system.sql.gz"

# 2) 业务数据 (试卷 + run 快照 + token; 目录相对仓库根)
tar czf "$DEST/data.tar.gz" data/

# 3) 生产配置副本 (密钥在 .env 不备份, 单独用密码管理器/离线介质保存)
if [ -f config.production.yaml ]; then
  cp config.production.yaml "$DEST/config.production.yaml"
else
  echo "WARN: config.production.yaml 不存在, 跳过"
fi

# 4) SHA-256 清单
SHA_BIN="$(command -v sha256sum || command -v shasum || true)"
if [ -z "$SHA_BIN" ]; then
  echo "ERROR: 无 sha256sum/shasum, 跳过清单" >&2
else
  (cd "$DEST" && { "$SHA_BIN" -a 256 ./* 2>/dev/null || "$SHA_BIN" ./*; } > SHA256SUMS)
fi

# 5) 只留最近 N 份
ls -1dt "$BACKUP_ROOT"/*/ 2>/dev/null | tail -n +$((BACKUP_KEEP + 1)) | xargs -r rm -rf

echo "==> [backup] 完成:"
cat "$DEST/SHA256SUMS" 2>/dev/null || ls -la "$DEST"
