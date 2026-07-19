"""多专业试卷录入与发布测试。"""
from __future__ import annotations

import json
import sys
import types
from enum import Enum
from pathlib import Path

import pytest
from fastapi.testclient import TestClient


def _install_subjective_scoring_stub(monkeypatch):
    """隔离链接测试与主观题评分库依赖。"""
    if "subjective_scoring" in sys.modules:
        return

    class ScoringMode(Enum):
        TEXT = "text"
        SQL = "sql"
        CODE = "code"
        CALCULATION = "calculation"

    class ReviewLevel(Enum):
        MANUAL_REQUIRED = "manual_required"
        SUGGESTED_REVIEW = "suggested_review"
        PASS = "pass"

    class ScoringRequest:
        @classmethod
        def model_validate(cls, payload):
            obj = cls()
            obj.__dict__.update(payload)
            return obj

    class ScoringResult:
        pass

    class CohereRerankerPairScorer:
        def __init__(self, *args, **kwargs):
            pass

        def close(self):
            pass

    class SubjectiveScoringService:
        def __init__(self, *args, **kwargs):
            pass

    module = types.ModuleType("subjective_scoring")
    module.CohereRerankerPairScorer = CohereRerankerPairScorer
    module.ReviewLevel = ReviewLevel
    module.ScoringMode = ScoringMode
    module.ScoringRequest = ScoringRequest
    module.ScoringResult = ScoringResult
    module.SubjectiveScoringService = SubjectiveScoringService
    monkeypatch.setitem(sys.modules, "subjective_scoring", module)


@pytest.fixture()
def papers_env(tmp_path, monkeypatch):
    _install_subjective_scoring_stub(monkeypatch)
    from backend import question_loader as ql
    from backend import paper_store
    from backend import database

    papers = tmp_path / "papers"
    papers.mkdir()
    backups = tmp_path / "backups" / "papers"
    backups.mkdir(parents=True)
    (papers / "index.json").write_text(json.dumps({"papers": []}), encoding="utf-8")

    monkeypatch.setattr(ql, "PAPERS_DIR", papers)
    monkeypatch.setattr(ql, "INDEX_PATH", papers / "index.json")
    monkeypatch.setattr(ql, "BACKUPS_DIR", backups)
    monkeypatch.setattr(ql, "DATA_DIR", tmp_path)
    monkeypatch.setattr(ql, "LEGACY_QUESTIONS_PATH", tmp_path / "questions.json")
    ql.clear_question_cache()

    # isolated db
    db_path = tmp_path / "exam.db"
    monkeypatch.setattr(database, "DB_PATH", db_path)
    database._initialized = False
    database.init_db()
    return tmp_path


def _sample_questions():
    return [
        {
            "id": "q1",
            "type": "single_choice",
            "question": "1+1?",
            "options": [{"key": "A", "text": "1"}, {"key": "B", "text": "2"}],
            "answer": "B",
            "score": 5,
        },
        {
            "id": "q2",
            "type": "short_answer",
            "question": "REST?",
            "answer": "资源导向与 HTTP 方法",
            "score": 10,
            "scoring_mode": "text",
            "scoring_points": [
                {"id": "p1", "text": "资源导向", "score": 5, "required": True},
                {"id": "p2", "text": "HTTP 方法", "score": 5, "required": False},
            ],
        },
    ]


def test_calculation_question_schema_is_accepted():
    from backend.question_loader import validate_questions

    data = {
        "exam_info": {"title": "计算题测试"},
        "questions": [
            {
                "id": "calc-1",
                "type": "short_answer",
                "question": "计算 1+1",
                "answer": "2",
                "score": 10,
                "scoring_mode": "calculation",
                "calculation": {
                    "steps": [],
                    "final_answers": [
                        {
                            "id": "result",
                            "description": "最终结果",
                            "expected": 2,
                            "score": 10,
                        }
                    ],
                },
            }
        ],
    }

    validate_questions(data)


