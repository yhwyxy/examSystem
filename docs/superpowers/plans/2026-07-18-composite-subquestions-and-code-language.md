# Composite Subquestions and Code Language Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在考试端为复合主观题的每个小问提供独立输入框，并让代码小问只能从出题人配置的语言白名单中选择语言后独立评分。

**Architecture:** 以 `type: "composite"` 和 `subquestions` 作为规范格式，加载时兼容现有 `sub_questions`；提交值按小问保存 `{answer, language?}`，评分器逐小问调用 subjective-scoring v0.1.7 并聚合父题结果。现有复合题实现作为基础增量调整，普通题与历史字符串答案保持兼容。

**Tech Stack:** Python 3.11、FastAPI、Pydantic、subjective-scoring 0.1.7、原生 JavaScript/HTML/CSS、pytest、openpyxl、JSON。

## Global Constraints

- subjective-scoring 固定为 v0.1.7，不新增评分依赖。
- 规范字段为 `type: "composite"`、`subquestions`、`allowed_languages`；读取时兼容旧 `sub_questions` 和 `code_language`。
- 代码语言只能取题目配置白名单，服务端不得信任客户端提交的评分配置。
- 普通题、旧复合题 JSON 和历史字符串答卷必须继续可读。
- 远程评分保持串行；本功能不得增加并发请求。
- 仅迁移规范中确认的 13 道普通主观题、测控新增 `q43` 和法务案例题。
- 不暂存或修改用户现有的 `data/questions.json` 删除、`answer.js` 和原始 Word 文件。

---

### Task 1: Canonical Composite Schema and Submission Validation

**Files:**
- Modify: `backend/question_loader.py`
- Modify: `backend/main.py`
- Test: `tests/test_papers.py`
- Test: `tests/test_submission_auto_submit.py`

**Interfaces:**
- Produces: `normalize_composite_question(question: dict[str, Any]) -> dict[str, Any]`
- Produces: `get_subquestions(question: dict[str, Any]) -> list[dict[str, Any]]`
- Produces: `normalize_submitted_subanswer(subquestion: dict[str, Any], raw: Any, *, allow_legacy: bool = False) -> tuple[str, str | None]`
- Consumes: existing `validate_questions`, `sanitize_for_student`, and `/api/submit` validation loop.

- [ ] **Step 1: Write failing schema and answer-shape tests**

Add tests that require the canonical format, language normalization, legacy aliases, and rejection of forged languages:

```python
def test_composite_canonical_schema_and_legacy_aliases():
    from backend.question_loader import get_subquestions, validate_questions

    paper = {
        "exam_info": {"title": "复合题"},
        "questions": [{
            "id": "c1", "type": "composite", "question": "父题", "score": 10,
            "subquestions": [
                {"id": "s1", "question": "解释", "answer": "A", "score": 4, "scoring_mode": "text"},
                {"id": "s2", "question": "编码", "answer": "print(1)", "score": 6,
                 "scoring_mode": "code", "allowed_languages": [" Python ", "javascript"]},
            ],
        }],
    }
    validate_questions(paper)
    assert get_subquestions(paper["questions"][0])[1]["allowed_languages"] == ["python", "javascript"]


def test_code_subanswer_language_must_be_allowed():
    from backend.question_loader import normalize_submitted_subanswer
    sub = {"id": "s1", "scoring_mode": "code", "allowed_languages": ["python"]}
    assert normalize_submitted_subanswer(sub, {"answer": "print(1)", "language": "Python"}) == ("print(1)", "python")
    with pytest.raises(ValueError, match="INVALID_CODE_LANGUAGE"):
        normalize_submitted_subanswer(sub, {"answer": "console.log(1)", "language": "javascript"})
```

- [ ] **Step 2: Run the focused tests and verify failure**

Run: `uv run pytest -q tests/test_papers.py tests/test_submission_auto_submit.py`

Expected: FAIL because `composite`, `subquestions`, `allowed_languages`, and the normalizer do not exist.

- [ ] **Step 3: Implement canonical normalization and validation**

Add focused helpers and route all existing composite checks through them:

