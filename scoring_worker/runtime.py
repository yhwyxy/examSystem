"""Runtime 主循环: poll 队列 -> claim -> load snapshot -> grade -> complete.

单线程 worker 接收一条 job; claim / payload 读取后立即结束事务, 评分期间不持有
未提交事务 (否则 complete 的 fenced verify 用的 now() 冻结在事务开始时刻,
租约校验形同虚设). 评分期间由独立心跳线程 (独立连接, autocommit) 每
heartbeat_seconds 调 renew_lease 续租, 支撑超过 lease_seconds 的长评分.
"""
from __future__ import annotations

import json
import logging
import threading
from . import repository as repo

logger = logging.getLogger(__name__)


def _heartbeat_loop(cfg, job, stop: threading.Event) -> None:
    """独立连接续租; 续租失败 (租约被回收/被抢) 即退出, 主线程 complete 时
    会被 fenced verify 拦下拿到 'lost'."""
    import psycopg
    try:
        with psycopg.connect(cfg.database_url, autocommit=True) as conn:
            while not stop.wait(max(1, cfg.heartbeat_seconds)):
                if not repo.renew_lease(conn, job.id, cfg.worker_id,
                                        job.lease_token, cfg.lease_seconds):
                    logger.warning("job=%s lease renew rejected (lost/superseded)",
                                   job.id)
                    return
    except Exception:
        logger.exception("job=%s heartbeat thread error", job.id)


def run_one_job(conn, cfg, snapshot_cache, ssvc: object) -> bool:
    """抢一条 job, 算分完成. 返回 True (处理了一条) / False (空队列 - idle sleep).

    fenced complete (lease verify) 在评分后的新事务里执行; 任一异常 traceback
    -> fail_job + conn.rollback 后续再退. 返回 idle (False) / processed (True).
    """
    job = repo.claim_job(conn, cfg.worker_id, cfg.lease_seconds)
    if job is None:
        return False
    conn.commit()

    try:
        payload = repo.load_job_payload(conn, job.id)
        preserve = _load_preserve_index(conn, job.submission_id)
        conn.commit()  # 结束读事务: 评分期间不持有事务
        doc = snapshot_cache.get(payload["snapshot_path"], payload.get("snapshot_hash"))
        answers = payload.get("answers_json") or "{}"
        if isinstance(answers, str):
            answers = json.loads(answers)
        from .grading import grade_submission
        stop_hb = threading.Event()
        hb = threading.Thread(target=_heartbeat_loop, args=(cfg, job, stop_hb),
                              name=f"hb-job-{job.id}", daemon=True)
        hb.start()
        try:
            result = grade_submission(
                doc, answers, ssvc, preserve=preserve,
                multiple_choice_partial=cfg.multiple_choice_partial)
        finally:
            stop_hb.set()
            hb.join(timeout=5)
        detail_json = json.dumps(result["detail"]).encode("utf-8")
        out = repo.complete_job(
            conn, job,
            subjective_score_machine=float(result["subjective_score_machine"]),
            subjective_score_final=float(result["subjective_score_final"]),
            grading_detail_json=detail_json,
            review_status=result.get("overall_review_status", "graded"),
        )
        conn.commit()
    except Exception as e:
        conn.rollback()
        logger.warning("job=%s grading error: %s", job.id, e)
        repo.fail_job(conn, job, error_msg=str(e))
        conn.commit()
        return True

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
        # 条目键兼容: 规范格式 question_id, worker 旧格式仅 id.
        qid = str(e.get("question_id") or e.get("id"))
        if e.get("manually_reviewed") is not True:
            # keep only manual preserved entries
            if e.get("review_status") in {"approved_manual", "rejected_manual"}:
                out[qid] = e
            continue
        out[qid] = e
    return out or None
