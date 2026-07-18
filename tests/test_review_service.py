import json


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
                "manually_reviewed": True,
                "reviewer_note": "人工确认",
            },
        ],
    })]