```python
ALLOWED_TYPES = {"single_choice", "multiple_choice", "true_false", "short_answer", "essay", "composite"}


def normalize_composite_question(question: dict[str, Any]) -> dict[str, Any]:
    if "subquestions" not in question and isinstance(question.get("sub_questions"), list):
        question["subquestions"] = question.pop("sub_questions")
    if question.get("subquestions") and question.get("type") in SUBJECTIVE_TYPES:
        question["type"] = "composite"
    for sub in question.get("subquestions") or []:
        legacy = sub.pop("code_language", None)
        languages = sub.get("allowed_languages") or ([legacy] if legacy else [])
        normalized = [_normalize_code_language(value, qid=str(sub.get("id"))) for value in languages]
        sub["allowed_languages"] = list(dict.fromkeys(lang for lang in normalized if lang))
    return question


def get_subquestions(question: dict[str, Any]) -> list[dict[str, Any]]:
    normalize_composite_question(question)
    value = question.get("subquestions")
    return value if isinstance(value, list) else []


def normalize_submitted_subanswer(
    subquestion: dict[str, Any], raw: Any, *, allow_legacy: bool = False
) -> tuple[str, str | None]:
    if isinstance(raw, str):
        if subquestion.get("scoring_mode") == "code" and not allow_legacy:
            raise ValueError("INVALID_ANSWER_SHAPE: 代码子题答案必须包含 language")
        answer = raw
        language = (subquestion.get("allowed_languages") or [None])[0]
    elif isinstance(raw, dict) and isinstance(raw.get("answer", ""), str):
        answer, language = raw.get("answer", ""), raw.get("language")
    else:
        raise ValueError("INVALID_ANSWER_SHAPE: 子题答案必须包含字符串 answer")
    if subquestion.get("scoring_mode") == "code":
        normalized = str(language or "").strip().lower()
        if normalized not in subquestion.get("allowed_languages", []):
            raise ValueError("INVALID_CODE_LANGUAGE: 代码语言不在允许范围内")
        return answer, normalized
    return answer, None
```

Update `validate_questions` to normalize before validating, require `type == "composite"` for canonical writes, require non-empty unique `allowed_languages` only for code subquestions, and require subquestion score sum to equal the parent. Update `sanitize_for_student` to expose `subquestions`, `scoring_mode`, and `allowed_languages` while stripping answers, rubrics, points, and calculations. Update `/api/submit` to convert either validation error to a 422 response with code `INVALID_ANSWER_SHAPE` or `INVALID_CODE_LANGUAGE`.

- [ ] **Step 4: Run schema and API tests**

Run: `uv run pytest -q tests/test_papers.py tests/test_submission_auto_submit.py`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add backend/question_loader.py backend/main.py tests/test_papers.py tests/test_submission_auto_submit.py
git commit -m "feat: validate composite answers and language choices"
```

### Task 2: Per-Subquestion Grading with Selected Language

**Files:**
- Modify: `backend/grader.py`
- Test: `tests/test_core.py`

**Interfaces:**
- Consumes: `get_subquestions` and `normalize_submitted_subanswer` from Task 1.
- Produces: `grade_composite_question(question, raw_answer)` details containing `sub_results[*].selected_language`.

- [ ] **Step 1: Write failing grading tests**

```python
def test_grade_composite_passes_selected_language(monkeypatch):
    from backend import grader
    parent = {
        "id": "c1", "type": "composite", "question": "父", "score": 5,
        "subquestions": [{"id": "s1", "question": "代码", "answer": "print(1)",
                          "score": 5, "scoring_mode": "code", "allowed_languages": ["python", "javascript"]}],
    }
    seen = {}
    async def fake_run(question, student_answer):
        seen.update(question)
        return {"machine_score": 5, "final_score": 5, "score": 5, "max_score": 5,
                "review_status": "high_confidence", "low_confidence": False}
    monkeypatch.setattr(grader, "_run_subjective_grading", fake_run)
    detail = asyncio.run(grader.grade_composite_question(
        parent, {"s1": {"answer": "print(1)", "language": "python"}}
    ))
    assert seen["code_language"] == "python"
    assert detail["sub_results"][0]["selected_language"] == "python"