def test_create_save_open_close_and_exam_flow(papers_env):
    from backend import paper_store, question_loader
    from backend.main import app

    meta = paper_store.create_paper(slug="mech", name="机电")
    assert meta["status"] == "closed"

    full = paper_store.save_paper("mech", {
        "name": "机电",
        "exam_info": {"title": "机电考试", "description": "d", "passing_score": 6},
        "questions": _sample_questions(),
    })
    assert full["exam_info"]["total_score"] == 15
    assert len(full["questions"]) == 2

    # closed: student cannot load
    client = TestClient(app)
    r = client.get("/api/exam", params={"paper": "mech"})
    assert r.status_code == 403

    paper_store.set_status("mech", "open")
    # open: cannot edit
    with pytest.raises(Exception):
        paper_store.add_question("mech", {
            "type": "true_false", "question": "x", "answer": True, "score": 1,
        })

    r = client.get("/api/exam", params={"paper": "mech"})
    assert r.status_code == 200
    body = r.json()
    assert body["paper_id"] == "mech"
    assert "answer" not in body["questions"][0]
    assert "scoring_points" not in body["questions"][1]

    # start + submit
    r = client.post("/api/exam/start", json={"employee_id": "E1", "paper_id": "mech"})
    assert r.status_code == 200
    r = client.post("/api/submit", json={
        "name": "张三",
        "employee_id": "E1",
        "paper_id": "mech",
        "answers": {"q1": "B", "q2": "资源导向 HTTP"},
    })
    assert r.status_code == 200

    # duplicate same paper
    client.post("/api/exam/start", json={"employee_id": "E1", "paper_id": "mech"})
    r = client.post("/api/submit", json={
        "name": "张三", "employee_id": "E1", "paper_id": "mech", "answers": {"q1": "A"},
    })
    assert r.status_code == 409

    # second paper allowed for same employee
    paper_store.create_paper(slug="elec", name="电气")
    paper_store.save_paper("elec", {
        "name": "电气",
        "exam_info": {"title": "电气考试", "passing_score": 3},
        "questions": [_sample_questions()[0]],
    })
    paper_store.set_status("elec", "open")
    r = client.post("/api/exam/start", json={"employee_id": "E1", "paper_id": "elec"})
    assert r.status_code == 200
    r = client.post("/api/submit", json={
        "name": "张三", "employee_id": "E1", "paper_id": "elec", "answers": {"q1": "B"},
    })
    assert r.status_code == 200

    paper_store.set_status("mech", "closed")
    # after close editable again
    paper_store.update_question("mech", "q1", {
        "id": "q1",
        "type": "single_choice",
        "question": "1+1? updated",
        "options": [{"key": "A", "text": "1"}, {"key": "B", "text": "2"}],
        "answer": "B",
        "score": 5,
    })


def test_missing_paper_param(papers_env):
    from backend.main import app
    client = TestClient(app)
    r = client.get("/api/exam")
    assert r.status_code == 400
    assert r.json()["detail"]["code"] == "PAPER_REQUIRED"


def test_paper_exam_link_uses_request_origin(papers_env):
    from backend import paper_store
    from backend.main import app

    paper_store.create_paper(slug="mech", name="机电")
    client = TestClient(app, base_url="https://exam.example.com")

    r = client.get("/api/admin/papers/mech/exam-link")

    assert r.status_code == 200
    assert r.json()["url"] == "https://exam.example.com/exam?paper=mech"


def test_sanitize_scoring_points():
    from backend.question_loader import sanitize_for_student
    qs = sanitize_for_student([{
        "id": "q1", "type": "short_answer", "question": "x", "score": 1,
        "answer": "secret", "scoring_rubric": "r", "scoring_points": [{"id": "p1", "text": "t", "score": 1}],
    }])
    assert "answer" not in qs[0]
    assert "scoring_rubric" not in qs[0]
    assert "scoring_points" not in qs[0]


def test_composite_sub_questions_schema_ok():
    from backend.question_loader import validate_questions

    data = {
        "exam_info": {"title": "复合题"},
        "questions": [
            {
                "id": "c1",
                "type": "short_answer",
                "question": "阅读下列材料并回答",
                "score": 10,
                "sub_questions": [
                    {
                        "id": "s1",
                        "question": "解释概念",
                        "answer": "参考A",
                        "score": 4,
                        "scoring_mode": "text",
                    },
                    {
                        "id": "s2",
                        "question": "写 SQL",
                        "answer": "SELECT 1",
                        "score": 6,
                        "scoring_mode": "sql",
                        "code_language": "SQL",
                    },
                ],
            }
        ],
    }
    validate_questions(data)
    q = data["questions"][0]
    assert q["type"] == "composite"
    assert len(q["subquestions"]) == 2
    assert q["subquestions"][1]["code_language"] == "sql"


