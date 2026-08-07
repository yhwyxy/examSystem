#!/usr/bin/env bash
# scripts/load-run.sh - 生产机导入开发机构建的镜像并启动 (全程不联网构建)。
#
# 前提:
#   - 默认优先使用 deploy-images.tar.part-000 ... + deploy-images.tar.sha256;
#   - 若没有分卷, 仍兼容完整 deploy-images.tar;
#   - 生产机上的 .env / config.production.yaml / 模型等已另行就绪。
#
# 用法:
#   ./scripts/load-run.sh                    # 查找 deploy-images.tar 的分卷或完整包
#   ./scripts/load-run.sh images.tar         # 查找 images.tar.part-* 或 images.tar
#
# 要求: Bash、sha256sum、Docker，以及 docker compose v2 或 docker-compose v1.27+。

set -euo pipefail

cd "$(dirname "$0")/.."

TAR="${1:-deploy-images.tar}"
TAR_DIR="$(dirname "$TAR")"
TAR_BASE="$(basename "$TAR")"
PART_PREFIX="${TAR_BASE}.part-"
CHECKSUM_BASE="${TAR_BASE}.sha256"
CHECKSUM_FILE="${TAR_DIR}/${CHECKSUM_BASE}"
COMPOSE_FILE="docker-compose.prod.yml"

fail() {
  echo "ERROR: $*" >&2
  exit 1
}

command -v docker >/dev/null 2>&1 || fail "未找到 docker"

# 选择 compose 命令 (优先 v2 的 docker compose)。
if docker compose version >/dev/null 2>&1; then
  COMPOSE_CMD=(docker compose)
elif command -v docker-compose >/dev/null 2>&1; then
  COMPOSE_CMD=(docker-compose)
else
  fail "未找到 docker compose / docker-compose, 见 docs/deployment-docker.md"
fi
COMPOSE_VERSION="$("${COMPOSE_CMD[@]}" version)"

echo "==> [1/6] 检查配置与镜像包"
[ -f .env ] || fail "缺少 .env — 请先在项目目录就绪 .env"
[ -f config.production.yaml ] || fail "缺少 config.production.yaml — 请先就绪"

shopt -s nullglob
ACTUAL_PARTS=("${TAR_DIR}/${PART_PREFIX}"[0-9][0-9][0-9])
shopt -u nullglob

if [ "${#ACTUAL_PARTS[@]}" -gt 0 ]; then
  command -v sha256sum >/dev/null 2>&1 || fail "加载分卷需要 sha256sum"
  [ -f "$CHECKSUM_FILE" ] || fail "发现分卷但缺少校验文件: ${CHECKSUM_FILE}"

  MANIFEST_PARTS=()
  EXPECTED_INDEX=0
  while IFS= read -r CHECKSUM_LINE || [ -n "$CHECKSUM_LINE" ]; do
    [ "${#CHECKSUM_LINE}" -ge 67 ] || fail "校验文件格式错误: ${CHECKSUM_FILE}"
    HASH="${CHECKSUM_LINE:0:64}"
    MARKER="${CHECKSUM_LINE:64:2}"
    PART_NAME="${CHECKSUM_LINE:66}"

    [[ "$HASH" =~ ^[0-9a-fA-F]{64}$ ]] || fail "校验文件包含非法 SHA-256: ${CHECKSUM_FILE}"
    [ "$MARKER" = " *" ] || [ "$MARKER" = "  " ] || fail "校验文件格式错误: ${CHECKSUM_FILE}"
    [ "$PART_NAME" = "$(printf '%s%03d' "$PART_PREFIX" "$EXPECTED_INDEX")" ] ||
      fail "分卷必须从 ${PART_PREFIX}000 开始连续编号: 当前为 ${PART_NAME}"
    [[ "$PART_NAME" != */* ]] || fail "校验文件中的分卷必须是文件名, 不能包含路径: ${PART_NAME}"

    MANIFEST_PARTS+=("${TAR_DIR}/${PART_NAME}")
    EXPECTED_INDEX=$((EXPECTED_INDEX + 1))
  done < "$CHECKSUM_FILE"

  [ "${#MANIFEST_PARTS[@]}" -gt 0 ] || fail "校验文件未包含任何分卷: ${CHECKSUM_FILE}"
  [ "${#ACTUAL_PARTS[@]}" -eq "${#MANIFEST_PARTS[@]}" ] ||
    fail "实际分卷数量与校验文件不一致"

  for INDEX in "${!MANIFEST_PARTS[@]}"; do
    [ "${ACTUAL_PARTS[$INDEX]}" = "${MANIFEST_PARTS[$INDEX]}" ] ||
      fail "实际分卷与校验文件不一致: ${ACTUAL_PARTS[$INDEX]}"
  done

  echo "==> [2/6] 校验 ${#MANIFEST_PARTS[@]} 个分卷"
  (cd "$TAR_DIR" && sha256sum -c "$CHECKSUM_BASE")

  echo "==> [3/6] 流式导入分卷镜像"
  cat -- "${MANIFEST_PARTS[@]}" | docker load
  IMAGE_SOURCE="${TAR_DIR}/${PART_PREFIX}*"
elif [ -f "$TAR" ]; then
  echo "==> [2/6] 使用兼容模式: 完整镜像包无需分卷校验"
  echo "==> [3/6] 导入完整镜像 (${TAR})"
  docker load -i "$TAR"
  IMAGE_SOURCE="$TAR"
else
  fail "未找到 ${TAR_DIR}/${PART_PREFIX}000 或完整镜像包 ${TAR}"
fi

echo "==> [4/6] 校验镜像与启动 postgres"
for IMAGE in examsystem:latest examsystem-worker:latest postgres:18-alpine; do
  docker image inspect "$IMAGE" >/dev/null 2>&1 ||
    fail "缺少镜像: ${IMAGE} — 检查 ${IMAGE_SOURCE} 是否来自最新 build-export.sh"
done
"${COMPOSE_CMD[@]}" -f "$COMPOSE_FILE" up -d --no-build postgres

echo "==> [5/6] 一次性迁移"
"${COMPOSE_CMD[@]}" -f "$COMPOSE_FILE" run --rm exam migrate --config /app/config.yaml

echo "==> [6/6] 启动全栈"
"${COMPOSE_CMD[@]}" -f "$COMPOSE_FILE" up -d --no-build

echo ""
echo "==> 状态:"
"${COMPOSE_CMD[@]}" -f "$COMPOSE_FILE" ps
echo ""
echo "验证: curl http://127.0.0.1:8000/api/health   (compose: ${COMPOSE_VERSION})"
