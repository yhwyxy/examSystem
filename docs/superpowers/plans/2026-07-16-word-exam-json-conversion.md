# Word Exam JSON Conversion Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Convert all 18 Word exam files in `docs/测试用试卷--含答案/` into validated exam JSON files under `docs/测试用试卷--含答案/json/`.

**Architecture:** Add a standalone Python converter with three boundaries: officecli/textutil extraction, deterministic paragraph parsing, and system-schema validation/output. Parsing is a state machine driven by section headers and question markers; uncertain data is recorded in a report instead of invented.

**Tech Stack:** Python 3.12, pytest, officecli 1.0.136, macOS textutil, existing `backend.question_loader.validate_questions`.

## Global Constraints

- Do not modify source Word files.
- Write deliverables only under `docs/测试用试卷--含答案/json/`.
- Do not import generated papers into `data/papers/`.
- Use officecli for all Word content extraction; `.doc` files may be converted to temporary `.docx` files with textutil first.
- Never fabricate a missing answer, question type, or score.
- A paper is `valid` only after `validate_questions()` returns successfully.

---

### Task 1: Parsing primitives

**Files:**
- Create: `scripts/word_exam_converter.py`
- Create: `tests/test_word_exam_converter.py`

**Interfaces:**
- Produces: `normalize_extracted_lines(text: str) -> list[str]`
- Produces: `parse_section_header(line: str) -> SectionSpec | None`
- Produces: `parse_answer_token(token: str, question_type: str) -> str | list[str] | bool | None`
- Produces: `split_options(text: str) -> tuple[str, list[dict[str, str]]]`

- [ ] **Step 1: Write failing primitive tests**

```python
from scripts.word_exam_converter import (
    normalize_extracted_lines,
    parse_answer_token,
    parse_section_header,
    split_options,
)


def test_normalize_extracted_lines_removes_officecli_paths():
    text = "[/body/p[1]] 标题\n[/body/p[2]] 1、题目"
    assert normalize_extracted_lines(text) == ["标题", "1、题目"]


def test_parse_section_header_reads_type_and_score():
    section = parse_section_header("二、单项选择题（每题1.5分，共30分）")
    assert section.question_type == "single_choice"
    assert section.default_score == 1.5


def test_parse_answer_token_normalizes_objective_answers():
    assert parse_answer_token("√", "true_false") is True
    assert parse_answer_token("×", "true_false") is False
    assert parse_answer_token(" A、C ", "multiple_choice") == ["A", "C"]


def test_split_options_handles_inline_options():
    stem, options = split_options("温度如何变化 A.升高 B.降低 C.不变 D.改变")
    assert stem == "温度如何变化"
    assert options == [
        {"key": "A", "text": "升高"},
        {"key": "B", "text": "降低"},
        {"key": "C", "text": "不变"},
        {"key": "D", "text": "改变"},
    ]
```

- [ ] **Step 2: Run tests and verify RED**

Run: `env PYTHONPATH=. uv run --extra dev pytest tests/test_word_exam_converter.py -q`

Expected: collection fails because `scripts.word_exam_converter` does not exist.

- [ ] **Step 3: Implement parsing primitives**

Create immutable dataclasses `SectionSpec` and `ParseIssue`; implement path-prefix removal, whitespace normalization, section aliases, numeric score parsing, answer normalization, and option splitting. Use compiled regular expressions and return `None` for ambiguous answer tokens.

- [ ] **Step 4: Run primitive tests and verify GREEN**

Run: `env PYTHONPATH=. uv run --extra dev pytest tests/test_word_exam_converter.py -q`

Expected: 4 tests pass.

- [ ] **Step 5: Commit primitives**

```bash
git add scripts/word_exam_converter.py tests/test_word_exam_converter.py
git commit -m "feat: add Word exam parsing primitives"
```

---

### Task 2: Exam paragraph state machine

**Files:**
- Modify: `scripts/word_exam_converter.py`
- Modify: `tests/test_word_exam_converter.py`

**Interfaces:**
- Consumes: Task 1 parsing primitives.
- Produces: `parse_exam_lines(source_name: str, lines: list[str]) -> ParseResult`
- `ParseResult.paper` contains `paper_id`, `name`, `exam_info`, and `questions`.
- `ParseResult.issues` contains structured warnings that determine `needs_review`.

