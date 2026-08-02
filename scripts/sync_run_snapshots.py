#!/usr/bin/env python3
"""运行快照同步——用最新试卷 data 覆盖 exam_runs 快照并更新 snapshot_hash。"""
from __future__ import annotations

import hashlib
import json
import os
import subprocess
import sys

DSN = os.environ.get(
    "DATABASE_URL",
    "postgres://exam_app:exam_app_dev@127.0.0.1:5432/exam_system?sslmode=disable",
)
ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


def canonical(doc) -> bytes:
    return json.dumps(doc, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode("utf-8")


def main() -> int:
    out = subprocess.run(
        ["psql", DSN, "-t", "-A", "-F", "|", "-c",
         "SELECT id, paper_id, snapshot_path FROM exam_runs WHERE snapshot_path IS NOT NULL;"],
        capture_output=True, text=True, check=True).stdout
    papers: dict[str, dict] = {}
    failed = 0
    for line in out.splitlines():
        if not line.strip():
            continue
        run_id, paper_id, snap_path = line.split("|", 2)
        if paper_id not in papers:
            path = os.path.join(ROOT, "data", "papers", f"{paper_id}.json")
            if not os.path.exists(path):
                print(f"SKIP run {run_id}: paper file not found at {path}")
                continue
            papers[paper_id] = json.load(open(path))
        snap_file = snap_path if os.path.isabs(snap_path) else os.path.join(ROOT, snap_path)
        snap = json.load(open(snap_file))
        snap["questions"] = papers[paper_id]["questions"]
        with open(snap_file, "w", encoding="utf-8") as f:
            json.dump(snap, f, ensure_ascii=False, indent=2)
        digest = hashlib.sha256(canonical(snap)).hexdigest()
        try:
            subprocess.run(["psql", DSN, "-q", "-c",
                           f"UPDATE exam_runs SET snapshot_hash='{digest}' WHERE id='{run_id}';"],
                          check=True, capture_output=True)
        except subprocess.CalledProcessError as e:
            print(f"FAIL run {run_id} ({paper_id}): {e.stderr.decode()[:100]}")
            failed += 1
            continue
        print(f"{run_id:44}  {paper_id:24}  {digest[:12]}")
    print(f"\nDone. {failed} failures." if not failed else f"\nDone. {failed} failures!")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())