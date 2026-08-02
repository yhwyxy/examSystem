"""lint_papers 单元测试（不加载 reranker 模型）。"""
import pathlib
import sys

ROOT = pathlib.Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))

from scripts.lint_papers import lint_paper


def _paper(*questions):
    return {"paper_id": "t", "name": "t", "exam_info": {}, "questions": list(questions)}


def test_meta_point_detected():
    q = {"id": "q1", "type": "short_answer", "score": 10, "scoring_mode": "text",
         "question": "x", "answer": "x",
         "scoring_points": [{"id": "p1", "text": "答出参考答案中的公司环保工作之一", "score": 5}]}
    assert any("L1" in e for e in lint_paper(_paper(q)))


def test_date_in_point_detected():
    q = {"id": "q1", "type": "short_answer", "score": 10, "scoring_mode": "text",
         "question": "x", "answer": "x",
         "scoring_points": [{"id": "p1", "text": "2月12日双方变更合同", "score": 10}]}
    assert any("L3" in e for e in lint_paper(_paper(q)))


def test_table_score_sum_checked():
    q = {"id": "q1", "type": "short_answer", "score": 10, "scoring_mode": "table",
         "question": "x", "answer": "x",
         "table": {"cells": [{"label": "a", "expected": ["1"], "score": 4}]}}
    assert any("L5" in e for e in lint_paper(_paper(q)))


def test_clean_paper_passes():
    q = {"id": "q1", "type": "short_answer", "score": 10, "scoring_mode": "enumeration",
         "question": "x", "answer": "x",
         "scoring_points": [{"id": "p1", "text": "大气污染", "score": 10}]}
    assert lint_paper(_paper(q)) == []
