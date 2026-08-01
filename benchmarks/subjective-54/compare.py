#!/usr/bin/env python3
"""54 份基准提交回归对比：当前 DB 评分 vs 盲评估计 vs v0.1.7 基线。

用法（在 examSystem 根目录）:
    python3 benchmarks/subjective-54/compare.py            # 全量指标 + 退化/改善清单
    python3 benchmarks/subjective-54/compare.py --paper software-development  # 只看某卷

数据:
    est_perq.json / est_totals.json      按参考答案的逐题/整卷盲评估计（真值近似）
    baseline_v017_perq.jsonl / *_totals  v0.1.7 首轮系统分（固定基线, 勿更新）
    当前分数                              实时从 dev DB submissions 表读取 (id 3-56)
判定:
    - 核心指标: 逐题 MAE / 偏差 / 误差<=3 题数
    - degraded  = 基线误差<=3 且当前>3 —— 每一例都必须逐个归因
    - improved  = 基线误差>3 且当前<=3
"""
import argparse
import json
import os
import statistics as st
import subprocess

DSN = os.environ.get(
    "DATABASE_URL",
    "postgres://exam_app:exam_app_dev@127.0.0.1:5432/exam_system?sslmode=disable",
)
HERE = os.path.dirname(os.path.abspath(__file__))


def load_baseline():
    perq = {}
    with open(os.path.join(HERE, "baseline_v017_perq.jsonl")) as f:
        for line in f:
            r = json.loads(line)
            perq[(r["slug"], r["emp"], r["qid"])] = (r["machine"], r["max"])
    totals = {}
    with open(os.path.join(HERE, "baseline_v017_totals.txt")) as f:
        for line in f:
            p = line.strip().split("|")
            totals[(p[0], p[1])] = float(p[4])
    est = json.load(open(os.path.join(HERE, "est_perq.json")))
    est_tot = json.load(open(os.path.join(HERE, "est_totals.json")))
    return perq, totals, est, est_tot


def load_current():
    sql = (
        "SELECT paper_id, employee_id, total_score, grading_detail_json::text "
        "FROM submissions WHERE id BETWEEN 3 AND 56 ORDER BY paper_id, employee_id;"
    )
    out = subprocess.run(
        ["psql", DSN, "-t", "-A", "-F", "\t", "-c", sql],
        capture_output=True, text=True, check=True,
    ).stdout
    perq, totals = {}, {}
    for line in out.splitlines():
        if not line.strip():
            continue
        slug, emp, total, detail = line.split("\t", 3)
        totals[(slug, emp)] = float(total)
        for e in json.loads(detail):
            qid = str(e.get("question_id") or e.get("id"))
            ms = e.get("machine_score")
            if ms is not None:
                perq[(slug, emp, qid)] = float(ms)
    return perq, totals


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--paper", help="只对比指定试卷 slug")
    args = ap.parse_args()

    base_perq, base_tot, est, est_tot = load_baseline()
    cur_perq, cur_tot = load_current()

    rows = []
    for (slug, emp, qid), (old_m, mx) in base_perq.items():
        if args.paper and slug != args.paper:
            continue
        e = est.get(slug, {}).get(emp, {}).get(qid)
        n = cur_perq.get((slug, emp, qid))
        if e is None or n is None:
            continue
        rows.append((slug, emp, qid, mx, e, old_m, n, old_m - e, n - e))

    n = len(rows)
    print(f"逐题对比 {n} 行")
    print(f"误差<=3: 基线 {sum(abs(r[7]) <= 3 for r in rows)} -> 当前 {sum(abs(r[8]) <= 3 for r in rows)}")
    print(f"MAE:     基线 {st.mean(abs(r[7]) for r in rows):.2f} -> 当前 {st.mean(abs(r[8]) for r in rows):.2f}")
    print(f"偏差:    基线 {st.mean(r[7] for r in rows):+.2f} -> 当前 {st.mean(r[8] for r in rows):+.2f}")

    deg = [r for r in rows if abs(r[7]) <= 3 and abs(r[8]) > 3]
    imp = [r for r in rows if abs(r[7]) > 3 and abs(r[8]) <= 3]
    print(f"\n退化 {len(deg)} 例（每例必须归因——看 grading_detail_json 的 reason 字段）:")
    for r in deg:
        print(f"  {r[0]} {r[1]} {r[2]}: max={r[3]} 估={r[4]} 基线={r[5]} 当前={r[6]}")
    print(f"改善 {len(imp)} 例:")
    for r in imp:
        print(f"  {r[0]} {r[1]} {r[2]}: max={r[3]} 估={r[4]} 基线={r[5]} 当前={r[6]}")

    bad = sorted((r for r in rows if abs(r[8]) > 3), key=lambda r: -abs(r[8]))
    print(f"\n当前仍 >3 的 {len(bad)} 例（优化候选, 按误差降序）:")
    for r in bad[:15]:
        print(f"  {r[0]} {r[1]} {r[2]}: max={r[3]} 估={r[4]} 基线={r[5]} 当前={r[6]}")

    if not args.paper:
        errs_old = [abs(base_tot[k] - est_tot[k[0]][k[1]]["total"]) for k in base_tot]
        errs_new = [abs(cur_tot[k] - est_tot[k[0]][k[1]]["total"]) for k in cur_tot if k in base_tot]
        print(f"\n整卷总分 MAE: 基线 {st.mean(errs_old):.2f} -> 当前 {st.mean(errs_new):.2f}; "
              f"<=3分: {sum(e <= 3 for e in errs_old)}/54 -> {sum(e <= 3 for e in errs_new)}/54")


if __name__ == "__main__":
    main()
