from backend import database


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
    assert row["auto_submit_reason"] is None
