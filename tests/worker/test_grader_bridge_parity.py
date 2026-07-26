"""Grader_bridge 主观题打分 parity 测试: 用 root .env RERANK 远端真实跑一次
Task 0 fixture paper.json 主观题, 验证 grader_bridge 与 subjective-scoring 0.1.7
API 对接正确. 实际网络调远端 Reranker API (~1s/单题, ~5-15s 整卷).

skip 条件: 没设 RERANK_API_KEY -> skip (避免无意中跑远端).
"""
import json
import os
import pathlib
import sys

import pytest

ROOT = pathlib.Path(__file__).resolve().parents[2]
FIXTURES = ROOT / "tests" / "fixtures" / "contract"
sys.path.insert(0, str(ROOT))


def _rerank_ready() -> bool:
    return all(os.getenv(k) for k in
               ("RERANK_USE_REMOTE", "RERANK_API_URL", "RERANK_API_KEY", "RERANK_MODEL"))


@pytest.fixture(scope="module")
def paper_doc():
    raw = (FIXTURES / "paper.json").read_text(encoding="utf-8")
    return json.loads(raw, parse_float=float)


@pytest.fixture(scope="module")
def subjective_questions(paper_doc):
    """挑主观题 (排除 single_choice/multiple_choice/true_false/section)."""
    SUBJ_TYPES = {"short_answer", "essay", "sql", "code", "composite",
                  "code_completion", "sql_completion", "calculation"}
    return [q for q in paper_doc.get("questions", []) if q.get("type") in SUBJ_TYPES]
