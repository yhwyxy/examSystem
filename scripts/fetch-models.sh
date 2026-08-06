#!/usr/bin/env bash
# scripts/fetch-models.sh - 部署时同步 bge-reranker-v2-m3 到宿主 ./models
#
# 走 HuggingFace 国内镜像 (hf-mirror.com) + 清华 PyPI, 避免直连 HF/GitHub 超时;
# 通过一次性容器下载, 不要求宿主机装 Python。
#
# 下载后目录结构即 RERANKER_MODEL 所需 (含 config.json), 与 compose 的
# ./models:/models 挂载、RERANKER_MODEL=/models/bge-reranker-v2-m3 对齐:
#   ./models/bge-reranker-v2-m3/{config.json, pytorch_model.bin, sentence_bert_config.json, ...}
#
# 用法:
#   ./scripts/fetch-models.sh                         # 默认 BAAI/bge-reranker-v2-m3 -> ./models/
#   MODEL_REPO=... MODEL_DIR=... ./scripts/fetch-models.sh
#   MODEL_REPO=BAAI/bge-reranker-base ./scripts/fetch-models.sh

set -euo pipefail

# 代理变量会经 docker daemon 注入容器; 与 worker 运行约束一致 (见
# scoring_worker README: 代理下模型加载会静默回退词法评分), 下载同样 unset,
# 避免走代理导致失败或半成品。
unset HTTP_PROXY HTTPS_PROXY ALL_PROXY http_proxy https_proxy all_proxy || true

MODEL_REPO="${MODEL_REPO:-BAAI/bge-reranker-v2-m3}"
MODEL_DIR="${MODEL_DIR:-./models/bge-reranker-v2-m3}"
HF_ENDPOINT="${HF_ENDPOINT:-https://hf-mirror.com}"
PIP_INDEX_URL="${PIP_INDEX_URL:-https://pypi.tuna.tsinghua.edu.cn/simple}"

# 容器内目标目录 (huggingface-cli --local-dir 需绝对路径): 相对 ./models 子路径
REL_DIR="${MODEL_DIR#./models/}"
REL_DIR="${REL_DIR#models/}"
LOCAL_DIR="/models/${REL_DIR%/}"

mkdir -p "$(dirname "$MODEL_DIR")"

echo ">> 下载 ${MODEL_REPO} -> ${MODEL_DIR}"
echo "   HF_ENDPOINT=${HF_ENDPOINT}  PIP_INDEX_URL=${PIP_INDEX_URL}"
echo "   (模型约 2GB, 视带宽可能需要数分钟; 可后台运行)"

docker run --rm \
  -e HF_ENDPOINT="${HF_ENDPOINT}" \
  -e HF_HUB_ENABLE_HF_TRANSFER=1 \
  -e PIP_INDEX_URL="${PIP_INDEX_URL}" \
  -v "$(pwd)/models:/models" \
  python:3.12-slim bash -euxo pipefail -c "
    pip install -q --no-cache-dir 'huggingface_hub[cli]>=0.23' hf_transfer
    huggingface-cli download --local-dir '${LOCAL_DIR}' '${MODEL_REPO}'
    test -f '${LOCAL_DIR}/config.json' || { echo '缺少 config.json, 下载不完整' >&2; exit 1; }
  "

echo ">> 完成: ${MODEL_DIR}"
