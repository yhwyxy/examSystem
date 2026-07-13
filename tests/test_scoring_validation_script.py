from __future__ import annotations

from scripts import validate_scoring_system as validation


def test_render_markdown_formats_confidence_without_duplicate_keyword():
    report = {
        "generated_at": "2026-07-12T00:00:00+00:00",
        "remote": {"url": "https://example.test/v1/rerank", "model": "test"},
        "metrics": {
            "planned_submissions": 1,
            "completed_submissions": 1,
            "workflow_success_rate": 1.0,
            "band_hit_rate": 1.0,
            "mean_absolute_error": 0.0,
            "ordering": {"accuracy": 1.0, "failed_checks": []},
            "scoring_error_count": 0,
            "workflow_errors": [],
        },
        "records": [
            {
                "paper_id": "paper",
                "candidate": "candidate",
                "quality": "complete",
                "question_id": "q1",
                "expected_min": 9.0,
                "expected_max": 10.0,
                "actual_score": 9.5,
                "within_band": True,
                "grading_method": "subjective_scoring:text",
                "confidence": 0.95,
            }
        ],
        "isolation": {"production_unchanged": True},
    }

    markdown = validation._render_markdown(report)

    assert "| 0.9500 |" in markdown


def test_submission_polling_waits_long_enough_to_avoid_rate_limit(monkeypatch):
    sleeps: list[float] = []
    resets: list[bool] = []

    class Response:
        def __init__(self, status):
            self._status = status

        def raise_for_status(self):
            pass

        def json(self):
            return {"status": self._status}

    class Client:
        statuses = iter(["grading", "pending"])

        def get(self, path):
            return Response(next(self.statuses))

    monkeypatch.setattr(validation.time, "sleep", sleeps.append)
    monkeypatch.setattr(
        validation,
        "_reset_isolated_rate_limit_state",
        lambda: resets.append(True),
    )
    monkeypatch.setattr(
        validation.review_service,
        "get_submission_detail",
        lambda submission_id: {"id": submission_id},
    )

    assert validation._wait_for_submission(Client(), 1) == {"id": 1}
    assert sleeps and min(sleeps) >= 0.5
    assert len(resets) == 2


def test_specialized_papers_have_three_quality_cases():
    specialized_slugs = {
        "text-scoring-specialist",
        "sql-scoring-specialist",
        "code-scoring-specialist",
    }
    cases = [case for case in validation.CANDIDATES if case.paper_id in specialized_slugs]

    assert {case.paper_id for case in cases} == specialized_slugs
    for slug in specialized_slugs:
        paper_cases = [case for case in cases if case.paper_id == slug]
        assert {case.quality for case in paper_cases} == {"complete", "partial", "wrong"}
        question_ids = {question["id"] for question in validation.PAPERS[slug]["questions"]}
        assert all(set(case.answers) == question_ids for case in paper_cases)


def test_validation_resets_isolated_rate_limit_state():
    validation.main_module._rate_store["testclient"] = [1.0, 2.0]

    validation._reset_isolated_rate_limit_state()

    assert dict(validation.main_module._rate_store) == {}
