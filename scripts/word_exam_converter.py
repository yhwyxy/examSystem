from __future__ import annotations

import argparse
import json
import re
import subprocess
import tempfile
import unicodedata
from collections import Counter
from dataclasses import asdict
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Callable


OFFICECLI_PATH_RE = re.compile(r"^\s*\[/[^\n]*\]\]\s*")
SCORE_RE = re.compile(r"每(?:小?题|空)\s*([0-9]+(?:\.[0-9]+)?)\s*分")
SCORE_FORMULA_RE = re.compile(
    r"[0-9]+\s*[×*X]\s*([0-9]+(?:\.[0-9]+)?)\s*=\s*[0-9]+(?:\.[0-9]+)?\s*分"
)
SECTION_COUNT_RE = re.compile(r"共\s*([0-9]+)\s*(?:小?题|题)")
SECTION_TOTAL_RE = re.compile(r"共(?:计)?\s*([0-9]+(?:\.[0-9]+)?)\s*分")
OPTION_MARKER_RE = re.compile(
    r"(?<![A-Z0-9(])([A-H])\s*[.:、]\s*"
    r"|(?:(?<=\s)|^)([A-H])(?:\s+|(?=[\u3400-\u9fff]))"
)
OPTION_LINE_MARKER_RE = re.compile(
    r"(?<![A-Z0-9(])([A-H])(?:\s*[.:、]\s*|\s+|(?=[\u3400-\u9fff]))"
)
OPTION_LINE_START_RE = re.compile(
    r"^\s*[A-H](?:\s*[.:、]\s*|\s+|(?=[\u3400-\u9fff]))"
)
OPTION_PUNCTUATED_RE = re.compile(r"(?<![A-Z0-9(])[A-H]\s*[.:、]")
QUESTION_MARKER_RE = re.compile(
    r"^\s*(\d{1,2})(?![年月日])\s*(?:、|[.](?!\d)|(?=[“\"\u3400-\u9fff]))\s*(.*)$"
)
PAREN_CONTENT_RE = re.compile(r"[（(]\s*([^（）()]*)\s*[）)]")
QUESTION_SCORE_RE = re.compile(
    r"[（(]\s*([0-9]+(?:\.[0-9]+)?)\s*分\s*[）)]"
)
ANSWER_PREFIX_RE = re.compile(r"^(?:答(?:案)?|解)\s*[:：]\s*")
STANDARD_ANSWER_RE = re.compile(r"^(?:标准答案|答案)\s*[:：]\s*(.+)$")
COMPACT_ANSWER_PAIR_RE = re.compile(r"(\d+)\s*、\s*([A-H]+)")
REFERENCE_ANSWER_HEADER_RE = re.compile(r"^参考答案\s*[:：]?\s*$")
EMBEDDED_ANSWER_RE = re.compile(
    r"\s{2,}([A-H](?:\s*[、,，]?\s*[A-H])*)\s*(?:[。.]|$)"
)
MIDDLE_EMBEDDED_ANSWER_RE = re.compile(
    r"\s{2,}([A-H](?:\s*[、,，]?\s*[A-H])*)\s{2,}"
)
TRAILING_TRUE_FALSE_RE = re.compile(r"\s*([√×Xⅹ])\s*$", re.IGNORECASE)

SOURCE_LINE_CORRECTIONS = {
    "试卷（能源与动力工程）.docx": {
        "A、焦耳;B、兆帕;B、瓦特;B、牛顿。":
            "A、焦耳;B、兆帕;C、瓦特;D、牛顿。",
    },
    "试卷（通信工程）.doc": {
        "4、关于IP地址划分,根据表格中的IP地址规律,请完善如下表格。":
            "4、关于IP地址划分,根据表格中的IP地址规律,请完善如下表格。(10分)",
    },
}

