"""Tests for translation_scorer: language-check gate + phrase-level scoring."""
import pathlib, sys
ROOT = pathlib.Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT))
from scoring_worker.exact_scorers import score_translation

LOGISTICS_Q52 = {
    "id": "q52", "type": "short_answer", "score": 10.0,
    "scoring_mode": "translation", "translation": {"target_lang": "en"},
    "question": "翻译：船长，为了保持船体平衡，我们要换3、5舱作业，请打开舱口。",
    "answer": "CAPTAIN! IN ORDER TO KEEP SHIP'S BALANCE, WE WILL SHIFT TO HOLD NO.3&5, PLEASE OPEN HATCH COVER.",
    "scoring_points": [
        {"id": "p1", "text": "CAPTAIN", "score": 2.0},
        {"id": "p2", "text": "IN ORDER TO KEEP SHIP'S BALANCE", "score": 3.0,
         "synonyms": ["keep the ship balanced", "keep ship balance", "even keel"]},
        {"id": "p3", "text": "WE WILL SHIFT TO HOLD NO.3&5", "score": 3.0,
         "synonyms": ["shift to no.3 and no.5", "shift to hold no.3", "no.3 and no.5 hatches"]},
        {"id": "p4", "text": "PLEASE OPEN HATCH COVER", "score": 2.0,
         "synonyms": ["open the hatch", "open hatch"]},
    ],
}


def test_correct_english_translation_full_marks():
    # SIM-M 真实作答：v0.1.11 被 "No. 3" 否定词误判压到 5.0/10，人工估分 10
    ans = "Captain, we will shift to No. 3 and No. 5 hatches to keep the ship balanced. Please open the hatches."
    r = score_translation(LOGISTICS_Q52, ans)
    assert r.score == 10.0 and r.review_level == "auto_pass"


def test_chinese_answer_to_english_task_zero():
    # SIM-L 真实作答：v0.1.11 跨语言相似度虚高给 5.7 且 auto_pass
    r = score_translation(LOGISTICS_Q52, "船上的事情听安排，按规范操作，注意安全。")
    assert r.score == 0.0 and r.review_level == "manual_required"