- [ ] **Step 1: Write failing state-machine tests**

Add representative tests for:

```python
def test_parse_exam_lines_builds_true_false_and_choice_questions():
    lines = [
        "化工专业考试试卷",
        "一、判断题（每题1分，共2分）",
        "1、理想气体混合物是一种理想溶液。（√）",
        "2、催化剂能使不可能反应发生。（×）",
        "二、单项选择题（每题2分，共2分）",
        "1、吸热反应升温是否有利（A） A.有利 B.不利 C.无关 D.不确定",
    ]
    result = parse_exam_lines("试卷（化工）.docx", lines)
    assert [q["type"] for q in result.paper["questions"]] == [
        "true_false", "true_false", "single_choice"
    ]
    assert [q["answer"] for q in result.paper["questions"]] == [True, False, "A"]
    assert result.paper["exam_info"]["total_score"] == 4


def test_parse_exam_lines_collects_multiline_subjective_answer():
    lines = [
        "专业试卷",
        "四、简答题（每题10分，共10分）",
        "1、影响反应速率的因素有哪些？（10分）",
        "答：温度、浓度和催化剂。",
        "提高温度或加入正催化剂可加速反应。",
    ]
    result = parse_exam_lines("试卷（化工）.docx", lines)
    question = result.paper["questions"][0]
    assert question["type"] == "short_answer"
    assert question["answer"] == "温度、浓度和催化剂。\n提高温度或加入正催化剂可加速反应。"


def test_parse_exam_lines_marks_missing_answer_for_review():
    result = parse_exam_lines(
        "试卷（软件开发）.docx",
        ["软件开发试卷", "一、单选题（每题2分）", "1、HTTP 默认端口是（ ） A.80 B.443"],
    )
    assert result.issues[0].code == "missing_answer"
```

- [ ] **Step 2: Run tests and verify RED**

Run: `env PYTHONPATH=. uv run --extra dev pytest tests/test_word_exam_converter.py -q`

Expected: tests fail because `parse_exam_lines` is missing.

- [ ] **Step 3: Implement the state machine**

Implement these rules:

- First non-empty line is the title.
- Section headers set question type and default score.
- Question markers accept `1.` / `1、` / `1．` and reset numbering per section while global IDs remain `q1...qN`.
- Parenthesized objective answers are removed from the displayed question.
- Choice options may occur on the question line or following lines.
- Subjective answer paragraphs beginning with `答：` / `答案：` / `解：` continue until the next question or section.
- Question-level `（N分）` overrides the section default.
- Missing or ambiguous required fields append a `ParseIssue`; they are never filled with guessed values.
- `passing_score` is `0` unless the source document explicitly declares a passing threshold.

- [ ] **Step 4: Run state-machine tests and verify GREEN**

Run: `env PYTHONPATH=. uv run --extra dev pytest tests/test_word_exam_converter.py -q`

Expected: all converter tests pass.

- [ ] **Step 5: Commit state machine**

```bash
git add scripts/word_exam_converter.py tests/test_word_exam_converter.py
git commit -m "feat: parse Word exam paragraphs into question JSON"
```

---

### Task 3: Office extraction, conversion CLI, and validation

**Files:**
- Modify: `scripts/word_exam_converter.py`
- Modify: `tests/test_word_exam_converter.py`

**Interfaces:**
- Produces: `extract_with_officecli(source: Path, temp_dir: Path) -> str`
- Produces: `convert_directory(source_dir: Path, output_dir: Path) -> dict[str, object]`
- Produces CLI: `python scripts/word_exam_converter.py SOURCE_DIR --output OUTPUT_DIR`

- [ ] **Step 1: Write failing extraction/output tests**

Use a dependency-injected command runner to assert:

- `.docx` invokes `officecli view <path> text`.
- `.doc` invokes `textutil -convert docx -output <temp>` followed by officecli.
- A valid paper is written as UTF-8 JSON with `ensure_ascii=False`.
- A paper with parse issues is included in `conversion-report.json` with `needs_review`.
- System validation exceptions are recorded as `invalid`.

- [ ] **Step 2: Run tests and verify RED**

Run: `env PYTHONPATH=. uv run --extra dev pytest tests/test_word_exam_converter.py -q`

Expected: tests fail because extraction and conversion functions are missing.

