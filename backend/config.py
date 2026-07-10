"""配置加载与校验。

支持启动后热重载（调用 reload_config()）。
启动时做严格的 schema 校验，尽早暴露配置错误。
"""
from __future__ import annotations

import os
import sys
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import yaml

_CONFIG_PATH = Path(__file__).resolve().parent.parent / "config.yaml"


# ---------------------------------------------------------------------------
# 配置数据类
# ---------------------------------------------------------------------------

@dataclass(frozen=True)
class ServerConfig:
    host: str = "0.0.0.0"
    port: int = 8000
    allow_origins: list[str] = field(default_factory=lambda: ["*"])


@dataclass(frozen=True)
class ExamConfig:
    title: str = "企业内部考试"
    duration_minutes: int = 60
    auto_submit: bool = True
    grace_period_seconds: int = 30
    allow_duplicate_submit: bool = False
    duplicate_key: str = "employee_id"
    # 全局考试时间窗口（可选，仅依赖服务器时间）
    enable_global_time_window: bool = False
    start_time: Any = None
    end_time: Any = None


@dataclass(frozen=True)
class ScoringConfig:
    multiple_choice_partial: bool = True
    wrong_choice_penalty: bool = False
    score_precision: int = 1


@dataclass(frozen=True)
class ReviewConfig:
    high_confidence_threshold: float = 0.75
    need_review_threshold: float = 0.5
    low_confidence_threshold: float = 0.35


@dataclass(frozen=True)
class EmbeddingConfig:
    """Ollama Embedding 配置。"""
    model: str = "bge-m3"
    device: str = "cpu"
    endpoint: str = "http://localhost:11434"
    timeout_seconds: int = 10


@dataclass(frozen=True)
class GradingConfig:
    """判分配置（仅使用 Embedding 模型）。"""
    sync_grading: bool = True
    embedding: EmbeddingConfig = field(default_factory=EmbeddingConfig)


@dataclass(frozen=True)
class AdminConfig:
    enable_auth: bool = False
    password: str | None = None


@dataclass(frozen=True)
class ExportConfig:
    format: str = "xlsx"


@dataclass(frozen=True)
class AppConfig:
    server: ServerConfig = field(default_factory=ServerConfig)
    exam: ExamConfig = field(default_factory=ExamConfig)
    scoring: ScoringConfig = field(default_factory=ScoringConfig)
    review: ReviewConfig = field(default_factory=ReviewConfig)
    grading: GradingConfig = field(default_factory=GradingConfig)
    admin: AdminConfig = field(default_factory=AdminConfig)
    export: ExportConfig = field(default_factory=ExportConfig)


# ---------------------------------------------------------------------------
# 全局单例
# ---------------------------------------------------------------------------

_config: AppConfig | None = None


def _deep_merge(base: dict, override: dict) -> dict:
    """递归合并：override 中的键覆盖 base 中的同名键，嵌套 dict 递归合并。"""
    merged = dict(base)
    for k, v in override.items():
        if k in merged and isinstance(merged[k], dict) and isinstance(v, dict):
            merged[k] = _deep_merge(merged[k], v)
        else:
            merged[k] = v
    return merged


def _resolve_env(raw: dict) -> dict:
    """将顶层 value 为 '${ENV_VAR}' 的字符串替换为环境变量，未设置保留原值。"""
    for k, v in raw.items():
        if isinstance(v, str) and v.startswith("${") and v.endswith("}"):
            env_key = v[2:-1]
            raw[k] = os.environ.get(env_key, v)
    return raw


def _build_config(raw: dict) -> AppConfig:
    """从原始 dict 构建 AppConfig，只提取已知字段，忽略未知字段。"""
    def _section(cls, key, defaults):
        data = _deep_merge(defaults, raw.get(key, {}))
        known = {f.name for f in cls.__dataclass_fields__.values()}
        return cls(**{k: v for k, v in data.items() if k in known})

    return AppConfig(
        server=_section(ServerConfig, "server", {
            "host": "0.0.0.0", "port": 8000, "allow_origins": ["*"],
        }),
        exam=_section(ExamConfig, "exam", {
            "title": "企业内部考试",
            "duration_minutes": 60,
            "auto_submit": True,
            "grace_period_seconds": 30,
            "allow_duplicate_submit": False,
            "duplicate_key": "employee_id",
            "enable_global_time_window": False,
            "start_time": None,
            "end_time": None,
        }),
        scoring=_section(ScoringConfig, "scoring", {
            "multiple_choice_partial": True, "wrong_choice_penalty": False, "score_precision": 1,
        }),
        review=_section(ReviewConfig, "review", {
            "high_confidence_threshold": 0.75, "need_review_threshold": 0.5, "low_confidence_threshold": 0.35,
        }),
        grading=_build_grading_config(raw.get("grading", {})),
        admin=_section(AdminConfig, "admin", {"enable_auth": False, "password": None}),
        export=_section(ExportConfig, "export", {"format": "xlsx"}),
    )


