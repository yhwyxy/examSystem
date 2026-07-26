"""Worker 主入口: poll loop + heartbeat 续租线程 + graceful shutdown.

启动: python -m scoring_worker  (依赖已 pip-installed)
"""
from __future__ import annotations

import logging
import psycopg
import signal
import sys
import threading
import time

from .runtime import run_one_job
from .snapshot import SnapshotCache
from .config import Config

logger = logging.getLogger("scoring_worker")


def _setup_logging(level: str) -> None:
    logging.basicConfig(
        level=getattr(logging, level.upper(), logging.INFO),
        format="%(asctime)s [%(levelname)s] %(name)s %(message)s",
    )


def _build_score_service():
    """lazy 加载 subjective_scoring 包. 失败时降级到 stub (Task 7 partial).

    TODO (Task 7 续): 完整 RERANK_USE_REMOTE / CohereReranker / 本地模型注入.
    """
    try:
        from .grader_bridge import StubSubjectiveScorer  # Task 7 stub bridge
        return StubSubjectiveScorer()
    except ImportError as e:
        logger.error("subjective_scoring import失败: %s. stub fallback", e)
        raise


def main() -> int:
    cfg = Config.from_env()
    _setup_logging(cfg.log_level)
    ssvc = _build_score_service()
    cache = SnapshotCache()
    stop = threading.Event()

    def _handle(signum, frame):
        logger.warning("received signal %d, graceful shutdown...", signum)
        stop.set()
    for s in (signal.SIGINT, signal.SIGTERM):
        signal.signal(s, _handle)

    logger.info("worker [%s] starting (poll=%.1fs lease=%ds heartbeat=%ds)",
                cfg.worker_id, cfg.poll_interval_seconds, cfg.lease_seconds,
                cfg.heartbeat_seconds)
    while not stop.is_set():
        try:
            with psycopg.connect(cfg.database_url) as conn:
                conn.autocommit = False
                processed = run_one_job(conn, cfg.worker_id,
                                        cfg.lease_seconds, cache, ssvc)
                if not processed:
                    time.sleep(cfg.poll_interval_seconds)
        except Exception:  # 重生; 不允许 worker 死
            logger.exception("worker iteration failed; backoff 1s")
            time.sleep(1.0)
    logger.info("worker [%s] exiting", cfg.worker_id)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
