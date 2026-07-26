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
        result = grade_submission(doc, answers, ssvc)
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
        review_status="graded",
    )
    conn.commit()
    logger.info("job=%s complete=%s obj=%s subj_%s",
                job.id, out, result["objective_score"], result["subjective_score_final"])
    return True