```

- [ ] **Step 2: Verify the test fails**

Run: `uv run pytest -q tests/test_core.py -k 'composite or code_language'`

Expected: FAIL because the existing grader treats the nested object as text and uses fixed `code_language`.

- [ ] **Step 3: Update composite grading**

Replace direct `raw.get(sid, "")` handling with the Task 1 normalizer:

```python
for sub in get_subquestions(question):
    sid = str(sub["id"])
    answer, selected_language = normalize_submitted_subanswer(
        sub, raw.get(sid, {"answer": ""}), allow_legacy=True
    )
    sub_q = {**sub, "type": "short_answer", "paper_id": question.get("paper_id"), "id": f"{qid}:{sid}"}
    if selected_language:
        sub_q["code_language"] = selected_language
    sub_detail = await _run_subjective_grading(sub_q, answer)
    sub_detail.update({
        "sub_question_id": sid,
        "question_id": qid,
        "question": sub.get("question", ""),
        "selected_language": selected_language,
    })
```

Keep the loop sequential. Aggregate machine/final scores and review state exactly once at the parent level. Accept legacy string values for historical regrade only; a legacy code answer uses the first allowed language.

- [ ] **Step 4: Run grading and review tests**

Run: `uv run pytest -q tests/test_core.py`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add backend/grader.py tests/test_core.py
git commit -m "feat: score composite code answers by selected language"
```

### Task 3: Exam-Side Inputs and Language Selector

**Files:**
- Modify: `frontend/js/exam.js`
- Modify: `frontend/css/style.css`
- Test: `tests/test_frontend_static.py`

**Interfaces:**
- Consumes: public `question.subquestions[*].allowed_languages` from Task 1.
- Produces: `collectAnswers()` values shaped as `{subId: {answer, language?}}`.

- [ ] **Step 1: Add failing static-contract tests**

```python
def test_exam_renders_code_language_whitelist_and_nested_answers():
    script = Path("frontend/js/exam.js").read_text(encoding="utf-8")
    assert "allowed_languages" in script
    assert "data-language-for" in script
    assert "{ answer:" in script
    assert "subquestions" in script
```

- [ ] **Step 2: Verify failure**

Run: `uv run pytest -q tests/test_frontend_static.py`

Expected: FAIL because the current renderer uses `sub_questions` and has no language select.

- [ ] **Step 3: Render stable subquestion controls**

For each code subquestion, create a label and select before the textarea:

```javascript
const languages = Array.isArray(s.allowed_languages) ? s.allowed_languages : [];
if (s.scoring_mode === 'code') {
  const select = document.createElement('select');
  select.className = 'code-language-select';
  select.dataset.languageFor = `${q.id}__${s.id}`;
  for (const language of languages) {
    const option = document.createElement('option');
    option.value = language;
    option.textContent = language;
    select.appendChild(option);
  }
  block.appendChild(select);
}
```

Use canonical `subquestions`, preserve code when the select changes, and collect values as:

```javascript
map[s.id] = {
  answer: textarea ? textarea.value : '',
  ...(languageSelect ? { language: languageSelect.value } : {}),
};
```

Add `.composite-answers`, `.sub-answer`, and `.code-language-select` styles with responsive width constraints and no nested card styling.

- [ ] **Step 4: Run frontend static tests**

Run: `uv run pytest -q tests/test_frontend_static.py`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add frontend/js/exam.js frontend/css/style.css tests/test_frontend_static.py
git commit -m "feat: add per-subquestion answer and language controls"
```

### Task 4: Admin Editor, Review Detail, and Export

**Files:**
- Modify: `frontend/js/papers.js`
- Modify: `frontend/js/detail.js`
- Modify: `frontend/css/style.css`
- Modify: `backend/exporter.py`
- Test: `tests/test_frontend_static.py`
- Test: `tests/test_core.py`

**Interfaces:**
- Consumes: canonical subquestion schema and `selected_language` grading detail.
- Produces: admin-authored `allowed_languages`, per-subquestion review display, and language-aware XLSX text.

- [ ] **Step 1: Write failing editor and export tests**

```python
def test_composite_export_includes_selected_language():
    from backend.exporter import format_student_answer_for_export
    detail = {"is_composite": True, "sub_results": [{
        "sub_question_id": "s1", "student_answer": "print(1)", "selected_language": "python"
    }]}
    assert "[python]" in format_student_answer_for_export(detail)
