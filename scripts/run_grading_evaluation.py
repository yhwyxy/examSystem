from __future__ import annotations

import argparse
import asyncio
import json
from collections import Counter
from pathlib import Path
from typing import Any

from backend.grader import close_subjective_service, grade_question


SUBJECTIVE_TYPES = {"short_answer", "essay"}


def _wrong_objective_answer(question: dict[str, Any]) -> Any:
    qtype = question["type"]
    reference = question.get("answer")
    if qtype == "true_false":
        return not bool(reference)
    keys = [option["key"] for option in question.get("options", [])]
    correct = set(reference if isinstance(reference, list) else [reference])
    wrong = next((key for key in keys if key not in correct), None)
    if qtype == "multiple_choice":
        return [wrong] if wrong else []
    return wrong


def build_simulated_answer(
    question: dict[str, Any], index: int
) -> tuple[Any, float, str]:
    max_score = float(question["score"])
    reference = question.get("answer")
    qtype = question["type"]

    if qtype in SUBJECTIVE_TYPES:
        mode = index % 3
        if mode == 0:
            return reference, max_score, "标准答案"
        if mode == 1:
            text = str(reference or "")
            midpoint = max(1, len(text) // 2)
            return text[:midpoint], max_score * 0.5, "半数内容/关键点"
        return "不了解该题，答案与题意无关。", 0.0, "无关答案"

    mode = index % 5
    if mode in {0, 4}:
        return reference, max_score, "正确答案"
    if mode == 1:
        return _wrong_objective_answer(question), 0.0, "错误答案"
    if mode == 2:
        return None, 0.0, "空答"
    if qtype == "multiple_choice" and isinstance(reference, list) and len(reference) > 1:
        selected = [reference[0]]
        expected = max_score / len(reference)
        return selected, expected, "正确但漏选"
    return reference, max_score, "正确答案"


async def evaluate_paper(path: Path, subjective_delay: float) -> dict[str, Any]:
    paper = json.loads(path.read_text(encoding="utf-8"))
    expected_total = 0.0
    system_total = 0.0
    details: list[dict[str, Any]] = []

    for index, question in enumerate(paper["questions"], start=1):
        answer, expected, scenario = build_simulated_answer(question, index)
        graded = await grade_question(question, answer)
        if question["type"] in SUBJECTIVE_TYPES and subjective_delay > 0:
            await asyncio.sleep(subjective_delay)
        actual = float(graded.get("final_score", 0) or 0)
        expected_total += expected
        system_total += actual
        details.append(
            {
                "question_id": question["id"],
                "type": question["type"],
                "scenario": scenario,
                "max_score": float(question["score"]),
                "expected_score": round(expected, 6),
                "system_score": round(actual, 6),
                "absolute_error": round(abs(actual - expected), 6),
                "review_status": graded.get("review_status"),
                "grading_method": graded.get("grading_method"),
            }
        )

    absolute_error = abs(system_total - expected_total)
    expected_denominator = expected_total if expected_total else 1.0
    return {
        "paper_id": paper["paper_id"],
        "paper_name": paper["name"],
        "question_count": len(paper["questions"]),
        "paper_total_score": float(paper["exam_info"]["total_score"]),
        "expected_score": round(expected_total, 6),
        "system_score": round(system_total, 6),
        "absolute_error": round(absolute_error, 6),
        "error_rate_vs_expected": round(absolute_error / expected_denominator, 8),
        "error_rate_vs_full_score": round(
            absolute_error / float(paper["exam_info"]["total_score"]), 8
        ),
        "review_counts": dict(Counter(d["review_status"] for d in details)),
        "details": details,
    }


async def run(input_dir: Path, subjective_delay: float) -> dict[str, Any]:
    paths = sorted(input_dir.glob("试卷*.json"), key=lambda path: path.name)
    papers = []
    try:
        for path in paths:
            papers.append(await evaluate_paper(path, subjective_delay))
    finally:
        close_subjective_service()

    expected = sum(p["expected_score"] for p in papers)
    actual = sum(p["system_score"] for p in papers)
    full_score = sum(p["paper_total_score"] for p in papers)
    question_errors = [d["absolute_error"] for p in papers for d in p["details"]]
    return {
        "methodology": {
            "objective": "固定序号生成正确、错误、空答和多选漏选答案",
            "subjective": "固定序号生成标准答案、截取前半内容和无关答案",
            "expected_partial_subjective_score": "参考答案前半部分按50%计分",
        },
        "summary": {
            "paper_count": len(papers),
            "question_count": sum(p["question_count"] for p in papers),
            "full_score_total": round(full_score, 6),
            "expected_score_total": round(expected, 6),
            "system_score_total": round(actual, 6),
            "absolute_error_total": round(abs(actual - expected), 6),
            "aggregate_error_rate_vs_expected": round(
                abs(actual - expected) / (expected or 1.0), 8
            ),
            "aggregate_error_rate_vs_full_score": round(
                abs(actual - expected) / (full_score or 1.0), 8
            ),
            "mean_question_absolute_error": round(
                sum(question_errors) / (len(question_errors) or 1), 8
            ),
        },
        "papers": papers,
    }


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("input_dir", type=Path)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--subjective-delay", type=float, default=1.0)
    args = parser.parse_args()
    report = asyncio.run(run(args.input_dir, max(0.0, args.subjective_delay)))
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(
        json.dumps(report, ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
    )
    print(json.dumps(report["summary"], ensure_ascii=False))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
