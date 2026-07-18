import json


def test_composite_review_status_uses_subquestion_semantics():
    from backend.grader import aggregate_composite_review_status

    assert aggregate_composite_review_status([]) == "pending"
    assert aggregate_composite_review_status([
        {"review_status": "reviewed"}, {"review_status": "reviewed"},
    ]) == "reviewed"
    assert aggregate_composite_review_status([
        {"review_status": "reviewed"}, {"review_status": "high_confidence"},
    ]) == "high_confidence"
    assert aggregate_composite_review_status([
        {"review_status": "high_confidence", "low_confidence": True},
    ]) == "need_review"
    assert aggregate_composite_review_status([
        {
            "review_status": "need_review",
            "low_confidence": True,
            "need_manual_review": True,
        },
    ]) == "need_review"
    assert aggregate_composite_review_status([
        {"review_status": "high_confidence", "need_manual_review": True},
    ]) == "need_review"
    assert aggregate_composite_review_status([
        {"review_status": "pending"},
    ]) == "need_review"


def test_regrade_preserves_only_reviewed_scores_and_recomputes_totals(tmp_path, monkeypatch):
    from backend import database, review_service
    from backend.grader import GradingResult

    monkeypatch.setattr(database, "DB_PATH", tmp_path / "regrade.db")
    monkeypatch.setattr(database, "_initialized", False)
    database.init_db()

    original_detail = [
        {
            "question_id": "q1", "type": "single_choice", "max_score": 3,
            "machine_score": 2, "score": 2, "final_score": 2,
            "review_status": "reviewed",
        },
        {
            "question_id": "q2", "type": "composite", "is_composite": True,
            "max_score": 10, "machine_score": 5, "score": 5, "final_score": 5,
            "review_status": "reviewed",
            "sub_results": [
                {
                    "sub_question_id": "s1", "max_score": 4,
                    "machine_score": 2, "score": 2, "final_score": 2,
                    "review_status": "reviewed",
                },
                {
                    "sub_question_id": "s2", "max_score": 6,
                    "machine_score": 3, "score": 3, "final_score": 3,
                    "review_status": "high_confidence",
                },
            ],
        },
        {
            "question_id": "q3", "type": "short_answer", "max_score": 5,
            "machine_score": 2, "score": 2, "final_score": 2,
            "review_status": "reviewed",
        },
        {
            "question_id": "q4", "type": "short_answer", "max_score": 6,
            "machine_score": 1, "score": 1, "final_score": 1,
            "review_status": "reviewed",
        },
    ]
    submission_id = database.insert_submission(
        name="重评测试", employee_id="E-REGR", paper_id="p1", paper_name="P1",
        department="D", answers={"q1": "A", "q2": {"s1": "a", "s2": "b"}},
        grading_detail=original_detail,
        scores={
            "objective_score": 2, "subjective_score_machine": 8,
            "subjective_score_final": 8, "total_score": 10,
        },
        review_status="reviewed", started_at=None, client_ip=None, user_agent=None,
    )
    database.apply_review(
        submission_id=submission_id, question_id="q2", sub_question_id="s1",
        new_score=4, note="子题人工确认",
    )
    database.apply_review(
        submission_id=submission_id, question_id="q3", new_score=5,
        note="整题人工确认",
    )

    graded = GradingResult(
        objective_score=3, subjective_score_machine=16,
        subjective_score_final=16, total_score=19, review_status="reviewed",
        grading_detail=[
            {
                "question_id": "q1", "type": "single_choice", "max_score": 3,
                "machine_score": 3, "score": 3, "final_score": 3,
                "review_status": "reviewed",
            },
            {
                "question_id": "q2", "type": "composite", "is_composite": True,
                "max_score": 10, "machine_score": 6, "score": 6, "final_score": 6,
                "review_status": "reviewed",
                "sub_results": [
                    {
                        "sub_question_id": "s1", "max_score": 4,
                        "machine_score": 1, "score": 1, "final_score": 1,
                        "review_status": "low_confidence",
                        "low_confidence": True,
                        "need_manual_review": True,
                    },
                    {
                        "sub_question_id": "s2", "max_score": 6,
                        "machine_score": 5, "score": 5, "final_score": 5,
                        "review_status": "high_confidence",
                        "low_confidence": False,
                        "need_manual_review": False,
                    },
                ],
            },
            {
                "question_id": "q3", "type": "short_answer", "max_score": 5,
                "machine_score": 4, "score": 4, "final_score": 4,
                "review_status": "reviewed",
            },
            {
                "question_id": "q4", "type": "short_answer", "max_score": 6,
                "machine_score": 6, "score": 6, "final_score": 6,
                "review_status": "reviewed",
            },
        ],
    )

    async def fake_grade_submission(answers, paper_id=None):
        assert paper_id == "p1"
        return graded

    monkeypatch.setattr(review_service, "grade_submission", fake_grade_submission)

    result = review_service.regrade_submission(submission_id)
    stored = database.get_submission(submission_id)
    assert stored is not None
    details = {d["question_id"]: d for d in stored["grading_detail"]}
    composite = details["q2"]
    subs = {s["sub_question_id"]: s for s in composite["sub_results"]}

    assert result == {"success": True, "total_score": 23.0, "review_status": "pending"}
    assert (subs["s1"]["machine_score"], subs["s1"]["score"], subs["s1"]["final_score"]) == (1, 1, 4)
    assert subs["s1"]["reviewed_by"] == "human"
    assert subs["s1"]["review_note"] == "子题人工确认"
    assert subs["s1"]["low_confidence"] is False
    assert subs["s1"]["need_manual_review"] is False
    assert (subs["s2"]["machine_score"], subs["s2"]["final_score"]) == (5, 5)
    assert (composite["machine_score"], composite["score"], composite["final_score"]) == (6.0, 6.0, 9.0)
    assert composite["review_status"] == "high_confidence"
    assert composite["low_confidence"] is False
    assert composite["need_manual_review"] is False
    assert composite["is_correct"] is False
    assert (details["q3"]["machine_score"], details["q3"]["score"], details["q3"]["final_score"]) == (4, 4, 5)
    assert details["q3"]["reviewed_by"] == "human"
    assert (details["q4"]["machine_score"], details["q4"]["final_score"]) == (6, 6)
    assert float(stored["objective_score"]) == 3
    assert float(stored["subjective_score_machine"]) == 16
    assert float(stored["subjective_score_final"]) == 20
    assert float(stored["total_score"]) == 23
    assert stored["review_status"] == "pending"