```

Add static assertions for `allowed_languages`, a multi-select `.sq-languages`, and `selected_language` in detail rendering.

- [ ] **Step 2: Verify failure**

Run: `uv run pytest -q tests/test_frontend_static.py tests/test_core.py -k 'export or composite or paper'`

Expected: FAIL because the editor stores one `code_language` and review/export omit selected language.

- [ ] **Step 3: Update admin and export paths**

Replace `.sq-lang` with a multiple select populated from the fixed supported-language list. Collect selected options only for code mode:

```javascript
const allowedLanguages = [...row.querySelector('.sq-languages').selectedOptions].map(o => o.value);
if (mode === 'code') item.allowed_languages = allowedLanguages;
```

Require at least one language for code mode. Keep add/delete/reorder behavior and canonical `subquestions`. In `detail.js`, display `语言：${selected_language}` next to code subquestion scores. In `exporter.py`, format each code answer as:

```python
language = s.get("selected_language")
prefix = f"[{sid}][{language}]" if language else f"[{sid}]"
lines.append(f"{prefix} {s.get('student_answer', '')}")
```

- [ ] **Step 4: Run admin, review, and export tests**

Run: `uv run pytest -q tests/test_frontend_static.py tests/test_core.py tests/test_papers.py`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add frontend/js/papers.js frontend/js/detail.js frontend/css/style.css backend/exporter.py tests/test_frontend_static.py tests/test_core.py tests/test_papers.py
git commit -m "feat: manage and display composite language choices"
```

### Task 5: Migrate the Confirmed Paper Questions

**Files:**
- Create: `scripts/migrate_composite_questions.py`
- Create: `tests/test_composite_paper_migration.py`
- Modify: `data/papers/mechanical.json`
- Modify: `data/papers/materials.json`
- Modify: `data/papers/instrumentation.json`
- Modify: `data/papers/chemical-analysis.json`
- Modify: `data/papers/chemical-engineering.json`
- Modify: `data/papers/metal-materials.json`
- Modify: `data/papers/legal.json`
- Modify: `data/papers/index.json`
- Create: `docs/测试用试卷--含答案/json/composite-question-migration-report.json`

**Interfaces:**
- Consumes: canonical schema from Task 1.
- Produces: idempotent `migrate_papers(papers_dir: Path) -> dict[str, Any]` and validated paper JSON.

- [ ] **Step 1: Write failing migration tests**

```python
def test_confirmed_papers_have_expected_composites():
    expected = {
        "mechanical.json": {"q41", "q44"},
        "materials.json": {"q42", "q43"},
        "instrumentation.json": {"q41", "q42", "q43"},
        "chemical-analysis.json": {"q42", "q43"},
        "chemical-engineering.json": {"q42", "q44"},
        "metal-materials.json": {"q43"},
        "legal.json": {"q35"},
    }
    for filename, ids in expected.items():
        paper = json.loads((Path("data/papers") / filename).read_text(encoding="utf-8"))
        composites = {q["id"] for q in paper["questions"] if q.get("type") == "composite"}
        assert ids <= composites


def test_instrumentation_and_legal_totals():
    instrumentation = load("instrumentation.json")
    legal = load("legal.json")
    assert instrumentation["exam_info"]["total_score"] == 100
    assert next(q for q in instrumentation["questions"] if q["id"] == "q43")["score"] == 20
    legal_case = next(q for q in legal["questions"] if q["id"] == "q35")
    assert [s["score"] for s in legal_case["subquestions"]] == [4, 5, 5, 5, 5, 4, 4]
    assert sum(s["score"] for s in legal_case["subquestions"]) == 32
```

