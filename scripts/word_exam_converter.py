from __future__ import annotations

import argparse
import json
import re
import subprocess
import tempfile
from collections import Counter
from dataclasses import asdict
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Callable


OFFICECLI_PATH_RE = re.compile(r"^\s*\[/[^\n]*\]\]\s*")
SCORE_RE = re.compile(r"每(?:题|空)\s*([0-9]+(?:\.[0-9]+)?)\s*分")
OPTION_MARKER_RE = re.compile(r"(?:(?<=\s)|^)([A-H])\s*[.．、]\s*")
QUESTION_MARKER_RE = re.compile(r"^\s*(\d+)\s*[.．、]\s*(.*)$")
PAREN_CONTENT_RE = re.compile(r"[（(]\s*([^（）()]*)\s*[）)]")
QUESTION_SCORE_RE = re.compile(
    r"[（(]\s*([0-9]+(?:\.[0-9]+)?)\s*分\s*[）)]"
)
ANSWER_PREFIX_RE = re.compile(r"^(?:答(?:案)?|解)\s*[:：]\s*")


@dataclass(frozen=True)
class SectionSpec:
    question_type: str
    default_score: float | None = None


@dataclass(frozen=True)
class ParseIssue:
    code: str
    message: str
    source_line: str = ""


@dataclass(frozen=True)
class ParseResult:
    paper: dict[str, Any]
    issues: list[ParseIssue]


SECTION_ALIASES = (
    ("多项选择", "multiple_choice"),
    ("多选", "multiple_choice"),
    ("单项选择", "single_choice"),
    ("单选", "single_choice"),
    ("判断", "true_false"),
    ("简答", "short_answer"),
    ("简单", "short_answer"),
    ("论述", "essay"),
    ("计算", "essay"),
    ("综合", "essay"),
)


def normalize_extracted_lines(text: str) -> list[str]:
    lines: list[str] = []
    for raw_line in text.splitlines():
        line = OFFICECLI_PATH_RE.sub("", raw_line)
        line = line.replace("\u00a0", " ").strip()
        if line:
            lines.append(line)
    return lines


def parse_section_header(line: str) -> SectionSpec | None:
    compact = re.sub(r"\s+", "", line)
    question_type = next(
        (mapped for label, mapped in SECTION_ALIASES if label in compact),
        None,
    )
    if question_type is None or "题" not in compact:
        return None
    score_match = SCORE_RE.search(compact)
    default_score = float(score_match.group(1)) if score_match else None
    return SectionSpec(question_type=question_type, default_score=default_score)


def parse_answer_token(
    token: str,
    question_type: str,
) -> str | list[str] | bool | None:
    normalized = token.strip().upper()
    if question_type == "true_false":
        if normalized in {"√", "正确", "对", "TRUE", "T"}:
            return True
        if normalized in {"×", "X", "错误", "错", "FALSE", "F"}:
            return False
        return None

    letters = re.findall(r"[A-H]", normalized)
    unique_letters = list(dict.fromkeys(letters))
    if question_type == "single_choice":
        return unique_letters[0] if len(unique_letters) == 1 else None
    if question_type == "multiple_choice":
        return unique_letters or None
    return normalized or None


def split_options(text: str) -> tuple[str, list[dict[str, str]]]:
    matches = list(OPTION_MARKER_RE.finditer(text))
    if not matches:
        return text.strip(), []

    stem = text[: matches[0].start()].strip()
    options: list[dict[str, str]] = []
    for index, match in enumerate(matches):
        end = matches[index + 1].start() if index + 1 < len(matches) else len(text)
        options.append(
            {
                "key": match.group(1),
                "text": text[match.end() : end].strip(),
            }
        )
    return stem, options


def _source_name(source_name: str) -> str:
    stem = Path(source_name).stem
    match = re.search(r"[（(]([^（）()]+)[）)]", stem)
    if match:
        return match.group(1).strip()
    return stem.removeprefix("试卷").strip("-_ ") or stem


def _extract_score(text: str, default_score: float | None) -> tuple[str, float | None]:
    match = QUESTION_SCORE_RE.search(text)
    if not match:
        return text, default_score
    score = float(match.group(1))
    return QUESTION_SCORE_RE.sub("", text, count=1).strip(), score


