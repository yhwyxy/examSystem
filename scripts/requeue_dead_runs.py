#!/usr/bin/env python3
"""选择性重放 dead 评分任务——只重排队快照已存在且 hash 与 DB 一致的 run 的 dead jobs.

背景 (2026-08-02): 一批 108 条 job 因「快照尚未同步就开评」被判
snapshot hash mismatch 而 dead. 现在 data/exam_runs 快照已同步且与
exam_runs.snapshot_hash 一致, 可安全重放; 快照缺失或 hash 不匹配的 run 一律跳过.

用法:
  # dry-run (默认, 只打印将重放的 run)
  DATABASE_URL='postgres://exam_app:exam_app_dev@127.0.0.1:5432/exam_system?sslmode=disable' \
    python3 scripts/requeue_dead_runs.py

  # 实际重排队 (仅把 verified run 的 dead job 回 queued)
  DATABASE_URL='...' python3 scripts/requeue_dead_runs.py --apply
"""
from __future__ import annotations

import argparse
import hashlib
import os
import subprocess
import sys

DSN = os.environ.get(
    "DATABASE_URL",
    "postgres://exam_app:exam_app_dev@127.0.0.1:5432/exam_system?sslmode=disable",
)
ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


def _psql(sql: str) -> str:
    out = subprocess.run(
        ["psql", DSN, "-t", "-A", "-F", "|", "-c", sql],
        capture_output=True, text=True, check=True,
    )
    return out.stdout


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--apply", action="store_true",
                        help="实际重排队 (默认仅 dry-run)")
    args = parser.parse_args()

    rows = [ln for ln in _psql(
        "SELECT r.id, r.paper_id, r.snapshot_path, r.snapshot_hash, count(j.id) "
        "FROM exam_runs r JOIN grading_jobs j ON j.run_id = r.id "
        "WHERE j.status = 'dead' "
        "GROUP BY r.id ORDER BY r.id"
    ).splitlines() if ln.strip()]

    eligible: list[tuple[str, str, int]] = []
    skipped = 0
    for line in rows:
        run_id, paper_id, snap_path, snap_hash, n = line.split("|", 4)
        n = int(n)
        if not snap_path or not snap_hash:
            print(f"SKIP run {run_id} ({paper_id}): 无 snapshot_path/hash, {n} 条 dead 不重放")
            skipped += n
            continue
        f = snap_path if os.path.isabs(snap_path) else os.path.join(ROOT, snap_path)
        if not os.path.exists(f):
            print(f"SKIP run {run_id} ({paper_id}): 快照缺失 {f}, {n} 条 dead 不重放")
            skipped += n
            continue
        actual = hashlib.sha256(open(f, "rb").read()).hexdigest()
        if actual != snap_hash:
            print(f"SKIP run {run_id} ({paper_id}): hash 不匹配 (预期 {snap_hash[:12]} 实得 {actual[:12]}), "
                  f"{n} 条 dead 不重放 (先跑 scripts/sync_run_snapshots.py)")
            skipped += n
            continue
        eligible.append((run_id, paper_id, n))

    total = sum(n for _, _, n in eligible)
    print(f"\n==> dead run 共 {len(rows)} 个; 快照校验通过可重放 {len(eligible)} 个 run / {total} 条 job; "
          f"跳过 {skipped} 条")
    for run_id, paper_id, n in eligible:
        print(f"  run {run_id} ({paper_id}): {n} 条")

    if not args.apply:
        print("\n(dry-run) 未做任何修改; 确认后加 --apply 执行重放")
        return 0
    if not eligible:
        print("没有可重放的 run, 退出")
        return 0

    ids = ",".join(f"'{r}'" for r, _, _ in eligible)
    print(f"\n==> 重排队 {total} 条 job (status->queued, attempts 清零, 清租约)")
    _psql(
        "UPDATE grading_jobs SET status = 'queued', attempts = 0, last_error = NULL, "
        "lease_owner = NULL, lease_token = NULL, lease_until = NULL, "
        "available_at = now(), updated_at = now() "
        f"WHERE status = 'dead' AND run_id IN ({ids})"
    )
    print("done")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
