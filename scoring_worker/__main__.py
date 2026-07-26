"""Worker 主入口: poll loop + heartbeat 续租线程 + graceful shutdown.

启动: python -m scoring_worker  (依赖已 pip-installed)
"""
from __future__ import annotations

import logging
import signal
import sys
import threading
import time

from .runtime import run_one_job
from .snapshot import SnapshotCache
from .grader_bridge import build_scoring_request
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


# 固定无敏感 fixture: 仅用于 preflight 健康评分, 不含真实答案/学生数据.
_PREFLIGHT_FIXTURE = {
    "id": "preflight-fixture-0001",
    "type": "short_answer",
    "question": "用一句话定义 HTTP 协议",
    "answer": "HTTP 是一种超文本传输协议",
    "score": 10,
    "scoring_mode": "text",
    "scoring_rubric": "提到超文本:5\n提到传输协议:5",
}
_PREFLIGHT_STUDENT_ANSWER = "HTTP 是用于传输超文本的协议"


def _preflight() -> int:
    """--preflight: --check 基础上用固定 fixture 跑一次真实评分."""
    cfg = Config.from_env()
    _setup_logging(cfg.log_level)
    ssvc = _build_score_service()
    try:
        request = build_scoring_request(_PREFLIGHT_FIXTURE,
                                        _PREFLIGHT_STUDENT_ANSWER)
        result = ssvc.score(request)
        track = str(getattr(result, "track", "judge"))
        score = float(getattr(result, "score", 0.0))
        warnings = list(getattr(result, "warnings", []) or [])
        logger.info("--preflight: fixture scored track=%s score=%.2f "
                    "warnings=%d", track, score, len(warnings))
        if track.lower() == "lexical":
            logger.error("--preflight FAIL: lexical fallback detected "
                         "(reranker/judge unavailable), 不应通过 preflight")
            return 2
        fallback_markers = ("回退", "不可用", "相似度", "fallback", "unavailable")
        fb_warns = [w for w in warnings
                    if any(m in str(w).lower() for m in fallback_markers)]
        if fb_warns:
            logger.error("--preflight FAIL: 检测到 reranker/语义降级 warnings=%s",
                         fb_warns)
            return 2
        if score < 0 or score > float(_PREFLIGHT_FIXTURE["score"]) + 1e-6:
            logger.error("--preflight FAIL: score %.2f 越界 (max=%.2f)",
                         score, float(_PREFLIGHT_FIXTURE["score"]))
            return 2
        if score == 0.0:
            logger.error("--preflight FAIL: score=0 fixture 有合理答案不应 0 分, "
                         "疑似 reranker 失败但 warnings 未显形")
            return 2
        logger.info("--preflight OK: 非 lexical, 评分正常 (track=%s)", track)
        return 0
    except Exception:
        logger.exception("--preflight FAIL: 评分执行异常")
        return 2
    finally:
        from .grader_bridge import close_service
        close_service()


def main() -> int:
    import argparse
    parser = argparse.ArgumentParser(prog="scoring_worker", description="主观题评分 worker")
    parser.add_argument("--check", action="store_true",
                        help="仅校验配置 + 服务初始化, 不进入轮询")
    parser.add_argument("--preflight", action="store_true",
                        help="check + 用固定无敏感 fixture 跑一次真实评分; "
                             "任一模式断言非 lexical fallback; 不 claim/不写 DB")
    args = parser.parse_args()
    if args.preflight:
        return _preflight()
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
                import psycopg
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
