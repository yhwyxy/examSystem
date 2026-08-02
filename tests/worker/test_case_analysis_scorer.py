"""案例分析题评分器单元测试 —— 用 legal q35 SIM-H/SIM-M 真实作答。"""
import pathlib, sys
ROOT = pathlib.Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT))
from scoring_worker.case_analysis_scorer import score_case_analysis


LEGAL_Q35_1 = {
    "id": "q35-1", "type": "short_answer", "score": 4.0,
    "scoring_mode": "case_analysis",
    "question": "01号房屋的物权归属应当如何确定？为什么？",
    "answer": "甲、丙办理了过户登记，完成不动产物权公示，物权由甲变更为丙。",
    "scoring_points": [
        {"id": "p1", "text": "归丙", "score": 2.0, "match": "phrase",
         "synonyms": ["归属丙", "丙取得", "属于丙", "所有权归丙", "变更为丙"]},
    ],
}


def test_phrase_hit_gets_full_phrase_score():
    """SIM-H 作答包含结论短语 "归丙" -> 2.0 + auto_pass。"""
    r = score_case_analysis(
        LEGAL_Q35_1,
        "01号房屋归丙所有。不动产所有权以登记为准，甲丙已完成过户登记，故丙取得所有权。"
    )
    assert abs(r.score - 2.0) < 0.01, f"expected 2.0, got {r.score}"
    assert r.review_level == "auto_pass", (
        f"expected auto_pass, got {r.review_level}")


def test_phrase_miss_manual_required():
    """完全不包含结论短语 -> 0 + manual_required。"""
    r = score_case_analysis(
        LEGAL_Q35_1,
        "关于房屋物权归属，严格按照物权法的规定处理。登记是物权变动的公示方式。"
    )
    assert r.score == 0.0, f"expected 0.0, got {r.score}"
    assert r.review_level == "manual_required", (
        f"expected manual_required, got {r.review_level}")


def test_synonym_hit_gets_full_phrase_score():
    """作答包含同义词 "丙取得" -> 2.0 + auto_pass。"""
    r = score_case_analysis(
        LEGAL_Q35_1,
        "该房屋应归甲所有，但经买卖后丙取得所有权。"
    )
    assert abs(r.score - 2.0) < 0.01, f"expected 2.0, got {r.score}"
    assert r.review_level == "auto_pass", (
        f"expected auto_pass, got {r.review_level}")


def test_empty_answer():
    """空答案 -> 0 + auto_pass (由上游拒空，不触发 manual_required)。"""
    r = score_case_analysis(LEGAL_Q35_1, "")
    assert r.score == 0.0
    # 空答案 _enum_norm("") = "" 非真值，不会触发 manual_required