- [ ] **Step 3: Implement extraction and output**

Implementation requirements:

- Enumerate only `.doc` and `.docx` files directly under the source directory.
- Use `tempfile.TemporaryDirectory()` for legacy conversion.
- Call subprocesses with argument arrays, `check=True`, `capture_output=True`, and `text=True`.
- Call `backend.question_loader.validate_questions(paper)` before writing a paper as valid.
- Write JSON atomically using a sibling `.tmp` file followed by `Path.replace()`.
- Report fields: `source`, `output`, `status`, `question_count`, `total_score`, `type_counts`, and `issues`.
- Continue after per-file failures.

- [ ] **Step 4: Run converter tests and full regression suite**

Run: `env PYTHONPATH=. uv run --extra dev pytest tests/test_word_exam_converter.py -q`

Expected: converter tests pass.

Run: `env PYTHONPATH=. uv run --extra dev pytest -q`

Expected: full suite passes.

- [ ] **Step 5: Commit extraction and CLI**

```bash
git add scripts/word_exam_converter.py tests/test_word_exam_converter.py
git commit -m "feat: convert Word exam directories to validated JSON"
```

---

### Task 4: Convert the source corpus and resolve document-specific parsing

**Files:**
- Modify: `scripts/word_exam_converter.py` when a source fragment exposes a missing general parsing rule.
- Modify: `tests/test_word_exam_converter.py` with the exact source fragment before each parser correction.
- Create: `docs/测试用试卷--含答案/json/*.json`
- Create: `docs/测试用试卷--含答案/json/conversion-report.json`

**Interfaces:**
- Consumes the Task 3 CLI.
- Produces the final 18-file conversion corpus and report.

- [ ] **Step 1: Run the converter against all source files**

Run:

```bash
env PYTHONPATH=. uv run python scripts/word_exam_converter.py \
  'docs/测试用试卷--含答案' \
  --output 'docs/测试用试卷--含答案/json'
```

Expected: 18 report entries; valid papers are written, and unresolved papers are marked `needs_review` or `invalid`.

- [ ] **Step 2: Inspect every unresolved entry against officecli text**

For each unresolved file, run `officecli view <source.docx> text` or inspect the temporary `.docx` equivalent. Add the smallest general parsing rule that explains the source format, then first add a failing regression test using the exact relevant text fragment.

- [ ] **Step 3: Repeat RED/GREEN conversion cycles**

After every new document-format rule:

1. Run the targeted regression test and observe the expected failure.
2. Implement the minimal parser change.
3. Run all converter tests.
4. Re-run the complete directory conversion.

Do not add a rule that silently invents missing source content.

- [ ] **Step 4: Validate all generated JSON independently**

Run a Python verification command that loads every JSON except `conversion-report.json`, calls `validate_questions()`, recomputes score totals, and asserts the report contains 18 source entries.

Expected: every delivered paper JSON loads successfully, passes validation, and has matching score totals.

- [ ] **Step 5: Final officecli/source audit**

For every paper, compare report counts and type distribution with the source section headings. Spot-check title, first question, last question, objective answers, and each subjective reference answer.

- [ ] **Step 6: Commit final converter adjustments and generated corpus**

```bash
git add scripts/word_exam_converter.py tests/test_word_exam_converter.py \
  'docs/测试用试卷--含答案/json'
git commit -m "data: convert Word exam corpus to validated JSON"
```

---

### Task 5: Final verification and handoff

**Files:**
- Verify: `scripts/word_exam_converter.py`
- Verify: `tests/test_word_exam_converter.py`
- Verify: `docs/测试用试卷--含答案/json/`

- [ ] **Step 1: Run fresh full tests**

Run: `env PYTHONPATH=. uv run --extra dev pytest -q`

Expected: zero failures.

- [ ] **Step 2: Run fresh corpus validation**

Run the independent JSON verification from Task 4 Step 4.

Expected: 18 report entries and zero invalid delivered JSON files.

- [ ] **Step 3: Check repository changes**

Run: `git status --short` and `git diff --check`.

Expected: only intended converter/test/output changes, with no whitespace errors and no changes to source Word files.

- [ ] **Step 4: Report outcome**

Report the output directory, number of converted papers, valid/needs-review counts, test result, and any source documents that could not yield a trustworthy answer.
