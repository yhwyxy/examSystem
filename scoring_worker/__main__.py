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
    """lazy 加载 subjective_scoring 服务 (Task 7 完整版).

    grader_bridge.get_subjective_service 负责 RERANK_USE_REMOTE 模式切换 +
    local model 加载. 失败 -> 退出 worker (避免静默乱评分).
    Worker 退出时调 close_service 释放 remote reranker (参看 finally 关闭).
    """
    try:
        from .grader_bridge import get_subjective_service
        return get_subjective_service()
    except ImportError as e:
        logger.error("subjective_scoring import失败: %s. 是否 uv lock --project scoring_worker?", e)
        raise


def _check_only() -> int:
    """--check 模式: 仅校验配置 + 服务初始化 + 退出, 不进入轮询 (plan Step 8)."""
    cfg = Config.from_env()
    _setup_logging(cfg.log_level)
    ssvc = _build_score_service()
    logger.info("--check: config OK, score service initialized: %r", ssvc)
    from .grader_bridge import close_service
    close_service()
    return 0


def main() -> int:
    import argparse
    parser = argparse.ArgumentParser(prog="scoring_worker", description="主观题评分 worker")
    parser.add_argument("--check", action="store_true",
                        help="仅校验配置 + 服务初始化, 不进入轮询")
    args = parser.parse_args()
    if args.check:
        return _check_only()

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
    try:
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
    finally:
        try:
            from .grader_bridge import close_service
            close_service()
        except Exception:
            logger.exception("grader_bridge.close_service")
        logger.info("worker [%s] exiting", cfg.worker_id)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