def test_composite_canonical_schema_and_legacy_aliases():
    from backend.question_loader import get_subquestions, validate_questions

    paper = {
        "exam_info": {"title": "复合题"},
        "questions": [{
            "id": "c1", "type": "composite", "question": "父题", "score": 10,
            "subquestions": [
                {"id": "s1", "question": "解释", "answer": "A", "score": 4,
                 "scoring_mode": "text"},
                {"id": "s2", "question": "编码", "answer": "print(1)", "score": 6,
                 "scoring_mode": "code", "allowed_languages": [" Python ", "javascript"]},
            ],
        }],
    }

    validate_questions(paper)
    assert get_subquestions(paper["questions"][0])[1]["allowed_languages"] == [
        "python", "javascript"
    ]

    legacy = {
        "id": "legacy", "type": "short_answer", "question": "父", "score": 5,
        "sub_questions": [{
            "id": "code", "question": "编码", "answer": "print(1)", "score": 5,
            "scoring_mode": "code", "code_language": "Python",
        }],
    }
    assert get_subquestions(legacy)[0]["allowed_languages"] == ["python"]
    assert legacy["type"] == "composite"
    assert "sub_questions" not in legacy


@pytest.mark.parametrize("subquestions", [None, []])
def test_composite_requires_nonempty_subquestions_even_with_parent_answer(subquestions):
    from backend.question_loader import validate_questions
    from fastapi import HTTPException

    question = {
        "id": "c1",
        "type": "composite",
        "question": "父题",
        "answer": "父题参考答案不能替代子题",
        "score": 10,
    }
    if subquestions is not None:
        question["subquestions"] = subquestions
    paper = {"exam_info": {"title": "复合题"}, "questions": [question]}

    with pytest.raises(HTTPException, match="subquestions.*非空"):
        validate_questions(paper)


def test_code_subanswer_language_must_be_allowed():
    from backend.question_loader import normalize_submitted_subanswer

    sub = {"id": "s1", "scoring_mode": "code", "allowed_languages": ["python"]}
    assert normalize_submitted_subanswer(
        sub, {"answer": "print(1)", "language": "Python"}
    ) == ("print(1)", "python")
    with pytest.raises(ValueError, match="INVALID_CODE_LANGUAGE"):
        normalize_submitted_subanswer(
            sub, {"answer": "console.log(1)", "language": "javascript"}
        )


def test_update_composite_question_preserves_subquestion_scoring_metadata(papers_env):
    from backend import paper_store

    paper_store.create_paper(slug="mech", name="机电")
    paper_store.add_question(
        "mech",
        {
            "id": "c1",
            "type": "composite",
            "question": "父题",
            "score": 7,
            "subquestions": [
                {
                    "id": "s1",
                    "question": "测量范围",
                    "answer": "100-200 kPa",
                    "score": 7,
                    "scoring_mode": "calculation",
                    "calculation": {
                        "final_answers": [
                            {
                                "id": "lower",
                                "description": "下限",
                                "expected": 100,
                                "score": 3.5,
                                "tolerance": 0.1,
                            },
                            {
                                "id": "upper",
                                "description": "上限",
                                "expected": 200,
                                "score": 3.5,
                                "tolerance": 0.1,
                            },
                        ]
                    },
                    "scoring_points": [
                        {"id": "p1", "text": "上下界正确", "score": 7}
                    ],
                    "scoring_rubric": "按上下界分别给分",
                }
            ],
        },
    )

    updated = paper_store.update_question(
        "mech",
        "c1",
        {
            "id": "c1",
            "type": "composite",
            "question": "父题-编辑后",
            "score": 7,
            "subquestions": [
                {
                    "id": "s1",
                    "question": "测量范围（含上下界）",
                    "answer": "100-200 kPa",
                    "score": 7,
                    "scoring_mode": "calculation",
                }
            ],
        },
    )

    sub = updated["subquestions"][0]
    assert sub["question"] == "测量范围（含上下界）"
    assert sub["calculation"]["final_answers"][0]["expected"] == 100
    assert sub["calculation"]["final_answers"][1]["expected"] == 200
    assert sub["scoring_points"][0]["id"] == "p1"
    assert sub["scoring_rubric"] == "按上下界分别给分"


def test_instrumentation_q43_1_is_calculation_with_two_bounds():
    import json
    from pathlib import Path

    from backend.question_loader import validate_questions

    data_path = Path(__file__).resolve().parents[1] / "data" / "papers" / "instrumentation.json"
    data = json.loads(data_path.read_text(encoding="utf-8"))
    validate_questions(data)

    q43 = next(q for q in data["questions"] if q["id"] == "q43")
    q431 = next(s for s in q43["subquestions"] if s["id"] == "q43-1")

    assert q431["scoring_mode"] == "calculation"
    finals = q431["calculation"]["final_answers"]
    assert [item["expected"] for item in finals] == [100, 200]
    assert sum(item["score"] for item in finals) == q431["score"]


