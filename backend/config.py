"""配置加载与校验。

读取项目根目录的 config.yaml，并用 pydantic 模型校验。
"""
from __future__ import annotations

from functools import lru_cache
from pathlib import Path
from datetime import datetime
from typing import Any

import yaml
from pydantic import BaseModel, Field, field_validator

PROJECT_ROOT = Path(__file__).resolve().parent.parent
CONFIG_PATH = PROJECT_ROOT / "config.yaml"


class ServerConfig(BaseModel):
    host: str = "0.0.0.0"
    port: int = 8000
    allow_origins: list[str] = Field(default_factory=lambda: ["http://127.0.0.1:8000", "http://localhost:8000"])


class ExamConfig(BaseModel):
    title: str = "企业内部考试"
    duration_minutes: int = 60
    auto_submit: bool = True
    allow_duplicate_submit: bool = False
    duplicate_key: str = "employee_id"
    enable_global_time_window: bool = False
    start_time: datetime | None = None
    end_time: datetime | None = None
    grace_period_seconds: int = 30

    @field_validator("start_time", "end_time")
    @classmethod
    def require_timezone(cls, value: datetime | None) -> datetime | None:
        if value is not None and value.tzinfo is None:
            raise ValueError("start_time/end_time 必须包含时区")
        return value


class ScoringConfig(BaseModel):
    multiple_choice_partial: bool = True
    wrong_choice_penalty: bool = False
    score_precision: int = 1


class ReviewConfig(BaseModel):
    high_confidence_threshold: float = 0.75
    need_review_threshold: float = 0.5
    low_confidence_threshold: float = 0.35


class LLMConfig(BaseModel):
    provider: str = "ollama"
    endpoint: str = "http://localhost:11434"
    model: str = "qwen2.5:7b"
    timeout_seconds: int = 8
    retry_times: int = 1


class EmbeddingConfig(BaseModel):
    model: str = "BAAI/bge-m3"
    device: str = "cpu"


class GradingConfig(BaseModel):
    strategy: str = "llm_first"
    use_llm: bool = True
    use_embedding_fallback: bool = True
    sync_grading: bool = True
    llm: LLMConfig = Field(default_factory=LLMConfig)
    embedding: EmbeddingConfig = Field(default_factory=EmbeddingConfig)


class AdminConfig(BaseModel):
    enable_auth: bool = False
    password: str | None = None


class ExportConfig(BaseModel):
    format: str = "xlsx"


class AppConfig(BaseModel):
    server: ServerConfig = Field(default_factory=ServerConfig)
    exam: ExamConfig = Field(default_factory=ExamConfig)
    scoring: ScoringConfig = Field(default_factory=ScoringConfig)
    review: ReviewConfig = Field(default_factory=ReviewConfig)
    grading: GradingConfig = Field(default_factory=GradingConfig)
    admin: AdminConfig = Field(default_factory=AdminConfig)
    export: ExportConfig = Field(default_factory=ExportConfig)


def _load_raw() -> dict[str, Any]:
    if not CONFIG_PATH.exists():
        return {}
    with CONFIG_PATH.open("r", encoding="utf-8") as f:
        return yaml.safe_load(f) or {}


@lru_cache(maxsize=1)
def get_config() -> AppConfig:
    """加载并缓存配置。配置错误会在启动期暴露，便于排错。"""
    try:
        return AppConfig.model_validate(_load_raw())
    except Exception as e:
        raise RuntimeError(f"config.yaml 配置校验失败: {e}") from e


def reload_config() -> AppConfig:
    """清除缓存并重新加载配置。"""
    get_config.cache_clear()
    return get_config()
