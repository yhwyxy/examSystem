"""Runtime 主循环: poll 队列 -> claim -> load snapshot -> grade -> complete.

单线程 worker 接收一条 job, 在单 conn 内做 fenced transaction: verify lease
-> complete_job (subjective 写回 / job done). heartbeat 在另一线程每 heartbeat
seconds 调 renew_lease.
"""
from __future__ import annotations

import json
import logging
from . import repository as repo

logger = logging.getLogger(__name__)


def run_one_job(conn, worker_id: str, lease_seconds: int,
                snapshot_cache, ssvc: object) -> bool:
    """抢一条 job, 算分完成. 返回 True (处理了一条) / False (空队列 - idle sleep).

    全过程在一个 conn 内; fenced complete (lease verify), 任一异常 traceback
    -> fail_job + conn.rollback 后续再退. 返回 idle (False) / processed (True).
    """
    job = repo.claim_job(conn, worker_id, lease_seconds)
    if job is None:
        return False
    payload = repo.load_job_payload(conn, job.id)
    doc = snapshot_cache.get(payload["snapshot_path"], payload.get("snapshot_hash"))
    answers = payload.get("answers_json") or "{}"
    if isinstance(answers, str):
        answers = json.loads(answers)
    try:
        from .grading import grade_submission
        preserve = _load_preserve_index(conn, job.submission_id)
        result = grade_submission(doc, answers, ssvc, preserve=preserve)
    except Exception as e:
        logger.warning("job=%s grading error: %s", job.id, e)
        out = repo.fail_job(conn, job, error_msg=str(e))
        conn.commit()
        return True
    detail_json = json.dumps(result["detail"]).encode("utf-8")
    out = repo.complete_job(
        conn, job,
        objective_score=int(result["objective_score"]),
        subjective_score_machine=float(result["subjective_score_machine"]),
        subjective_score_final=float(result["subjective_score_final"]),
        grading_detail_json=detail_json,
        review_status=result.get("overall_review_status", "graded"),
    )
    conn.commit()
    logger.info("job=%s complete=%s obj=%s subj_%s",
                job.id, out, result["objective_score"], result["subjective_score_final"])
    return True


_LOAD_PRESERVE_SQL = """
SELECT grading_detail_json FROM submissions
WHERE id = %(submission_id)s
LIMIT 1;
"""

def _load_preserve_index(conn, submission_id: int) -> dict | None:
    """读旧 grading_detail_json -> {qid -> entry} 仅含 manually_reviewed=True 条目 (preserve manual)."""
    from psycopg.rows import dict_row
    cur = conn.cursor(row_factory=dict_row)
    cur.execute(_LOAD_PRESERVE_SQL, {"submission_id": submission_id})
    row = cur.fetchone()
    if not row or not row.get("grading_detail_json"):
        return None
    try:
        details = json.loads(row["grading_detail_json"]) if isinstance(row["grading_detail_json"], str) else row["grading_detail_json"]
    except json.JSONDecodeError:
        return None
    if not isinstance(details, list):
        return None
    out = {}
    for e in details:
        if not isinstance(e, dict):
            continue
        if e.get("manually_reviewed") is not True:
            # keep only manual preserved entries
            if e.get("review_status") in {"approved_manual", "rejected_manual"}:
                out[str(e.get("question_id"))] = e
            continue
        out[str(e.get("question_id"))] = e
    return out or None