SOFTWARE_ANSWER_OVERRIDES = {
    16: """JavaScript：
for (var i = 0; i < 10; i++) {
    if (i == 5) {
        break;
    }
    // ……
    for (var j = 0; j < 10; j++) {
        if (j < 9) {
            continue;
        }
        // ……
    }
}""",
    17: """A、抽象：忽略与当前目标无关的方面，以便更充分地关注与当前目标有关的方面。
B、继承：一种联结类的层次模型，允许和鼓励重用，提供明确表述共性的方法。
C、封装：把过程和数据包围起来，对数据的访问只能通过已定义的界面。
D、多态：允许不同类的对象对同一消息作出响应。""",
    18: """1、HTTP（超文本传输协议）用于在 Web 浏览器和网站服务器之间传递信息。
2、HTTPS 采用安全套接字层，使传输数据更加安全。
3、HTTPS 在 HTTP 的基础上增加 SSL/TLS 协议。
4、使用 HTTPS 协议需要 CA 证书。""",
    19: "观察法、体验法、问卷调查法、访谈法、单据分析法、报表分析法等。",
    20: (
        "SELECT emp_id, emp_name, dept_name FROM emp, dept "
        "WHERE salary > 3000 AND emp.dept_id = dept.dept_id;"
    ),
    21: """JavaScript：
function findIndex(array, item) {
    return array.indexOf(item);
}""",
}


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
    ("中译英", "short_answer"),
    ("不定项选择", "multiple_choice"),
    ("多项选择", "multiple_choice"),
    ("多选", "multiple_choice"),
    ("单项选择", "single_choice"),
    ("单选", "single_choice"),
    ("选择", "multiple_choice"),
    ("判断", "true_false"),
    ("填空", "short_answer"),
    ("问答", "short_answer"),
    ("简答", "short_answer"),
    ("简单", "short_answer"),
    ("程序", "essay"),
    ("案例", "essay"),
    ("论述", "essay"),
    ("计算", "essay"),
    ("综合", "essay"),
)


def normalize_extracted_lines(text: str) -> list[str]:
    lines: list[str] = []
    for raw_line in text.splitlines():
        line = OFFICECLI_PATH_RE.sub("", raw_line)
        line = unicodedata.normalize("NFKC", line).replace("\u00a0", " ").strip()
        if not line:
            continue
        if re.match(r"^[A-H]\s*[.、]", line):
            lines.extend(part.strip() for part in re.split(r"\s+(?=\d{1,2}、)", line))
        else:
            lines.append(line)
    return lines


def parse_section_header(line: str) -> SectionSpec | None:
    compact = re.sub(r"\s+", "", line)
    question_type = next(
        (mapped for label, mapped in SECTION_ALIASES if label in compact),
        None,
    )
    if question_type is None or ("题" not in compact and "案例" not in compact):
        return None
    score_match = SCORE_RE.search(compact)
    formula_match = SCORE_FORMULA_RE.search(compact)
    if score_match:
        default_score = float(score_match.group(1))
    elif formula_match:
        default_score = float(formula_match.group(1))
    else:
        count_match = SECTION_COUNT_RE.search(compact)
        total_match = SECTION_TOTAL_RE.search(compact)
        if count_match and total_match:
            default_score = float(total_match.group(1)) / int(count_match.group(1))
        else:
            default_score = None
    return SectionSpec(question_type=question_type, default_score=default_score)


def parse_answer_token(
    token: str,
    question_type: str,
) -> str | list[str] | bool | None:
    normalized = unicodedata.normalize("NFKC", token).strip().upper()
    if question_type == "true_false":
        if normalized in {"√", "正确", "对", "TRUE", "T"}:
            return True
        if normalized in {"×", "X", "错误", "错", "FALSE", "F"}:
            return False
        return None

    compact = re.sub(r"[\s、,，;；/]+", "", normalized)
    if not re.fullmatch(r"[A-H]+", compact):
        return None
    unique_letters = list(dict.fromkeys(compact))
    if question_type == "single_choice":
        return unique_letters[0] if len(unique_letters) == 1 else None
    if question_type == "multiple_choice":
        return unique_letters or None
    return normalized or None


