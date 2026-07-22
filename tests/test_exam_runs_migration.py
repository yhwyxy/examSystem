"""旧库迁移到 exam_runs / run_id 的冒烟测试。"""
from __future__ import annotations

import sqlite3
from pathlib import Path

from backend import database


def test_legacy_submissions_get_run_id_and_unique(tmp_path, monkeypatch):
    db_path = tmp_path / "legacy.db"
    # 先写一个旧 schema（无 run_id、会话为旧形态）
    conn = sqlite3.connect(str(db_path))
    conn.executescript(
        """
        CREATE TABLE submissions (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            name TEXT NOT NULL,
            employee_id TEXT NOT NULL,
            paper_id TEXT NOT NULL DEFAULT 'default',
            paper_name TEXT,
            department TEXT,
            answers_json TEXT NOT NULL,
            grading_detail_json TEXT NOT NULL,
            objective_score REAL NOT NULL DEFAULT 0,
            subjective_score_machine REAL NOT NULL DEFAULT 0,
            subjective_score_final REAL NOT NULL DEFAULT 0,
            total_score REAL NOT NULL DEFAULT 0,
            review_status TEXT NOT NULL DEFAULT 'pending',
            started_at TEXT,
            submitted_at TEXT NOT NULL,
            reviewed_at TEXT,
            reviewer_note TEXT,
            client_ip TEXT,
            user_agent TEXT,
            auto_submit_reason TEXT,
            UNIQUE(employee_id, paper_id)
        );
        CREATE TABLE exam_sessions (
            employee_id TEXT NOT NULL,
            paper_id TEXT NOT NULL DEFAULT 'default',
            started_at TEXT NOT NULL,
            created_at TEXT NOT NULL,
            PRIMARY KEY (employee_id, paper_id)
        );
        INSERT INTO submissions (
            name, employee_id, paper_id, paper_name, department,
            answers_json, grading_detail_json, total_score, review_status, submitted_at
        ) VALUES
            ('甲', 'E1', 'mech', '机电', NULL, '{}', '[]', 10, 'reviewed', '2026-01-01T00:00:00+00:00'),
            ('乙', 'E2', 'mech', '机电', NULL, '{}', '[]', 8, 'reviewed', '2026-01-02T00:00:00+00:00'),
            ('丙', 'E1', 'elec', '电气', NULL, '{}', '[]', 9, 'reviewed', '2026-01-03T00:00:00+00:00');
        """
    )
    conn.commit()
    conn.close()

    monkeypatch.setattr(database, "DB_PATH", db_path)
    monkeypatch.setattr(database, "_initialized", False)
    database.init_db()

    rows = database.list_submissions(limit=20)
    assert len(rows) == 3
    assert all(r.get("run_id") for r in rows)
    mech_runs = {r["run_id"] for r in rows if r["paper_id"] == "mech"}
    elec_runs = {r["run_id"] for r in rows if r["paper_id"] == "elec"}
    assert len(mech_runs) == 1
    assert len(elec_runs) == 1
    assert mech_runs != elec_runs

    run = database.get_run_by_id(next(iter(mech_runs)))
    assert run is not None
    assert run["is_legacy"] == 1
    assert run["status"] == "closed"
    assert run["public_token_hash"] is None

    # 新会话表形状
    with database.db_cursor() as c:
        cols = {r[1] for r in c.execute("PRAGMA table_info(exam_sessions)").fetchall()}
    assert "session_token_hash" in cols
    assert "draft_json" in cols
    assert "run_id" in cols
