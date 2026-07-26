#!/usr/bin/env bash
# Task 13 k6 容量测试 smoke runner (本机 10s 短曲线验证, 真阈值留给 staging)
# 前置: Go serve 已起 (loadtest/config.yaml) + prepare.py 已写 3 scenarios
# 用法: ./scripts/loadtest/run_smoke.sh [base_url]
set -euo pipefail
BASE_URL="${1:-http://127.0.0.1:18080}"
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
RESULTS="$ROOT/loadtest/results"
mkdir -p "$RESULTS"

echo "=== Step 1: 清 loadtest schema (保证 smoke 数据干净) ==="
psql "postgres://exam_migrator:exam_migrator_dev@127.0.0.1:5432/exam_system?sslmode=disable&options=-c%20search_path%3Dloadtest" \
  -c "TRUNCATE exam_sessions, submissions, grading_jobs, exam_runs RESTART IDENTITY CASCADE;" >/dev/null

echo "=== Step 2: 跑 prepare.py (50 user smoke 形态) ==="
cd "$ROOT" && EXAM_LT_USERS=50 \
  EXAM_LT_BASE_URL="$BASE_URL" \
  EXAM_LT_DB_URL="postgres://exam_migrator:exam_migrator_dev@127.0.0.1:5432/exam_system?sslmode=disable&options=-c%20search_path%3Dloadtest" \
  python3 scripts/loadtest/prepare.py

echo "=== Step 3: k6 三场景 smoke (10s 短曲线) ==="
for scene in start_peak draft_steady submit_peak; do
  # 映射到 summarize.py 期望的 start.json/draft.json/submit.json
  case $scene in
    start_peak)    out=start  ;;
    draft_steady)  out=draft  ;;
    submit_peak)   out=submit ;;
  esac
  echo "--- $scene (10s smoke) -> ${out}.json ---"
  cd "$ROOT/loadtest" && BASE_URL="$BASE_URL" k6 run \
    --out "json=$RESULTS/${out}.json" \
    --env SMOKE=1 \
    "$scene.js" || echo "k6 $scene failed (continue)"
done

echo "=== Step 4: summarize ==="
cd "$ROOT" && python3 scripts/loadtest/summarize.py "$RESULTS"
echo "=== summary.json ==="
cat "$RESULTS/summary.json" 2>/dev/null || echo "no summary - 检查 k6 输出"
