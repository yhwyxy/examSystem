import json
from pathlib import Path
import sys

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from scripts.migrate_composite_questions import migrate_all


def test_migration_targets_and_is_idempotent(tmp_path):
    root = Path("data/papers")
    report = migrate_all(root, report_path=tmp_path / "report.json")
    assert report["migrated"]
    expected = {
        "mechanical": {"q41", "q44"}, "materials": {"q42", "q43"},
        "instrumentation": {"q41", "q42", "q43"},
        "chemical-analysis": {"q42", "q43"}, "chemical-engineering": {"q42", "q44"},
        "metal-materials": {"q43"}, "legal": {"q35"},
    }
    for slug, ids in expected.items():
        data = json.loads((root / f"{slug}.json").read_text())
        questions = {q["id"]: q for q in data["questions"]}
        for qid in ids:
            assert questions[qid]["type"] == "composite"
            assert questions[qid]["subquestions"]
            assert sum(s["score"] for s in questions[qid]["subquestions"]) == questions[qid]["score"]
    legal = json.loads((root / "legal.json").read_text())
    ids = [q["id"] for q in legal["questions"]]
    assert all(x not in ids for x in ("q36", "q37", "q38", "q39"))
    before = (root / "mechanical.json").read_text()
    migrate_all(root, report_path=tmp_path / "report2.json")
    assert (root / "mechanical.json").read_text() == before