def _build_grading_config(grading: dict) -> GradingConfig:
    """从 grading 原始 dict 构建 GradingConfig，忽略未知字段（向前兼容）。"""
    defaults_grading = {"sync_grading": True}
    merged_grading = _deep_merge(defaults_grading, grading)
    emb_raw = merged_grading.get("embedding", {})
    defaults_emb = {
        "model": "bge-m3",
        "device": "cpu",
        "endpoint": "http://localhost:11434",
        "timeout_seconds": 10,
    }
    merged_emb = _deep_merge(defaults_emb, emb_raw)
    emb_known = {f.name for f in EmbeddingConfig.__dataclass_fields__.values()}
    emb = EmbeddingConfig(**{k: v for k, v in merged_emb.items() if k in emb_known})
    known = {f.name for f in GradingConfig.__dataclass_fields__.values()}
    return GradingConfig(**{k: v for k, v in merged_grading.items() if k in known and k != "embedding"}, embedding=emb)


def _validate_config(raw: dict) -> None:
    """启动时做严格的 schema 校验，尽早暴露配置错误。"""
    _allowed = {
        "server": {"host", "port", "allow_origins"},
        "exam": {
            "title", "duration_minutes", "auto_submit", "grace_period_seconds",
            "allow_duplicate_submit", "duplicate_key",
            "enable_global_time_window", "start_time", "end_time",
        },
        "scoring": {"multiple_choice_partial", "wrong_choice_penalty", "score_precision"},
        "review": {"high_confidence_threshold", "need_review_threshold", "low_confidence_threshold"},
        "grading": {
            "sync_grading",
            "embedding",  # 嵌套 section
        },
        "admin": {"enable_auth", "password"},
        "export": {"format"},
    }
    # grading.embedding 的允许键单独校验
    _allowed_grading_embedding = {"model", "device", "endpoint", "timeout_seconds"}

    errors: list[str] = []
    for section, allowed in _allowed.items():
        data = raw.get(section, {})
        if not isinstance(data, dict):
            errors.append(f"[{section}] 必须是 dict，实际是 {type(data).__name__}")
            continue
        unknown = set(data.keys()) - allowed
        if unknown:
            errors.append(f"[{section}] 未知配置项: {unknown}。已忽略，请检查拼写或删除。")
        # 递归校验 grading.embedding
        if section == "grading":
            emb = data.get("embedding", {})
            if not isinstance(emb, dict):
                errors.append("[grading.embedding] 必须是 dict")
            else:
                emb_unknown = set(emb.keys()) - _allowed_grading_embedding
                if emb_unknown:
                    errors.append(f"[grading.embedding] 未知配置项: {emb_unknown}")

    # 严格校验 embedding.model：必须是实际路径，不能是纯文字描述
    emb_section = raw.get("grading", {}).get("embedding", {})
    if isinstance(emb_section, dict):
        model = emb_section.get("model", "")
        if isinstance(model, str) and ("词向量" in model or "embedding" in model.lower() and "/" not in model and "." not in model):
            errors.append(
                f"[grading.embedding.model] 不能是描述性文字（实际收到: {model!r}）。"
                "请填写实际模型路径，例如 ollama 的 bge-m3 或 BAAI/bge-m3。"
            )

    if errors:
        print("⚠️  配置校验发现以下问题（已忽略未知字段，不影响启动）：", file=sys.stderr)
        for e in errors:
            print(f"  - {e}", file=sys.stderr)
        print(f"  已知配置项: {', '.join(sorted(_allowed.keys()))}", file=sys.stderr)
        if any("不能是描述性文字" in e for e in errors):
            sys.exit(1)


# ---------------------------------------------------------------------------
# 公共 API
# ---------------------------------------------------------------------------

def _load_config() -> AppConfig:
    with open(_CONFIG_PATH, "r", encoding="utf-8") as f:
        raw = yaml.safe_load(f) or {}
    raw = _resolve_env(raw)
    _validate_config(raw)
    return _build_config(raw)


def get_config() -> AppConfig:
    """返回当前配置单例。首次调用时自动加载。"""
    global _config
    if _config is None:
        _config = _load_config()
    return _config


def reload_config() -> AppConfig:
    """热重载配置，下次 get_config() 返回新值。"""
    global _config
    _config = _load_config()
    return _config