def split_options(text: str) -> tuple[str, list[dict[str, str]]]:
    text = unicodedata.normalize("NFKC", text)
    parenthetical_spans = [match.span() for match in PAREN_CONTENT_RE.finditer(text)]
    use_broad_option_line = (
        OPTION_LINE_START_RE.match(text) is not None
        and OPTION_PUNCTUATED_RE.search(text) is None
    )
    marker_re = OPTION_LINE_MARKER_RE if use_broad_option_line else OPTION_MARKER_RE
    matches = [
        match
        for match in marker_re.finditer(text)
        if not any(start < match.start() < end for start, end in parenthetical_spans)
    ]
    if not matches:
        return text.strip(), []

    stem = text[: matches[0].start()].strip()
    options: list[dict[str, str]] = []
    for index, match in enumerate(matches):
        end = matches[index + 1].start() if index + 1 < len(matches) else len(text)
        options.append(
            {
                "key": match.group(1)
                or (match.group(2) if match.re.groups > 1 else None),
                "text": text[match.end() : end].strip(),
            }
        )
    return stem, options


def apply_source_corrections(source_name: str, lines: list[str]) -> list[str]:
    corrections = SOURCE_LINE_CORRECTIONS.get(source_name, {})
    corrected = [corrections.get(line, line) for line in lines]
    if source_name != "试卷（软件开发）.docx":
        return corrected

    software_answers = {
        8: "C",
        9: "A",
        10: "ABCD",
        11: "A",
        12: "B",
        13: "A",
        14: "A",
        15: "ABC",
    }
    result: list[str] = []
    for line in corrected:
        if re.fullmatch(r"\d{1,2}、[A-H]+", line) or line == "1.":
            continue
        if line.startswith("以下哪些加密算法是不可逆的:"):
            line = re.sub(r"\s*1、2、ABC$", "", line)
            line = f"1.{line}"
        question_match = re.match(r"^(\d{1,2})[.]", line)
        if question_match:
            number = int(question_match.group(1))
            answer = software_answers.get(number)
            if answer and parse_answer_token(answer, "multiple_choice") is not None:
                line = re.sub(r"[（(]\s*[）)]", f"({answer})", line, count=1)
        if line == "1.下面有两张表:":
            line = "1.下面有两张表:(15分)"
        elif line == "2.找出元素item在给定数组array中的位置":
            line = f"{line}(15分)"
        elif line == "[Table: 5 rows]":
            line = (
                "EMP表数据：\n"
                "EMP_ID | EMP_NAME | DEPT_ID | SALARY\n"
                "10022222 | 张三 | 001 | 1000\n"
                "10022796 | 李四 | 002 | 5500\n"
                "10022696 | 王五 | 002 | 3200\n"
                "10022101 | 赵六 | 003 | 2000"
            )
        elif line == "[Table: 4 rows]":
            line = (
                "DEPT表数据：\n"
                "DEPT_ID | DEPT_NAME\n"
                "001 | IT部\n"
                "002 | 财务部\n"
                "003 | 公司办"
            )
        result.append(line)
    return result


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
    if question_type == "true_false":
        trailing = TRAILING_TRUE_FALSE_RE.search(text)
        if trailing:
            answer = parse_answer_token(trailing.group(1), question_type)
            if isinstance(answer, bool):
                return text[: trailing.start()].strip(), answer
    for match in PAREN_CONTENT_RE.finditer(text):
        answer = parse_answer_token(match.group(1), question_type)
        if answer is not None:
            cleaned = f"{text[:match.start()]}{text[match.end():]}".strip()
            return cleaned, answer
    middle = MIDDLE_EMBEDDED_ANSWER_RE.search(text)
    if middle:
        answer = parse_answer_token(middle.group(1), question_type)
        if answer is not None:
            cleaned = f"{text[:middle.start()]} {text[middle.end():]}".strip()
            return cleaned, answer
    embedded = EMBEDDED_ANSWER_RE.search(text)
    if embedded:
        answer = parse_answer_token(embedded.group(1), question_type)
        if answer is not None:
            cleaned = f"{text[:embedded.start()]}{text[embedded.end():]}".strip()
            return cleaned, answer
    return text, None


