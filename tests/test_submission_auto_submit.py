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