- [ ] **Step 2: Verify migration tests fail**

Run: `uv run pytest -q tests/test_composite_paper_migration.py`

Expected: FAIL because the papers are still single-answer questions and instrumentation lacks `q43`.

- [ ] **Step 3: Implement an explicit, idempotent migration**

The script must use an explicit target map, never infer arbitrary numbered text:

```python
TARGETS = {
    "mechanical": ["q41", "q44"],
    "materials": ["q42", "q43"],
    "instrumentation": ["q41", "q42"],
    "chemical-analysis": ["q42", "q43"],
    "chemical-engineering": ["q42", "q44"],
    "metal-materials": ["q43"],
}


def split_scores(total: float, count: int) -> list[float]:
    unit = round(total / count, 4)
    values = [unit] * count
    values[-1] = round(total - sum(values[:-1]), 4)
    return values
```

Store exact question/answer segment boundaries for each target in the script. Add instrumentation `q43` with three subquestions: measurement range `100-200 kPa`, span `100 kPa`, and output mapping `100/150/200 kPa -> 4/12/20 mA`; configure calculation expected values and tolerances per subquestion. Rebuild legal `q35` from Word paragraphs 207-228 as one 32-point parent with seven subquestions and delete old `q36`-`q39`; preserve all seven exact reference answers and scores `4,5,5,5,5,4,4`. Recompute paper totals and update the instrumentation entry in `index.json` to 100.

Write a report containing `paper`, `question_id`, `source`, `before_score`, `after_score`, and child IDs. A second run must make no changes.

- [ ] **Step 4: Run migration and validation tests**

Run: `uv run python scripts/migrate_composite_questions.py`

Expected: report lists 13 migrated/created parent questions and no errors.

Run: `uv run pytest -q tests/test_composite_paper_migration.py tests/test_papers.py`

Expected: PASS.

- [ ] **Step 5: Commit migrated papers**

```bash
git add scripts/migrate_composite_questions.py tests/test_composite_paper_migration.py data/papers/mechanical.json data/papers/materials.json data/papers/instrumentation.json data/papers/chemical-analysis.json data/papers/chemical-engineering.json data/papers/metal-materials.json data/papers/legal.json data/papers/index.json docs/测试用试卷--含答案/json/composite-question-migration-report.json
git commit -m "feat: migrate confirmed papers to composite questions"
```

### Task 6: Full Regression, Browser Verification, and Index Refresh

**Files:**
- Modify only if verification finds a defect in files owned by Tasks 1-5.

**Interfaces:**
- Consumes: all prior tasks.
- Produces: verified release-ready branch and refreshed codebase-memory graph.

- [ ] **Step 1: Validate every paper**

Run:

```bash
uv run python -c "import json; from pathlib import Path; from backend.question_loader import validate_questions; files=[p for p in Path('data/papers').glob('*.json') if p.name!='index.json']; [validate_questions(json.loads(p.read_text(encoding='utf-8'))) for p in files]; print(f'{len(files)} papers valid')"
```

Expected: `22 papers valid` and no exception.

- [ ] **Step 2: Run the complete test suite**

Run: `uv run pytest -q`

Expected: all tests pass; no new warnings beyond the existing exception-path logging.

- [ ] **Step 3: Start the app and verify desktop/mobile flows**

Run: `env PYTHONPATH=. uv run uvicorn backend.main:app --host 127.0.0.1 --port 8000`

Verify with Playwright at desktop and mobile widths:

- composite text questions render one textarea per subquestion;
- code subquestions show only configured languages;
- switching language preserves code;
- submission payload includes answer and language;
- admin detail displays language and supports child-score review;
- controls do not overlap or overflow.

- [ ] **Step 4: Refresh codebase-memory after the final code commit**

Run: `codebase-memory-mcp cli index_repository '{"repo_path": "'$(pwd)'"}'`

Expected: `status: indexed` with non-zero nodes and edges.

- [ ] **Step 5: Final repository check**

Run: `git status --short`

Expected: only the pre-existing user-owned `data/questions.json`, `answer.js`, and original Word files remain; no task-owned files are uncommitted.
