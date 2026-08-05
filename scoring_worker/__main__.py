"""Worker 主入口: poll loop + heartbeat 续租线程 + graceful shutdown.

启动: python -m scoring_worker  (依赖已 pip-installed)
"""
from __future__ import annotations

import json
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

# 评分方式设置: 从 DB app_settings['scoring'] 读取 (Go admin 设置栏写入).
# 表/行不存在 -> None (走环境变量默认), 与旧部署兼容.
_SCORING_SETTING_SQL = """
SELECT value FROM app_settings WHERE key = 'scoring' LIMIT 1
"""


def _load_scoring_setting(conn) -> dict | None:
    """读 DB 评分设置 (app_settings['scoring']). 表不存在 / 无行 / 解析失败 -> None."""
    try:
        with conn.cursor() as cur:
            cur.execute(_SCORING_SETTING_SQL)
            row = cur.fetchone()
    except Exception:
        return None
    if not row:
        return None
    try:
        val = row[0]
        if isinstance(val, str):
            val = json.loads(val)
        return val if isinstance(val, dict) else None
    except Exception:
        return None


def _scoring_fingerprint(cfg: dict | None) -> str | None:
    """评分设置的规范化指纹; None (env 默认) 或 dict 序列化."""
    if not cfg:
        return None
    try:
        return json.dumps(cfg, sort_keys=True)
    except Exception:
        return None


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
    # 评分服务随 DB app_settings['scoring'] 变化热切换 (admin 设置栏保存后对
    # 后续评分生效, 无需重启 worker). 首轮从环境变量构造 (与旧部署行为一致).
    svc = _build_score_service()
    svc_fp: str | None = None
    try:
        while not stop.is_set():
            try:
                import psycopg
                with psycopg.connect(cfg.database_url) as conn:
                    conn.autocommit = False
                    db_cfg = _load_scoring_setting(conn)
                    conn.commit()  # 结束只读事务; run_one_job 自行开新事务
                    fp = _scoring_fingerprint(db_cfg)
                    if fp != svc_fp:
                        try:
                            from .grader_bridge import (
                                get_subjective_service_for_config, reset_service,
                            )
                            reset_service()
                            svc = (get_subjective_service_for_config(db_cfg)
                                   if db_cfg else _build_score_service())
                            svc_fp = fp
                            logger.info("scoring 服务已按设置切换: %s",
                                        (db_cfg or {}).get("method", "env 默认"))
                        except Exception:
                            # 设置不完整/不可用: 沿用旧服务, 记一次错 (避免每
                            # 1s 刷屏; 重新保存合法设置会再次触发切换).
                            logger.exception("评分设置切换失败, 沿用旧服务; "
                                             "请检查 app_settings['scoring']")
                            svc_fp = fp
                    processed = run_one_job(conn, cfg, cache, svc)
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
