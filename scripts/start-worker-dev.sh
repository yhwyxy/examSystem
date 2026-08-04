#!/usr/bin/env bash
# 手动启动 scoring_worker (开发模式): 一条命令设置环境变量并拉起 worker.
# 用法: bash scripts/start-worker-dev.sh [--preflight]
set -euo pipefail
cd "$(dirname "$0")/.."

export DATABASE_URL="${DATABASE_URL:-postgres://exam_app:exam_app_dev@127.0.0.1:5432/exam_system?sslmode=disable}"
export WORKER_ID="${WORKER_ID:-dev-worker-1}"
export MULTIPLE_CHOICE_PARTIAL="${MULTIPLE_CHOICE_PARTIAL:-true}"
export RERANK_USE_REMOTE="${RERANK_USE_REMOTE:-false}"

PY="${PY:-.venv/bin/python}"
echo "==> starting scoring_worker (${PY})"
exec "$PY" -m scoring_worker "$@"