def test_composite_rejects_duplicate_languages_after_normalization():
    from backend.question_loader import validate_questions
    from fastapi import HTTPException

    paper = {
        "exam_info": {"title": "复合题"},
        "questions": [{
            "id": "c1", "type": "composite", "question": "父题", "score": 5,
            "subquestions": [{
                "id": "s1", "question": "编码", "answer": "print(1)", "score": 5,
                "scoring_mode": "code", "allowed_languages": ["Python", " python "],
            }],
        }],
    }

    with pytest.raises(HTTPException, match="allowed_languages 不能重复"):
        validate_questions(paper)


def test_sanitize_canonical_composite_exposes_only_public_configuration():
    from backend.question_loader import sanitize_for_student

    out = sanitize_for_student([{
        "id": "c1", "type": "composite", "question": "父", "score": 5,
        "subquestions": [{
            "id": "s1", "question": "编码", "answer": "print(1)", "score": 5,
            "scoring_mode": "code", "allowed_languages": ["python"],
            "scoring_points": [{"text": "正确", "score": 5}],
        }],
    }])[0]

    assert out["subquestions"] == [{
        "id": "s1", "question": "编码", "score": 5,
        "scoring_mode": "code", "allowed_languages": ["python"],
    }]


def test_composite_score_must_equal_sum():
    from backend.question_loader import validate_questions
    from fastapi import HTTPException

    data = {
        "exam_info": {"title": "t"},
        "questions": [
            {
                "id": "c1",
                "type": "short_answer",
                "question": "父",
                "score": 9,
                "sub_questions": [
                    {"id": "s1", "question": "a", "answer": "a", "score": 4, "scoring_mode": "text"},
                    {"id": "s2", "question": "b", "answer": "b", "score": 6, "scoring_mode": "text"},
                ],
            }
        ],
    }
    with pytest.raises(HTTPException) as ei:
        validate_questions(data)
    assert "subquestions" in str(ei.value.detail).lower() or "子题" in str(ei.value.detail)


def test_code_mode_requires_language():
    from backend.question_loader import validate_questions
    from fastapi import HTTPException

    data = {
        "exam_info": {"title": "t"},
        "questions": [
            {
                "id": "q1",
                "type": "short_answer",
                "question": "写代码",
                "answer": "print(1)",
                "score": 5,
                "scoring_mode": "code",
            }
        ],
    }
    with pytest.raises(HTTPException) as ei:
        validate_questions(data)
    assert "code_language" in str(ei.value.detail)


def test_validate_answer_shape_composite_and_single():
    from backend.question_loader import validate_answer_shape

    composite = {
        "id": "c1",
        "type": "short_answer",
        "sub_questions": [{"id": "s1", "score": 5}, {"id": "s2", "score": 5}],
    }
    validate_answer_shape(composite, {"s1": "a", "s2": "b"})
    with pytest.raises(ValueError, match="INVALID_ANSWER_SHAPE"):
        validate_answer_shape(composite, "plain string")
    with pytest.raises(ValueError, match="INVALID_ANSWER_SHAPE"):
        validate_answer_shape(composite, {"s1": "only-one"})

    single = {"id": "q1", "type": "short_answer"}
    validate_answer_shape(single, "ok")
    with pytest.raises(ValueError, match="INVALID_ANSWER_SHAPE"):
        validate_answer_shape(single, {"s1": "x"})


def test_sanitize_keeps_sub_questions_strips_answers():
    from backend.question_loader import sanitize_for_student

    qs = sanitize_for_student([
        {
            "id": "c1",
            "type": "short_answer",
            "question": "父干",
            "score": 10,
            "answer": "不应出现",
            "sub_questions": [
                {
                    "id": "s1",
                    "question": "子1",
                    "answer": "密钥",
                    "score": 10,
                    "scoring_mode": "text",
                    "scoring_points": [{"id": "p1", "text": "点", "score": 10}],
                    "code_language": "python",
                }
            ],
        }
    ])
    out = qs[0]
    assert "answer" not in out
    assert "scoring_points" not in out
    assert out["subquestions"][0]["id"] == "s1"
    assert out["subquestions"][0]["question"] == "子1"
    assert "answer" not in out["subquestions"][0]
    assert "scoring_points" not in out["subquestions"][0]
    assert out["subquestions"][0].get("scoring_mode") == "text"
