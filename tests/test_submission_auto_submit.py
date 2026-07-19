from backend import database
from backend.main import SubmitRequest
from pydantic import ValidationError
import pytest


def test_submission_persists_auto_submit_reason(tmp_path, monkeypatch):
    monkeypatch.setattr(database, "DB_PATH", tmp_path / "exam.db")
    monkeypatch.setattr(database, "_initialized", False)
    database.init_db()

    submission_id = database.insert_submission_pending(
        name="张三",
        employee_id="E001",
        paper_id="paper-1",
        department=None,
        answers={"q1": "A"},
        started_at=None,
        client_ip=None,
        user_agent=None,
        auto_submit_reason="third_blur",
    )

    rows = database.list_submissions()
    row = next(item for item in rows if item["id"] == submission_id)
    assert row["auto_submit_reason"] == "third_blur"


def test_normal_submission_has_no_auto_submit_reason(tmp_path, monkeypatch):
    monkeypatch.setattr(database, "DB_PATH", tmp_path / "exam.db")
    monkeypatch.setattr(database, "_initialized", False)
    database.init_db()

    submission_id = database.insert_submission_pending(
        name="李四",
        employee_id="E002",
        paper_id="paper-1",
        department=None,
        answers={"q1": "A"},
        started_at=None,
        client_ip=None,
        user_agent=None,
    )

    row = next(item for item in database.list_submissions() if item["id"] == submission_id)


def test_submit_request_accepts_only_known_auto_submit_reasons():
    assert SubmitRequest(
        name="张三",
        employee_id="E001",
        paper_id="paper-1",
        answers={"q1": "A"},
        auto_submit_reason="third_blur",
    ).auto_submit_reason == "third_blur"

    with pytest.raises(ValidationError):
        SubmitRequest(
            name="张三",
            employee_id="E001",
            paper_id="paper-1",
            answers={"q1": "A"},
            auto_submit_reason="client_supplied_score",
        )


def test_submit_rejects_unknown_parent_question_id_and_does_not_persist(
    tmp_path, monkeypatch
):
    from fastapi.testclient import TestClient

    from backend import database, paper_store, question_loader as ql
    from backend.main import app

    papers = tmp_path / "papers"
    papers.mkdir()
    backups = tmp_path / "backups" / "papers"
    backups.mkdir(parents=True)
    (papers / "index.json").write_text('{"papers": []}', encoding="utf-8")

    monkeypatch.setattr(ql, "PAPERS_DIR", papers)
    monkeypatch.setattr(ql, "INDEX_PATH", papers / "index.json")
    monkeypatch.setattr(ql, "BACKUPS_DIR", backups)
    monkeypatch.setattr(ql, "DATA_DIR", tmp_path)
    monkeypatch.setattr(ql, "LEGACY_QUESTIONS_PATH", tmp_path / "questions.json")
    ql.clear_question_cache()

    monkeypatch.setattr(database, "DB_PATH", tmp_path / "exam.db")
    monkeypatch.setattr(database, "_initialized", False)
    database.init_db()

    paper_store.create_paper(slug="mech", name="机电")
    paper_store.save_paper(
        "mech",
        {
            "name": "机电",
            "exam_info": {"title": "机电考试", "passing_score": 3},
            "questions": [
                {
                    "id": "q1",
                    "type": "short_answer",
                    "question": "已知题",
                    "answer": "A",
                    "score": 5,
                }
            ],
        },
    )
    paper_store.set_status("mech", "open")

    # 避免异步评分副作用干扰断言
    monkeypatch.setattr("backend.main.schedule_grading", lambda *args, **kwargs: None)

    client = TestClient(app)
    start = client.post(
        "/api/exam/start",
        json={"name": "张三", "employee_id": "E1", "paper_id": "mech"},
    )
    assert start.status_code == 200

    r = client.post(
        "/api/submit",
        json={
            "name": "张三",
            "employee_id": "E1",
            "paper_id": "mech",
            "answers": {
                "q1": "合法答案",
                "q-unknown": "ghost",
            },
        },
    )
    assert r.status_code == 422
    body = r.json()
    assert body["detail"]["code"] == "UNKNOWN_QUESTION_ID"
    assert "q-unknown" in body["detail"]["message"]

    submissions = database.list_submissions(paper_id="mech")
    assert submissions == []

def test_composite_submission_rejects_forged_code_language():
    from backend.question_loader import validate_answer_shape

    question = {
        "id": "c1", "type": "composite", "score": 5,
        "subquestions": [{
            "id": "s1", "scoring_mode": "code", "allowed_languages": ["python"],
        }],
    }

    with pytest.raises(ValueError, match="INVALID_CODE_LANGUAGE"):
        validate_answer_shape(question, {
            "s1": {"answer": "console.log(1)", "language": "javascript"},
        })
