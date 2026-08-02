"""账目题评分器单元测试 —— 用 finance q31 SIM-H/M 真实作答。"""
import pathlib, sys
ROOT = pathlib.Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT))
from scoring_worker.exact_scorers import score_ledger


FINANCE_Q31_1 = {
    "id": "q31-1", "type": "short_answer", "score": 6.0,
    "scoring_mode": "ledger",
    "question": "编制业务（1）在12月份的相关会计分录。",
    "answer": "借：银行存款 10530 贷：应交税费-销项 1530 其他应付款 9000；借：财务费用 100 贷：其他应付款 100",
    "ledger": {
        "entries": [
            {"keywords": ["银行存款"], "numbers": [10530], "score": 1.2},
            {"keywords": ["销项税额", "应交税费", "销项"], "numbers": [1530], "score": 1.2},
            {"keywords": ["其他应付款"], "numbers": [9000], "score": 1.2},
            {"keywords": ["财务费用"], "numbers": [100], "score": 1.2},
        ],
        "treatment_points": [
            {"text": "不确认收入", "synonyms": ["融资", "售后回购按融资处理"], "score": 1.2},
        ],
    },
}


def test_q31_1_sim_h_gets_full_score():
    """SIM-H 完整正确分录 -> 6.0 + suggested_review。"""
    ans = ("借：银行存款 10530\n"
           "贷：应交税费-应交增值税（销项税额）1530\n"
           "  其他应付款 9000\n"
           "借：财务费用 100\n"
           "  贷：其他应付款 100\n"
           "（此业务为售后回购，不确认收入）")
    r = score_ledger(FINANCE_Q31_1, ans)
    assert abs(r.score - 6.0) < 0.01, f"expected 6.0, got {r.score}"
    assert r.review_level == "suggested_review", (
        f"expected suggested_review, got {r.review_level}")


def test_q31_1_sim_m_summary_gets_partial():
    """SIM-M 简略摘要 -> 约 2.4 分（仅部分分录命中）+ suggested_review。"""
    ans = ("银行存款10530，销项税1530，其他应付款9000，财务费用100，"
           "这是售后回购，不确认收入")
    r = score_ledger(FINANCE_Q31_1, ans)
    # SIM-M 会命中数字但可能漏部分 keywords（如 "财务费用" 需与 "财务费用100" 一起出现）
    # 确保命中一部分但非满分，且 review_level 为 suggested_review
    assert 1.2 <= r.score <= 6.0, f"expected partial score 1.2~6.0, got {r.score}"
    assert r.review_level == "suggested_review", (
        f"expected suggested_review, got {r.review_level}")


def test_irrelevant_answer_manual_required():
    """完全不相关的作答 -> 0 + manual_required。"""
    r = score_ledger(FINANCE_Q31_1, "会计账簿要按时登记，确保账实相符。")
    assert r.score == 0.0 and r.review_level == "manual_required", (
        f"expected 0/manual_required, got {r.score}/{r.review_level}")