def _extract_objective_answer(
    text: str,
    question_type: str,
) -> tuple[str, str | list[str] | bool | None]:
    for match in PAREN_CONTENT_RE.finditer(text):
        answer = parse_answer_token(match.group(1), question_type)
        if answer is not None:
            cleaned = f"{text[:match.start()]}{text[match.end():]}".strip()
            return cleaned, answer
    return text, None


def parse_exam_lines(source_name: str, lines: list[str]) -> ParseResult:
    clean_lines = [line.strip() for line in lines if line.strip()]
    title = clean_lines[0] if clean_lines else Path(source_name).stem
    name = _source_name(source_name)
    issues: list[ParseIssue] = []
    questions: list[dict[str, Any]] = []
    section: SectionSpec | None = None
    current: dict[str, Any] | None = None
    collecting_answer = False

    def finalize_current() -> None:
        nonlocal current, collecting_answer
        if current is None:
            return
        current["question"] = "\n".join(current.pop("question_parts", [])).strip()
        answer_parts = current.pop("answer_parts", [])
        if answer_parts:
            current["answer"] = "\n".join(answer_parts).strip()
        current["id"] = f"q{len(questions) + 1}"

        qtype = str(current["type"])
        if not current["question"]:
            issues.append(
                ParseIssue("missing_question", "题目缺少题干", str(current.get("source_line", "")))
            )
        if not isinstance(current.get("score"), (int, float)) or current.get("score", 0) <= 0:
            issues.append(
                ParseIssue("missing_score", "题目缺少有效分值", str(current.get("source_line", "")))
            )
        if qtype in {"single_choice", "multiple_choice"} and not current.get("options"):
            issues.append(
                ParseIssue("missing_options", "选择题缺少选项", str(current.get("source_line", "")))
            )
        answer = current.get("answer")
        if "answer" not in current or answer is None or answer == "" or answer == []:
            issues.append(
                ParseIssue("missing_answer", "题目缺少可确定的答案", str(current.get("source_line", "")))
            )
        current.pop("source_line", None)
        questions.append(current)
        current = None
        collecting_answer = False

    for line in clean_lines[1:]:
        parsed_section = parse_section_header(line)
        if parsed_section is not None:
            finalize_current()
            section = parsed_section
            continue

        question_match = QUESTION_MARKER_RE.match(line)
        if question_match and section is not None:
            finalize_current()
            body = question_match.group(2).strip()
            body, score = _extract_score(body, section.default_score)
            answer: str | list[str] | bool | None = None
            if section.question_type in {
                "true_false",
                "single_choice",
                "multiple_choice",
            }:
                body, answer = _extract_objective_answer(body, section.question_type)
            stem, options = split_options(body)
            current = {
                "type": section.question_type,
                "question_parts": [stem] if stem else [],
                "answer_parts": [],
                "score": score,
                "source_line": line,
            }
            if options:
                current["options"] = options
            if answer is not None:
                current["answer"] = answer
            if section.question_type in {"short_answer", "essay"}:
                current["scoring_mode"] = "text"
            continue

        if current is None:
            continue

        if current["type"] in {"single_choice", "multiple_choice"}:
            stem, options = split_options(line)
            if options:
                if stem:
                    current["question_parts"].append(stem)
                current.setdefault("options", []).extend(options)
            else:
                current["question_parts"].append(line)
            continue

        if current["type"] in {"short_answer", "essay"}:
            if ANSWER_PREFIX_RE.match(line):
                collecting_answer = True
                answer_text = ANSWER_PREFIX_RE.sub("", line, count=1).strip()
                if answer_text:
                    current["answer_parts"].append(answer_text)
            elif collecting_answer:
                current["answer_parts"].append(line)
            else:
                current["question_parts"].append(line)
            continue

        current["question_parts"].append(line)

    finalize_current()
    total_score = sum(
        float(question["score"])
        for question in questions
        if isinstance(question.get("score"), (int, float))
    )
    paper = {
        "paper_id": name,
        "name": name,
        "exam_info": {
            "title": title,
            "description": "由原 Word 试卷转换生成。",
            "total_score": total_score,
            "passing_score": 0,
        },
        "questions": questions,
    }
    return ParseResult(paper=paper, issues=issues)