def _collect_question_issues(
    question: dict[str, Any],
    issues: list[ParseIssue],
) -> None:
    source_line = str(question.get("_source_line", ""))
    qtype = str(question["type"])
    if not question.get("question"):
        issues.append(ParseIssue("missing_question", "题目缺少题干", source_line))
    if not isinstance(question.get("score"), (int, float)) or question.get("score", 0) <= 0:
        issues.append(ParseIssue("missing_score", "题目缺少有效分值", source_line))
    if qtype in {"single_choice", "multiple_choice"} and not question.get("options"):
        issues.append(ParseIssue("missing_options", "选择题缺少选项", source_line))
    answer = question.get("answer")
    if "answer" not in question or answer is None or answer == "" or answer == []:
        issues.append(ParseIssue("missing_answer", "题目缺少可确定的答案", source_line))
    if qtype in {"single_choice", "multiple_choice"} and question.get("options"):
        keys = [str(option["key"]) for option in question["options"]]
        duplicate_keys = sorted({key for key in keys if keys.count(key) > 1})
        if duplicate_keys:
            issues.append(
                ParseIssue(
                    "duplicate_option_key",
                    f"选择题选项键重复: {', '.join(duplicate_keys)}",
                    source_line,
                )
            )
        answers = answer if isinstance(answer, list) else [answer]
        invalid_answers = [
            str(item) for item in answers if item is not None and str(item) not in keys
        ]
        if invalid_answers:
            issues.append(
                ParseIssue(
                    "answer_not_in_options",
                    f"答案不在选项中: {', '.join(invalid_answers)}",
                    source_line,
                )
            )


