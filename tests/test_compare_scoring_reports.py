import pytest

from scripts.compare_scoring_reports import (
    compare_reports,
    compare_versions,
    render_markdown,
    render_versions_markdown,
)


def _report(*, ordering: float, submissions: list[dict], records: list[dict]):
    return {
        "metrics": {
            "workflow_success_rate": 1.0,
            "scoring_error_count": 0,
            "ordering": {"accuracy": ordering},
        },
        "submissions": submissions,
        "records": records,
    }


def test_compare_reports_calculates_specialized_deltas_and_largest_changes():
    baseline = _report(
        ordering=0.8,
        submissions=[
            {"paper_id": "text-scoring-specialist", "quality": "complete", "total_score": 60},
            {"paper_id": "text-scoring-specialist", "quality": "wrong", "total_score": 20},
        ],
        records=[
            {"paper_id": "text-scoring-specialist", "quality": "complete", "question_id": "q1", "actual_score": 12},
            {"paper_id": "text-scoring-specialist", "quality": "wrong", "question_id": "q1", "actual_score": 4},
        ],
    )
    candidate = _report(
        ordering=0.9,
        submissions=[
            {"paper_id": "text-scoring-specialist", "quality": "complete", "total_score": 70},
            {"paper_id": "text-scoring-specialist", "quality": "wrong", "total_score": 5},
        ],
        records=[
            {"paper_id": "text-scoring-specialist", "quality": "complete", "question_id": "q1", "actual_score": 16},
            {"paper_id": "text-scoring-specialist", "quality": "wrong", "question_id": "q1", "actual_score": 1},
        ],
    )

    result = compare_reports(baseline, candidate)

    assert result["metric_deltas"]["ordering_accuracy"] == 0.1
    assert result["submission_deltas"][0] == {
        "paper_id": "text-scoring-specialist",
        "quality": "complete",
        "baseline": 60.0,
        "candidate": 70.0,
        "delta": 10.0,
    }
    assert result["largest_question_changes"][0]["delta"] == 4.0
    assert "text-scoring-specialist" in render_markdown(result)


def test_compare_versions_builds_three_version_totals_and_ordering_status():
    reports = {
        "v0.1.2": _report(
            ordering=0.8,
            submissions=[
                {"paper_id": "text-scoring-specialist", "quality": "complete", "total_score": 60},
                {"paper_id": "text-scoring-specialist", "quality": "partial", "total_score": 40},
                {"paper_id": "text-scoring-specialist", "quality": "wrong", "total_score": 20},
            ],
            records=[
                {"paper_id": "text-scoring-specialist", "quality": "complete", "question_id": "q1", "actual_score": 12},
                {"paper_id": "text-scoring-specialist", "quality": "partial", "question_id": "q1", "actual_score": 8},
                {"paper_id": "text-scoring-specialist", "quality": "wrong", "question_id": "q1", "actual_score": 4},
            ],
        ),
        "v0.1.3": _report(
            ordering=0.9,
            submissions=[
                {"paper_id": "text-scoring-specialist", "quality": "complete", "total_score": 50},
                {"paper_id": "text-scoring-specialist", "quality": "partial", "total_score": 30},
                {"paper_id": "text-scoring-specialist", "quality": "wrong", "total_score": 10},
            ],
            records=[
                {"paper_id": "text-scoring-specialist", "quality": "complete", "question_id": "q1", "actual_score": 10},
                {"paper_id": "text-scoring-specialist", "quality": "partial", "question_id": "q1", "actual_score": 6},
                {"paper_id": "text-scoring-specialist", "quality": "wrong", "question_id": "q1", "actual_score": 2},
            ],
        ),
        "v0.1.4": _report(
            ordering=1.0,
            submissions=[
                {"paper_id": "text-scoring-specialist", "quality": "complete", "total_score": 80},
                {"paper_id": "text-scoring-specialist", "quality": "partial", "total_score": 45},
                {"paper_id": "text-scoring-specialist", "quality": "wrong", "total_score": 5},
            ],
            records=[
                {"paper_id": "text-scoring-specialist", "quality": "complete", "question_id": "q1", "actual_score": 16},
                {"paper_id": "text-scoring-specialist", "quality": "partial", "question_id": "q1", "actual_score": 9},
                {"paper_id": "text-scoring-specialist", "quality": "wrong", "question_id": "q1", "actual_score": 1},
            ],
        ),
    }

    result = compare_versions(reports)

    assert result["versions"] == ["v0.1.2", "v0.1.3", "v0.1.4"]
    assert result["totals"][0]["scores"]["v0.1.4"] == 80.0
    assert result["ordering_status"]["v0.1.4"]["text-scoring-specialist"] is True
    assert result["largest_latest_changes"][0]["delta"] == 6.0
    assert "v0.1.2 → v0.1.3 → v0.1.4" in render_versions_markdown(result)


def test_compare_versions_rejects_missing_common_keys():
    report = _report(
        ordering=1.0,
        submissions=[
            {"paper_id": "text-scoring-specialist", "quality": "complete", "total_score": 80}
        ],
        records=[
            {"paper_id": "text-scoring-specialist", "quality": "complete", "question_id": "q1", "actual_score": 16}
        ],
    )
    missing = _report(ordering=1.0, submissions=[], records=[])

    with pytest.raises(ValueError, match="keys differ"):
        compare_versions({"v0.1.2": report, "v0.1.3": report, "v0.1.4": missing})