CommandRunner = Callable[..., Any]
Extractor = Callable[[Path, Path], str]
Validator = Callable[[dict[str, Any]], None]


def extract_with_officecli(
    source: Path,
    temp_dir: Path,
    *,
    runner: CommandRunner = subprocess.run,
) -> str:
    office_source = source
    if source.suffix.lower() == ".doc":
        office_source = temp_dir / f"{source.stem}.docx"
        runner(
            [
                "textutil",
                "-convert",
                "docx",
                "-output",
                str(office_source),
                str(source),
            ],
            check=True,
            capture_output=True,
            text=True,
        )
    result = runner(
        ["officecli", "view", str(office_source), "text"],
        check=True,
        capture_output=True,
        text=True,
    )
    return str(result.stdout)


def _atomic_write_json(path: Path, data: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary_path = path.with_suffix(f"{path.suffix}.tmp")
    temporary_path.write_text(
        json.dumps(data, ensure_ascii=False, indent=2) + "\n",
        encoding="utf-8",
    )
    temporary_path.replace(path)


def _system_validator(paper: dict[str, Any]) -> None:
    from backend.question_loader import validate_questions

    validate_questions(paper)


def _report_entry(source: Path, result: ParseResult, output: Path) -> dict[str, Any]:
    questions = result.paper["questions"]
    return {
        "source": source.name,
        "output": output.name,
        "status": "pending",
        "question_count": len(questions),
        "total_score": result.paper["exam_info"]["total_score"],
        "type_counts": dict(Counter(question["type"] for question in questions)),
        "issues": [asdict(issue) for issue in result.issues],
    }


def convert_directory(
    source_dir: Path,
    output_dir: Path,
    *,
    extractor: Extractor = extract_with_officecli,
    validator: Validator | None = None,
) -> dict[str, Any]:
    validator = validator or _system_validator
    output_dir.mkdir(parents=True, exist_ok=True)
    entries: list[dict[str, Any]] = []
    sources = sorted(
        (
            path
            for path in source_dir.iterdir()
            if path.is_file() and path.suffix.lower() in {".doc", ".docx"}
        ),
        key=lambda path: path.name,
    )

    with tempfile.TemporaryDirectory(prefix="word-exam-converter-") as temp_name:
        temp_dir = Path(temp_name)
        for source in sources:
            output = output_dir / f"{source.stem}.json"
            try:
                extracted = extractor(source, temp_dir)
                result = parse_exam_lines(
                    source.name,
                    normalize_extracted_lines(extracted),
                )
            except Exception as exc:
                entries.append(
                    {
                        "source": source.name,
                        "output": output.name,
                        "status": "extraction_failed",
                        "question_count": 0,
                        "total_score": 0,
                        "type_counts": {},
                        "issues": [str(exc)],
                    }
                )
                continue

            entry = _report_entry(source, result, output)
            if result.issues:
                entry["status"] = "needs_review"
                entries.append(entry)
                continue

            try:
                validator(result.paper)
            except Exception as exc:
                entry["status"] = "invalid"
                entry["issues"] = [str(exc)]
                entries.append(entry)
                continue

            _atomic_write_json(output, result.paper)
            entry["status"] = "valid"
            entries.append(entry)

    report = {
        "source_directory": str(source_dir),
        "output_directory": str(output_dir),
        "file_count": len(entries),
        "files": entries,
    }
    _atomic_write_json(output_dir / "conversion-report.json", report)
    return report


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description="使用 officecli 将 Word 试卷转换为系统 JSON。"
    )
    parser.add_argument("source_dir", type=Path)
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args(argv)
    report = convert_directory(args.source_dir, args.output)
    status_counts = Counter(entry["status"] for entry in report["files"])
    print(
        json.dumps(
            {
                "file_count": report["file_count"],
                "status_counts": dict(status_counts),
                "report": str(args.output / "conversion-report.json"),
            },
            ensure_ascii=False,
        )
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
