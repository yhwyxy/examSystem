"""分派器兜底行为——不加载 reranker 模型即可跑。"""
import pathlib, sys
ROOT = pathlib.Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT))
from scoring_worker.grader_bridge import _grade_by_exact_mode


def test_no_mode_falls_through_to_text():
    q = {"id": "x", "type": "short_answer", "score": 10, "question": "q", "answer": "a"}
    assert _grade_by_exact_mode("", q, "ans") is None
    assert _grade_by_exact_mode("text", q, "ans") is None
    assert _grade_by_exact_mode("nonsense", q, "ans") is None


def test_placeholder_scorer_forces_manual_review():
    q = {"id": "x", "type": "short_answer", "score": 10,
         "scoring_mode": "enumeration", "question": "q", "answer": "a"}
    final, machine, status, entry = _grade_by_exact_mode("enumeration", q, "ans")
    assert (final, machine, status) == (0.0, 0.0, "need_review")
    assert entry["need_manual_review"] is True