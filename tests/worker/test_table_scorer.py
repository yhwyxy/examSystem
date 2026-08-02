"""Tests for table_scorer: cell-level exact matching with optional label-context check."""
import pathlib, sys
ROOT = pathlib.Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT))
from scoring_worker.exact_scorers import score_table

COMM_Q44 = {
    "id": "q44", "type": "short_answer", "score": 10.0,
    "scoring_mode": "table", "question": "根据表格中的IP地址规律，完善表格。",
    "answer": "A 掩码 255.255.255.0；B 网段 172.16.0.0，结束 172.16.255.254；C 起始 192.168.1.1，结束 192.168.1.254",
    "table": {"cells": [
        {"label": "a", "expected": ["255.255.255.0"], "score": 2.0, "require_label_context": True},
        {"label": "b", "expected": ["172.16.0.0"], "score": 2.0},
        {"label": "b", "expected": ["172.16.255.254"], "score": 2.0},
        {"label": "c", "expected": ["192.168.1.1"], "score": 2.0},
        {"label": "c", "expected": ["192.168.1.254"], "score": 2.0},
    ]},
}


def test_generic_mask_statement_scores_low():
    # SIM-M 真实作答：默认掩码通论，未填表。v0.1.11 给了 6.5，人工估分 0
    ans = "按首段分A、B、C类，默认掩码分别为255.0.0.0、255.255.0.0、255.255.255.0。"
    r = score_table(COMM_Q44, ans)
    assert r.score <= 2.0 and r.review_level == "manual_required"


def test_filled_table_full_marks():
    r = score_table(COMM_Q44, COMM_Q44["answer"])
    assert r.score == 10.0 and r.review_level == "auto_pass"