# Task 13 k6 容量测试包

本机 smoke (10s 短曲线验证脚本形态) + staging 真阈值 (5min / 30s) 双模式.
plan 行 1742 要求: 不可复用生产数据 → 本机用 `exam_system_loadtest` schema 隔离.

## 文件布局

- `loadtest/{start_peak,draft_steady,submit_peak}.js` - k6 三场景脚本
  - SMOKE=1 env 切换 10s 短曲线 (smoke); 默认 30s/5m 真阈值 (staging)
- `loadtest/config.yaml` - Go serve loadtest 配置 (admin auth 关, search_path=loadtest)
- `scripts/loadtest/prepare.py` - 写脱敏 paper + 直插 exam_runs + 预 start 500 session
- `scripts/loadtest/summarize.py` - k6 JSON 输出 -> summary.json + passed 验收门
- `scripts/loadtest/run_smoke.sh` - 本机 smoke runner (一站式)

## 路径 C: 直连 PG 绕 admin openRun stub

`prepare.py` 直插 `exam_runs` 行 (绕过 Task 9 admin openRun stub, 用户决断保留 stub
未补). 同时走 admin savePaper API (handler 非 stub) 写 paper JSON. start API 真实工作,
预 start 500 session 拿 session_token 给 draft/submit 场景.

## 本机 smoke (10s 短曲线, 形态验证)

```bash
# 1. 启 Go serve 指向 loadtest schema
./exam-server migrate -config loadtest/config.yaml
./exam-server serve -config loadtest/config.yaml -bind 127.0.0.1:18080 -static loadtest/frontend &

# 2. 一站式 smoke (清 schema + prepare + k6 三场景 10s + summarize)
bash scripts/loadtest/run_smoke.sh
```

预期: `loadtest/results/summary.json` passed=true (本机 p95 ~3-6ms 远超阈值).

## staging 真阈值 (5min draft_steady + 30s start_peak/submit_peak)

复制本机 loadtest/ 到 staging + 改 config.yaml DSN / BASE_URL, 跑:

```bash
cd loadtest && for s in start_peak draft_steady submit_peak; do
  BASE_URL=$STAGING k6 run --out json=results/${s}.json $s.js
done
python3 ../scripts/loadtest/summarize.py results
```

## 阈值 (plan 1771/1782)

| 曲线 | 阈值 | 期望 RPS / 持续 |
|---|---|---|
| start_peak | err<1% p95<500ms | 500 并发 30s |
| draft_steady | err<1% p95<750ms | 50 VU 5min 持续 |
| submit_peak | err<1% p95<750ms | 500 并发 30s |

## 本机 smoke 实测 (2026-07-26)

| 曲线 | p95 ms | fail_rate | submit RPS | 结果 |
|---|---|---|---|---|
| start | 3.5 | 0.0 | - | ✅ |
| draft | 5.4 | 0.0 | - | ✅ |
| submit | 2.6 | 0.0 | 8904 | ✅ |

submit 单次 smoke 触发 89045 个请求 / 10s, 远超 plan 阈值.

## 不入 git (合规: plan 行 1810)

`.gitignore` 已忽略: `loadtest/frontend/` (纸卷) / `loadtest/scenarios/` (session token) /
`loadtest/results/*` (k6 原始输出). 仅 `loadtest/results/summary.json` 脱敏汇总入 git.
