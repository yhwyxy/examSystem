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
