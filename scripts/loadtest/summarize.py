#!/usr/bin/env python3
"""Task 13 k6 容量测试 summarize.py: 从 k6 JSON 输出生成 summary.json + passed 验收门.

plan 行 1731: 不可手工编辑 summary.json - 必须脚本生成.
plan 行 1810: 不提交 session token / 原始生产数据 - summary.json 仅写脱敏汇集值.

用法: python summarize.py <results_dir> [results_dir2 ...]
  每个 results_dir 含 start.json / draft.json / submit.json (k6 --out json=)
  产 <results_dir>/summary.json, 各曲线 passed+阈值.
"""
from __future__ import annotations
import json, sys
from pathlib import Path

# k6 v2 JSON: metric="http_req_duration", tag.name=start/draft/submit
THRESHOLDS = {
    "start":  ("start",  500, 0.01),
    "draft":  ("draft",  750, 0.01),
    "submit": ("submit", 750, 0.01),
}

def _load_metrics(path: Path) -> dict:
    """k6 JSON 输出每行一个 Metric/Point; 聚成 {metric_name: {rate,p(95),...}}."""
    out: dict[str, dict] = {}
    for line in path.read_text().splitlines():
        try: rec = json.loads(line)
        except json.JSONDecodeError: continue
        if rec.get("type") != "Point": continue
        m = rec.get("metric", "")
        d = rec.get("data", {})
        if m not in out: out[m] = {"count": 0, "sum": 0.0, "failed": 0, "dur_sum": 0.0}
        if m in ("http_req_failed",):
            out[m]["count"] += 1
            if d.get("value", 0) > 0: out[m]["failed"] += 1
        elif m.startswith("http_req_duration"):
            out[m]["count"] += 1
            out[m]["dur_sum"] += float(d.get("value", 0))
    return out

def _percentile(vals: list[float], p: float) -> float:
    if not vals: return 0.0
    s = sorted(vals)
    k = (len(s) - 1) * p / 100.0
    f = int(k)
    c = min(f + 1, len(s) - 1)
    return s[f] + (s[c] - s[f]) * (k - f)

def _collect_jsonl(path: Path, metric_prefix: str = "") -> list[dict]:
    out = []
    for line in path.read_text().splitlines():
        try: rec = json.loads(line)
        except json.JSONDecodeError: continue
        if rec.get("type") != "Point": continue
        m = rec.get("metric", "")
        if metric_prefix and not m.startswith(metric_prefix): continue
        out.append(rec)
    return out


def summarize_one(results_dir: Path) -> dict:
    """读 <results_dir>/{start,draft,submit}.json -> 产 <results_dir>/summary.json.
    k6 v2 JSON: metric 字段为 http_req_duration / http_req_failed,
    真实曲线区分在 data.tags.name (start/draft/submit)."""
    curves: dict = {}
    for k, (tag_name, p95_max, fail_max) in THRESHOLDS.items():
        fp = results_dir / f"{k}.json"
        if not fp.exists():
            curves[k] = {"present": False, "passed": False, "error": "k6 json missing"}
            continue
        dur_pts = _collect_jsonl(fp, "http_req_duration")
        durs = [float(r["data"].get("value", 0)) for r in dur_pts
                if r.get("metric") == "http_req_duration"
                and r["data"].get("tags", {}).get("name") == tag_name]
        if not durs:
            curves[k] = {"present": True, "passed": False,
                         "error": f"http_req_duration name={tag_name} no points"}
            continue
        p95 = _percentile(durs, 95)
        fail_pts = [r for r in _collect_jsonl(fp, "http_req_failed")
                    if r["data"].get("tags", {}).get("name") == tag_name]
        fail_total = len(fail_pts)
        fail_bad = sum(1 for r in fail_pts if r["data"].get("value", 0) > 0)
        rate = fail_bad / fail_total if fail_total else 0.0
        passed = (p95 <= p95_max) and (rate < fail_max)
        curves[k] = {"present": True, "p95_ms": round(p95, 1),
                    "fail_rate": round(rate, 4), "p95_max": p95_max,
                    "fail_max": fail_max, "passed": passed}
    summary = {"passed": all(c.get("passed") for c in curves.values()),
              "curves": curves}
    (results_dir / "summary.json").write_text(json.dumps(summary, ensure_ascii=False, indent=2))
    return summary

def main() -> int:
    if len(sys.argv) < 2:
        print("用法: summarize.py <results_dir> [results_dir2 ...]", file=sys.stderr)
        return 2
    overall = True
    for d in sys.argv[1:]:
        s = summarize_one(Path(d))
        print(f"=== {d} ===")
        print(json.dumps(s, ensure_ascii=False, indent=2))
        overall = overall and s.get("passed", False)
    return 0 if overall else 1


if __name__ == "__main__":
    sys.exit(main())
