from __future__ import annotations

import json
from pathlib import Path

from backend.question_loader import validate_questions


ROOT = Path(__file__).resolve().parents[1]
PAPERS_DIR = ROOT / "data" / "papers"
SPECIALIZED_PAPERS = {
    "text-scoring-specialist": ("text", None),
    "sql-scoring-specialist": ("sql", "sql"),
    "code-scoring-specialist": ("code", "python"),
}


def test_specialized_papers_are_valid_and_mode_specific():
    for slug, (mode, language) in SPECIALIZED_PAPERS.items():
        data = json.loads((PAPERS_DIR / f"{slug}.json").read_text(encoding="utf-8"))

        assert data["paper_id"] == slug
        assert data["exam_info"]["total_score"] == 100
        assert data["exam_info"]["passing_score"] == 60
        assert len(data["questions"]) == 5
        assert sum(question["score"] for question in data["questions"]) == 100
        assert {question["type"] for question in data["questions"]} == {
            "short_answer"
        }
        assert {question["score"] for question in data["questions"]} == {20}
        assert {question["scoring_mode"] for question in data["questions"]} == {
            mode
        }
        if language:
            assert {
                question["code_language"] for question in data["questions"]
            } == {language}
        validate_questions(data)


def test_specialized_papers_are_registered_closed():
    index = json.loads((PAPERS_DIR / "index.json").read_text(encoding="utf-8"))
    entries = {item["slug"]: item for item in index["papers"]}

    for slug in SPECIALIZED_PAPERS:
        assert entries[slug]["status"] == "closed"
        assert entries[slug]["question_count"] == 5
        assert entries[slug]["total_score"] == 100