def parse_exam_lines(source_name: str, lines: list[str]) -> ParseResult:
    clean_lines = [line.strip() for line in lines if line.strip()]
    first_section = parse_section_header(clean_lines[0]) if clean_lines else None
    title = (
        Path(source_name).stem
        if first_section is not None
        else (clean_lines[0] if clean_lines else Path(source_name).stem)
    )
    content_lines = clean_lines if first_section is not None else clean_lines[1:]
    title_has_answers = "答案" in title
    name = _source_name(source_name)
    issues: list[ParseIssue] = []
    questions: list[dict[str, Any]] = []
    section: SectionSpec | None = None
    section_questions: list[dict[str, Any]] = []
    section_context: list[str] = []
    current: dict[str, Any] | None = None
    collecting_answer = False
    reference_answer_mode = False
    reference_target: dict[str, Any] | None = None

    def finalize_current() -> None:
        nonlocal current, collecting_answer
        if current is None:
            return
        current["question"] = "\n".join(current.pop("question_parts", [])).strip()
        answer_parts = current.pop("answer_parts", [])
        if answer_parts:
            current["answer"] = "\n".join(answer_parts).strip()
        current["id"] = f"q{len(questions) + 1}"
        questions.append(current)
        current = None
        collecting_answer = False

    for line in content_lines:
        parsed_section = parse_section_header(line)
        if parsed_section is not None:
            finalize_current()
            section = parsed_section
            section_questions = []
            section_context = []
            reference_answer_mode = False
            reference_target = None
            collecting_answer = False
            continue

        if REFERENCE_ANSWER_HEADER_RE.match(line):
            finalize_current()
            reference_answer_mode = True
            reference_target = None
            continue

        standard_answer = STANDARD_ANSWER_RE.match(line)
        if standard_answer:
            compact_answers = COMPACT_ANSWER_PAIR_RE.findall(standard_answer.group(1))
            if compact_answers:
                finalize_current()
                unresolved_questions = [
                    question
                    for question in section_questions
                    if not question.get("answer")
                ]
                for question, (_, answer_token) in zip(
                    unresolved_questions,
                    compact_answers,
                ):
                    parsed_answer = parse_answer_token(
                        answer_token,
                        str(question["type"]),
                    )
                    if parsed_answer is not None:
                        question["answer"] = parsed_answer
                continue

        if reference_answer_mode:
            reference_match = QUESTION_MARKER_RE.match(line)
            if reference_match:
                local_number = int(reference_match.group(1))
                reference_target = next(
                    (
                        question
                        for question in section_questions
                        if question.get("_section_number") == local_number
                    ),
                    None,
                )
                if reference_target is not None:
                    reference_target["answer"] = reference_match.group(2).strip()
            elif reference_target is not None:
                reference_target["answer"] = (
                    f"{reference_target.get('answer', '')}\n{line}".strip()
                )
            continue

        if (
            current is not None
            and current["type"] in {"short_answer", "essay"}
            and collecting_answer
            and re.match(r"^\d{1,2}[.](?!\d)", line)
            and QUESTION_SCORE_RE.search(line) is None
        ):
            current["answer_parts"].append(line)
            continue

        question_match = QUESTION_MARKER_RE.match(line)
        if question_match and section is not None:
            finalize_current()
            body = question_match.group(2).strip()
            has_explicit_score = QUESTION_SCORE_RE.search(body) is not None
            body, score = _extract_score(body, section.default_score)
            question_type = section.question_type
            answer: str | list[str] | bool | None = None
            if question_type in {
                "true_false",
                "single_choice",
                "multiple_choice",
            }:
                body, answer = _extract_objective_answer(body, question_type)
                if (
                    question_type == "multiple_choice"
                    and isinstance(answer, list)
                    and len(answer) == 1
                ):
                    question_type = "single_choice"
                    answer = answer[0]
            elif question_type == "short_answer":
                boolean_body, boolean_answer = _extract_objective_answer(body, "true_false")
                if isinstance(boolean_answer, bool):
                    question_type = "true_false"
                    body = boolean_body
                    answer = boolean_answer
            stem, options = split_options(body)
            question_parts = [stem] if stem else []
            if section_context and question_type == "essay":
                question_parts = [*section_context, *question_parts]
            current = {
                "type": question_type,
                "question_parts": question_parts,
                "answer_parts": [],
                "score": score,
                "_source_line": line,
                "_section_number": int(question_match.group(1)),
                "allow_unlabelled_answer": (
                    question_type == "short_answer"
                    and (
                        title_has_answers
                        or has_explicit_score
                        or stem.rstrip().endswith(("?", "？"))
                    )
                ),
            }
            if options:
                current["options"] = options
            if answer is not None:
                current["answer"] = answer
            if question_type in {"short_answer", "essay"}:
                current["scoring_mode"] = "text"
            section_questions.append(current)
            continue

        if current is None:
            if section is not None and section.question_type == "essay":
                if line.rstrip(":：") not in {"问题", "问题如下"}:
                    section_context.append(line)
            continue

        if current["type"] in {"true_false", "single_choice", "multiple_choice"}:
            standard_answer = STANDARD_ANSWER_RE.match(line)
            if standard_answer:
                parsed_answer = parse_answer_token(
                    standard_answer.group(1),
                    str(current["type"]),
                )
                if parsed_answer is not None:
                    current["answer"] = parsed_answer
                    continue
            cleaned_line, continued_answer = _extract_objective_answer(
                line,
                str(current["type"]),
            )
            if continued_answer is not None and "answer" not in current:
                current["answer"] = continued_answer
                line = cleaned_line
                if not line:
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
            elif current.get("allow_unlabelled_answer"):
                collecting_answer = True
                current["answer_parts"].append(line)
            else:
                current["question_parts"].append(line)
            continue

        current["question_parts"].append(line)

    finalize_current()
    if source_name == "试卷（软件开发）.docx":
        for number, answer in SOFTWARE_ANSWER_OVERRIDES.items():
            if number <= len(questions):
                questions[number - 1]["answer"] = answer
    for question in questions:
        _collect_question_issues(question, issues)
        question.pop("_source_line", None)
        question.pop("_section_number", None)
        question.pop("allow_unlabelled_answer", None)
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
                    apply_source_corrections(
                        source.name,
                        normalize_extracted_lines(extracted),
                    ),
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
