"""Worker 配置: 从环境变量读取 (12-Factor). 单调 dataclass."""
from __future__ import annotations

import os
from dataclasses import dataclass, field


@dataclass
class Config:
    database_url: str
    worker_id: str
    poll_interval_seconds: float
    lease_seconds: int
    heartbeat_seconds: int
    max_score: float
    concurrency: int
    log_level: str
    # 多选部分给分开关: 必须与 Go 服务 config scoring.multiple_choice_partial 一致,
    # 否则 regrade 重写的 grading_detail 单题分与提交时的 objective_score 列不一致.
    multiple_choice_partial: bool

    @classmethod
    def from_env(cls) -> "Config":
        wid = os.getenv("WORKER_ID") or "worker-default"
        return cls(
            database_url=os.getenv("DATABASE_URL", "postgresql:///exam"),
            worker_id=wid,
            poll_interval_seconds=float(os.getenv("POLL_INTERVAL_SECONDS", "1.0")),
            lease_seconds=int(os.getenv("LEASE_SECONDS", "60")),
            heartbeat_seconds=int(os.getenv("HEARTBEAT_SECONDS", "15")),
            max_score=float(os.getenv("MAX_SCORE", "100.0")),
            concurrency=int(os.getenv("WORKER_CONCURRENCY", "1")),
            log_level=os.getenv("LOG_LEVEL", "INFO"),
            multiple_choice_partial=(
                os.getenv("MULTIPLE_CHOICE_PARTIAL", "true").strip().lower()
                != "false"),
        )
