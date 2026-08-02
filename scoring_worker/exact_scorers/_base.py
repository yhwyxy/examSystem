from __future__ import annotations
from dataclasses import dataclass
from typing import Any


@dataclass
class ExactScoreResult:
    score: float
    detail: dict[str, Any]
    review_level: str  # "auto_pass" | "suggested_review" | "manual_required"