#!/usr/bin/env bash
# scripts/build-export.sh - 开发机构建生产镜像 (linux/amd64) 并导出分卷 tar。
#
# 背景: 生产机无法访问 GitHub / Docker Hub 构建通道不稳; 改为开发机
# (可访问 GitHub + 国内源) 一次性构建, docker save 导出 tar, 自行拷到生产机 load。
#
# 用法:
#   ./scripts/build-export.sh                   # 构建+导出 deploy-images.tar.part-000 ...
#   MODE=remote ./scripts/build-export.sh       # remote_reranker/llm 评分: 不带 torch, 镜像更小
#   TAR=images.tar ./scripts/build-export.sh    # 自定义导出文件名
#   SPLIT_SIZE=2000M ./scripts/build-export.sh  # 自定义单卷大小 (默认 3900M, 兼容 FAT32)
#   KEEP_TAR=true ./scripts/build-export.sh     # 分卷后保留原始完整 tar
#
# 说明:
#   - 生产机是 x86_64, 开发机若是 Apple Silicon 必须 --platform linux/amd64;
#     amd64 构建走 QEMU 模拟, worker 的 torch 安装较慢 (首次约 20-40 分钟)。
#   - MODE=local (默认): worker 带 torch/sentence-transformers (本地语义模型);
#     MODE=remote: 不带, 镜像轻量 (远程 reranker / LLM 评分方式)。
#   - 默认产物为 deploy-images.tar.part-000 ... + deploy-images.tar.sha256。
#   - 生产机可把分卷直接通过管道交给 docker load, 无需合并出完整 tar。
#   - 原始 deploy-images.tar 默认在分卷校验完成后删除; KEEP_TAR=true 可保留。

set -euo pipefail

cd "$(dirname "$0")/.."

MODE="${MODE:-local}"
PLATFORM="${PLATFORM:-linux/amd64}"
TAR="${TAR:-deploy-images.tar}"
SPLIT_SIZE="${SPLIT_SIZE:-3900M}"
KEEP_TAR="${KEEP_TAR:-false}"
PART_PREFIX="${TAR}.part-"
CHECKSUM_FILE="${TAR}.sha256"
TAR_DIR="$(dirname "$TAR")"
TAR_BASE="$(basename "$TAR")"

case "$KEEP_TAR" in
  true|false) ;;
  *) echo "KEEP_TAR 只能是 true 或 false" >&2; exit 1 ;;
esac

case "$MODE" in
  local)  WORKER_EXTRAS="sentence-transformers>=3.0 torch>=2.2,<3" ;;
  remote) WORKER_EXTRAS="" ;;
  *) echo "MODE 只能是 local 或 remote" >&2; exit 1 ;;
esac

echo "==> [1/5] 构建 exam 镜像 (${PLATFORM})"
docker build --platform "$PLATFORM" -t examsystem:latest .

echo "==> [2/5] 构建 worker 镜像 (${PLATFORM}, MODE=${MODE})"
BUILD_ARGS=()
if [ -n "$WORKER_EXTRAS" ]; then
  BUILD_ARGS=(--build-arg "WORKER_EXTRAS=${WORKER_EXTRAS}")
fi
docker build --platform "$PLATFORM" "${BUILD_ARGS[@]}" \
  -f Dockerfile.worker -t examsystem-worker:latest .

echo "==> [3/5] 拉取 postgres 基础镜像 (${PLATFORM})"
docker pull --platform "$PLATFORM" postgres:18-alpine

echo "==> [4/5] 导出镜像 -> ${TAR}"
docker save examsystem:latest examsystem-worker:latest postgres:18-alpine -o "$TAR"
ls -lh "$TAR"

echo "==> [5/5] 分卷镜像 (${SPLIT_SIZE}/卷)"
# 清理同名旧分卷, 防止本次分卷较少时残留文件被误加载。
rm -f -- "${PART_PREFIX}"* "$CHECKSUM_FILE"
split -d -a 3 -b "$SPLIT_SIZE" "$TAR" "$PART_PREFIX"
# 校验文件只记录分卷文件名, 因此复制到生产机其他目录后仍可直接校验。
(
  cd "$TAR_DIR"
  sha256sum "${TAR_BASE}.part-"* > "${TAR_BASE}.sha256"
  sha256sum -c "${TAR_BASE}.sha256"
)
ls -lh "${PART_PREFIX}"* "$CHECKSUM_FILE"

if [ "$KEEP_TAR" = "false" ]; then
  rm -f -- "$TAR"
  echo "==> 已删除完整 tar; 如需保留可设置 KEEP_TAR=true"
fi

PART_NAME="$(basename "$PART_PREFIX")"
CHECKSUM_NAME="$(basename "$CHECKSUM_FILE")"

echo ""
echo "完成。把 ${PART_PREFIX}* 和 ${CHECKSUM_FILE} 拷到生产机项目目录 (/opt/examSystem/)。"
echo "生产机执行:"
echo "  cd /opt/examSystem"
echo "  sha256sum -c ${CHECKSUM_NAME}"
echo "  cat ${PART_NAME}* | docker load"
echo "  docker compose -f docker-compose.prod.yml up -d --no-build"
