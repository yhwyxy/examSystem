"""多专业试卷录入与发布测试。"""
from __future__ import annotations

import json
from pathlib import Path

import pytest
from fastapi.testclient import TestClient


@pytest.fixture()
def papers_env(tmp_path, monkeypatch):
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
