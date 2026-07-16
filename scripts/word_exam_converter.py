from __future__ import annotations

import re
from dataclasses import dataclass


OFFICECLI_PATH_RE = re.compile(r"^\s*\[/[^\n]*\]\]\s*")
SCORE_RE = re.compile(r"每(?:题|空)\s*([0-9]+(?:\.[0-9]+)?)\s*分")
OPTION_MARKER_RE = re.compile(r"(?:(?<=\s)|^)([A-H])\s*[.．、]\s*")


@dataclass(frozen=True)
class SectionSpec:
    question_type: str
    default_score: float | None = None


@dataclass(frozen=True)
class ParseIssue:
    code: str
    message: str
    source_line: str = ""


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