def test_regrade_submission_awaits_grader_and_persists_complete_result(monkeypatch):
    from backend import review_service
    from backend.grader import GradingResult

    submission = {
        "id": 7,
        "paper_id": "mechanical",
        "answers_json": json.dumps({"q1": "A", "q2": {"s1": {"answer": "old"}}}),
        "grading_detail_json": json.dumps([
            {
                "question_id": "q2",
                "type": "composite",
                "final_score": 7,
                "manually_reviewed": True,
                "review_status": "reviewed",
                "reviewer_note": "人工确认",
            }
        ]),
    }
    graded = GradingResult(
        objective_score=3,
        subjective_score_machine=4,
        subjective_score_final=4,
        total_score=7,
        review_status="pending",
        grading_detail=[
            {
                "question_id": "q1",
                "type": "single_choice",
                "machine_score": 3,
                "final_score": 3,
                "review_status": "reviewed",
            },
            {
                "question_id": "q2",
                "type": "composite",
                "machine_score": 4,
                "final_score": 4,
                "review_status": "need_review",
            },
        ],
    )
    calls = []
    saved = []

    async def fake_grade_submission(answers, paper_id=None):
        calls.append((answers, paper_id))
        return graded

    monkeypatch.setattr(review_service.database, "get_submission", lambda submission_id: submission)
    monkeypatch.setattr(review_service.database, "save_grading_result", lambda submission_id, result: saved.append((submission_id, result)))
    monkeypatch.setattr(review_service, "grade_submission", fake_grade_submission)

    result = review_service.regrade_submission(7)

    assert calls == [({"q1": "A", "q2": {"s1": {"answer": "old"}}}, "mechanical")]
    assert result == {"success": True, "total_score": 10.0, "review_status": "reviewed"}
    assert saved == [(7, {
        "objective_score": 3.0,
        "subjective_score_machine": 4.0,
        "subjective_score_final": 7.0,
        "total_score": 10.0,
        "review_status": "reviewed",
        "grading_detail": [
            {
                "question_id": "q1",
                "type": "single_choice",
                "machine_score": 3,
                "final_score": 3,
                "review_status": "reviewed",
            },
            {
                "question_id": "q2",
                "type": "composite",
                "machine_score": 4,
                "final_score": 7,
                "review_status": "reviewed",
                "low_confidence": False,
                "need_manual_review": False,
                "manually_reviewed": True,
                "reviewer_note": "人工确认",
            },
        ],
    })]
