"""Bridge 透传 calculation / extra_equivalences 测试（不加载 reranker 模型）。"""
import pathlib
import sys

ROOT = pathlib.Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT))

from scoring_worker.grader_bridge import build_scoring_request

CALC_Q = {
    "id": "q42",
    "type": "short_answer",
    "score": 10.0,
    "scoring_mode": "calculation",
    "question": "吨矿产气是多少kg/t?",
    "answer": "吨矿产气量=35*24*30*1000/335000=75.22kg/t",
    "scoring_points": [{"id": "p1", "text": "结果为 75.22kg/t", "score": 10}],
    "calculation": {
        "strategy": "static_values",
        "steps": [
            {"id": "s1", "description": "月产气量 25200 吨", "expected": 25200, "score": 4, "tolerance": 1},
        ],
        "final_answers": [
            {"id": "f1", "description": "吨矿产气 kg/t", "expected": 75.22, "score": 6, "tolerance": 0.5},
        ],
    },
}


def test_calculation_mode_and_config_passthrough():
    req = build_scoring_request(CALC_Q, "月产气=35×24×30=25200吨，吨矿产气≈75.2 kg/t")
    assert req.scoring_mode.value == "calculation"
    calc = req.scoring_config.calculation
    assert calc.final_answers[0].expected == 75.22
    assert calc.final_answers[0].tolerance == 0.5


def test_calculation_mode_without_config_stays_text():
    q = dict(CALC_Q)
    q.pop("calculation")
    req = build_scoring_request(q, "答案")
    assert req.scoring_mode.value == "text"


def test_extra_equivalences_passthrough():
    q = {
        "id": "q42-2",
        "type": "short_answer",
        "score": 10.0,
        "scoring_mode": "text",
        "question": "提高淬透性的合金元素",
        "answer": "B、Mn、Mo、Cr、Si、Ni。",
        "scoring_points": [{"id": "p1", "text": "Mn", "score": 5}],
        "extra_equivalences": [["Mn", "锰"], ["Cr", "铬"]],
    }
    req = build_scoring_request(q, "锰、铬等元素")
    eqs = req.scoring_config.text_bounded_corrections.extra_equivalences
    assert ("Mn", "锰") in eqs
    assert ("Cr", "铬") in eqs
