# 复合题 sub_questions + code_language 透传 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 支持 `short_answer`/`essay` 下的 `sub_questions` 复合题（父题干 + 多子题独立作答与评分），并把 `code_language` 可靠透传到评分库，同时打通管理端编辑、考试端作答、复核与导出。

**Architecture:** 沿用现有「父题一条 `grading_detail`」模型；复合题在 detail 内嵌 `sub_results[]`，父题 `score`/`final_score` 为子题分数之和。提交答案形态：单题 `string`，复合题 `{sub_id: string}`。校验集中在 `question_loader`；评分循环在 `grader`；复核在 `database.apply_review` 支持可选 `sub_question_id`；前端三处（试卷编辑、考试作答、复核详情）按 `sub_questions` 分支渲染。

**Tech Stack:** Python 3 / FastAPI、`subjective_scoring`（`ScoringRequest`/`ScoringMode`）、SQLite `grading_detail_json`、原生前端 JS（`papers.js` / `exam.js` / `detail.js`）、pytest + FastAPI TestClient。

## Global Constraints

- 仅 `short_answer` / `essay` 可带 `sub_questions`；客观题禁止。
- `sub_questions` 非空时父题 `score` 必须等于子题 `score` 之和（允许浮点容差 `1e-6`）。
- 子题 `id` 在父题内唯一；子题字段复用主观题规则（`scoring_mode` / `code_language` / `calculation` / `scoring_points` 等）。
- 复合题学生答案必须为对象 map，禁止字符串；单题禁止 map。
- 提交非法答案形状 → HTTP **422**，错误码 `INVALID_ANSWER_SHAPE`。
- `code_language` 透传到 `ScoringRequest.code_language`；`scoring_mode=code` 时语言必填且合法。
- 兼容旧数据：无 `sub_questions` 的题与字符串答案路径不变。
- 不做数据库表结构迁移；只扩展 `grading_detail` JSON 形状。
- 中文 UI 文案；TDD：先写失败测试再实现。

## File Structure

| 文件 | 职责 |
|------|------|
| `backend/question_loader.py` | 复合题校验、`code_language` 校验、学生端脱敏、提交答案形状校验 helper |
| `backend/grader.py` | 复合题逐子题评分、detail 聚合、`code_language` 透传（已有则加固） |
| `backend/main.py` | 提交时调用答案形状校验；复核 API 增加可选 `sub_question_id` |
| `backend/database.py` | `apply_review` 支持按子题改分并回写父题合计 |
| `backend/exporter.py` | 导出展示子题答案 / 分项得分 |
| `backend/review_service.py` | 若需，保证复核清单可识别复合题（多数情况只消费 detail） |
| `frontend/admin.html` | 题目表单：`calculation` 模式 + 子题编辑区 |
| `frontend/js/papers.js` | 读写 `sub_questions`，父分值自动汇总 |
| `frontend/js/exam.js` | 复合题多 textarea；提交 map |
| `frontend/js/detail.js` | 子题分项展示与分项复核 |
| `tests/test_core.py` / `tests/test_papers.py` / 新建测试 | 校验、评分、提交、复核、导出 |

---

### Task 1: 题库校验与脱敏（`sub_questions` + `code_language`）

**Files:**
- Modify: `backend/question_loader.py`
- Test: `tests/test_papers.py`（或 `tests/test_core.py`）

**Interfaces:**
- Consumes: 现有 `validate_questions(data) -> dict`、`sanitize_for_student(q) -> dict`
- Produces:
  - `ALLOWED_CODE_LANGUAGES: frozenset[str]`（至少：`python`, `java`, `javascript`, `typescript`, `go`, `c`, `cpp`, `csharp`, `sql`, `bash`, `shell`；大小写不敏感入库前规范化为小写）
  - `validate_answer_shape(question: dict, answer: Any) -> None`（非法则 `ValueError`，消息含 `INVALID_ANSWER_SHAPE`）
  - `is_composite_question(q: dict) -> bool`

- [ ] **Step 1: 写失败测试 — 复合题 schema 与答案形状**

在 `tests/test_papers.py` 追加（与现有 `papers_env` / `validate_questions` 风格一致）：

```python
def test_composite_sub_questions_schema_ok():
    from backend.question_loader import validate_questions

    data = {
        "exam_info": {"title": "复合题"},
        "questions": [
            {
                "id": "c1",
                "type": "short_answer",
                "question": "阅读下列材料并回答",
                "score": 10,
                "sub_questions": [
                    {
                        "id": "s1",
                        "question": "解释概念",
                        "answer": "参考A",
                        "score": 4,
                        "scoring_mode": "text",
                    },
                    {
                        "id": "s2",
                        "question": "写 SQL",
                        "answer": "SELECT 1",
                        "score": 6,
                        "scoring_mode": "sql",
                        "code_language": "SQL",
                    },
                ],
            }
        ],
    }
    out = validate_questions(data)
    q = out["questions"][0]
    assert len(q["sub_questions"]) == 2
    assert q["sub_questions"][1]["code_language"] == "sql"


def test_composite_score_must_equal_sum():
    from backend.question_loader import validate_questions
    import pytest

    data = {
        "exam_info": {"title": "t"},
        "questions": [
            {
                "id": "c1",
                "type": "short_answer",
                "question": "父",
                "score": 9,
                "sub_questions": [
                    {"id": "s1", "question": "a", "answer": "a", "score": 4, "scoring_mode": "text"},
                    {"id": "s2", "question": "b", "answer": "b", "score": 6, "scoring_mode": "text"},
                ],
            }
        ],
    }
    with pytest.raises(ValueError, match="sub_questions"):
        validate_questions(data)


def test_code_mode_requires_language():
    from backend.question_loader import validate_questions
    import pytest

    data = {
        "exam_info": {"title": "t"},
        "questions": [
            {
                "id": "q1",
                "type": "short_answer",
                "question": "写代码",
                "answer": "print(1)",
                "score": 5,
                "scoring_mode": "code",
            }
        ],
    }
    with pytest.raises(ValueError, match="code_language"):
        validate_questions(data)


def test_validate_answer_shape_composite_and_single():
    from backend.question_loader import validate_answer_shape
    import pytest

    composite = {
        "id": "c1",
        "type": "short_answer",
        "sub_questions": [{"id": "s1", "score": 5}, {"id": "s2", "score": 5}],
    }
    validate_answer_shape(composite, {"s1": "a", "s2": "b"})
    with pytest.raises(ValueError, match="INVALID_ANSWER_SHAPE"):
        validate_answer_shape(composite, "plain string")
    with pytest.raises(ValueError, match="INVALID_ANSWER_SHAPE"):
        validate_answer_shape(composite, {"s1": "only-one"})

    single = {"id": "q1", "type": "short_answer"}
    validate_answer_shape(single, "ok")
    with pytest.raises(ValueError, match="INVALID_ANSWER_SHAPE"):
        validate_answer_shape(single, {"s1": "x"})


def test_sanitize_keeps_sub_questions_strips_answers():
    from backend.question_loader import sanitize_for_student

    q = {
        "id": "c1",
        "type": "short_answer",
        "question": "父干",
        "score": 10,
        "answer": "不应出现",
        "sub_questions": [
            {
                "id": "s1",
                "question": "子1",
                "answer": "密钥",
                "score": 10,
                "scoring_mode": "text",
                "scoring_points": [{"id": "p1", "text": "点", "score": 10}],
            }
        ],
    }
    out = sanitize_for_student(q)
    assert "answer" not in out
    assert "scoring_points" not in out
    assert out["sub_questions"][0]["id"] == "s1"
    assert out["sub_questions"][0]["question"] == "子1"
    assert "answer" not in out["sub_questions"][0]
    assert "scoring_points" not in out["sub_questions"][0]
```

- [ ] **Step 2: 运行测试确认失败**

```bash
cd /Users/yhw/Code/Github/examSystem && .venv/bin/python -m pytest tests/test_papers.py::test_composite_sub_questions_schema_ok tests/test_papers.py::test_composite_score_must_equal_sum tests/test_papers.py::test_code_mode_requires_language tests/test_papers.py::test_validate_answer_shape_composite_and_single tests/test_papers.py::test_sanitize_keeps_sub_questions_strips_answers -v
```

Expected: FAIL（函数/规则未实现或 assert 失败）

- [ ] **Step 3: 实现 `question_loader` 校验与 helper**

在 `backend/question_loader.py` 增加常量与函数（放在 `SUBJECTIVE_TYPES` 附近）：

```python
ALLOWED_CODE_LANGUAGES = frozenset({
    "python", "java", "javascript", "typescript", "go",
    "c", "cpp", "csharp", "sql", "bash", "shell",
})


def is_composite_question(q: dict[str, Any]) -> bool:
    subs = q.get("sub_questions")
    return isinstance(subs, list) and len(subs) > 0


def _normalize_code_language(raw: Any) -> str | None:
    if raw is None or raw == "":
        return None
    lang = str(raw).strip().lower()
    if lang not in ALLOWED_CODE_LANGUAGES:
        raise ValueError(f"不支持的 code_language: {raw}")
    return lang


def validate_answer_shape(question: dict[str, Any], answer: Any) -> None:
    """校验提交答案形状。非法时 raise ValueError('INVALID_ANSWER_SHAPE: ...')。"""
    if is_composite_question(question):
        if not isinstance(answer, dict):
            raise ValueError("INVALID_ANSWER_SHAPE: 复合题答案必须为对象 map")
        expected = {str(s.get("id")) for s in question["sub_questions"] if s.get("id")}
        got = {str(k) for k in answer.keys()}
        if got != expected:
            raise ValueError(
                f"INVALID_ANSWER_SHAPE: 复合题答案键必须为 {sorted(expected)}，实际 {sorted(got)}"
            )
        for k, v in answer.items():
            if v is not None and not isinstance(v, str):
                raise ValueError(f"INVALID_ANSWER_SHAPE: 子题 {k} 答案必须为字符串")
        return
    if isinstance(answer, dict):
        raise ValueError("INVALID_ANSWER_SHAPE: 非复合题答案不能为对象")
```

扩展 `_validate_one_question`（主观题分支）要点：

1. 若存在 `sub_questions`：
   - 必须为非空 list（空 list 当作非法或直接删除字段，推荐：**空 list 视为非法**，要求省略字段）
   - 仅 `short_answer`/`essay`
   - 父题 **不应** 再要求顶层 `answer`（复合题答案在子题上）；若有顶层 `answer` 可忽略或报错——按设计：**父题可不写 answer，子题必须有 answer**
   - 每个子题：`id`（非空 str）、`question`、`answer`、`score>0`，并递归套用与单题相同的 `scoring_mode` / `code_language` / `calculation` / `scoring_points` 校验
   - 子题 `id` 唯一
   - `abs(parent.score - sum(sub.score)) <= 1e-6`
   - 规范化每个子题的 `code_language` 小写
2. 无 `sub_questions` 时保持现状：要求 `answer` 非空等
3. 任意主观题（父或子）`scoring_mode == "code"` → 必须有合法 `code_language`
4. `scoring_mode == "sql"` → 若给了 `code_language` 则必须是 `sql`；未给可自动补 `sql`
5. 规范化顶层 `code_language` 小写

扩展 `sanitize_for_student`：

```python
# 在返回 public 前：
if is_composite_question(q):
    public["sub_questions"] = [
        {
            "id": s.get("id"),
            "question": s.get("question", ""),
            "score": s.get("score"),
            # 不暴露 answer / scoring_points / scoring_rubric / calculation 细节中的答案
            # 可保留 scoring_mode、code_language 供前端展示提示（按设计：可保留 mode/lang）
            **({"scoring_mode": s["scoring_mode"]} if s.get("scoring_mode") else {}),
            **({"code_language": s["code_language"]} if s.get("code_language") else {}),
        }
        for s in q["sub_questions"]
    ]
# 复合题不要把父级 answer 放进 public（本就应被剔除）
```

- [ ] **Step 4: 跑测试通过**

```bash
.venv/bin/python -m pytest tests/test_papers.py::test_composite_sub_questions_schema_ok tests/test_papers.py::test_composite_score_must_equal_sum tests/test_papers.py::test_code_mode_requires_language tests/test_papers.py::test_validate_answer_shape_composite_and_single tests/test_papers.py::test_sanitize_keeps_sub_questions_strips_answers tests/test_papers.py::test_calculation_question_schema_is_accepted -v
```

Expected: PASS（含既有 calculation 用例）

- [ ] **Step 5: Commit**

```bash
git add backend/question_loader.py tests/test_papers.py
git commit -m "feat(loader): validate composite sub_questions and code_language"
```

---

### Task 2: 提交接口拒绝非法答案形状（422）

**Files:**
- Modify: `backend/main.py`
- Test: `tests/test_core.py` 或 `tests/test_papers.py`

**Interfaces:**
- Consumes: `question_loader.validate_answer_shape`、`question_loader.load_questions`
- Produces: `POST /api/submit` 在写入 pending 前校验每道题答案形状

- [ ] **Step 1: 写失败测试**

```python
def test_submit_rejects_bad_composite_answer_shape(client, monkeypatch, tmp_path):
    """使用 TestClient：构造含复合题的试卷，提交字符串答案 → 422 INVALID_ANSWER_SHAPE。"""
    # 若项目 submit 依赖考试会话，先 exam/start；或直接单测 helper + 在 main 中挂校验后用 client。
    from backend.question_loader import validate_answer_shape
    import pytest
    q = {
        "id": "c1",
        "type": "short_answer",
        "sub_questions": [{"id": "s1", "score": 5}],
    }
    with pytest.raises(ValueError, match="INVALID_ANSWER_SHAPE"):
        validate_answer_shape(q, "x")
```

补充集成测试（按项目现有 submit fixture 模式；参考 `tests/test_core.py` 中 admin/submit 相关）：

```python
def test_submit_composite_answer_shape_http(papers_env, monkeypatch):
    from backend import main as main_mod
    from backend import question_loader as ql
    from backend import paper_store
    from fastapi.testclient import TestClient

    paper = {
        "exam_info": {"title": "复合", "total_score": 10, "passing_score": 0},
        "questions": [
            {
                "id": "c1",
                "type": "short_answer",
                "question": "父",
                "score": 10,
                "sub_questions": [
                    {"id": "s1", "question": "子", "answer": "答", "score": 10, "scoring_mode": "text"}
                ],
            }
        ],
    }
    # 通过 paper_store / 写 JSON 创建 slug=demo 的已开放试卷（按现有测试工具函数）
    # ... 打开试卷、start、submit ...
    # assert status_code == 422
    # assert body detail code == INVALID_ANSWER_SHAPE
```

> 实现时对照仓库里**已有**「创建试卷 + open + start + submit」的 helper；若没有，在本测试文件写最小 `_open_paper_and_start(client, slug, employee_id)`。

- [ ] **Step 2: 运行确认失败（HTTP 路径尚未挂钩时）**

```bash
.venv/bin/python -m pytest tests/test_papers.py -k "answer_shape" -v
```

- [ ] **Step 3: 在 `submit` 中校验**

在 `backend/main.py` 的 `submit` 中，`insert_submission_pending` **之前**：

```python
full = question_loader.load_questions(paper_id)
qmap = {str(q["id"]): q for q in full.get("questions", [])}
for qid, ans in (req.answers or {}).items():
    q = qmap.get(str(qid))
    if not q:
        continue
    try:
        question_loader.validate_answer_shape(q, ans)
    except ValueError as e:
        msg = str(e)
        raise _error(422, "INVALID_ANSWER_SHAPE", msg) from e
```

确保 `_error` 支持 422；若现有 `_error` 仅用于 4xx 通用，保持一致即可。

- [ ] **Step 4: 测试通过**

```bash
.venv/bin/python -m pytest tests/test_papers.py -k "answer_shape or composite" -v
```

- [ ] **Step 5: Commit**

```bash
git add backend/main.py tests/test_papers.py
git commit -m "feat(submit): reject invalid composite answer shapes with 422"
```

---

### Task 3: 复合题评分与 `code_language` 透传（`grader`）

**Files:**
- Modify: `backend/grader.py`
- Test: `tests/test_core.py`

**Interfaces:**
- Consumes: `build_scoring_request(question, student_answer)`、`score_async`、`detail_from_scoring_result`
- Produces:
  - 复合题 detail 形状：
    ```python
    {
      "question_id": "c1",
      "type": "short_answer",
      "question": "父干",
      "is_composite": True,
      "student_answer": {"s1": "...", "s2": "..."},
      "max_score": 10,
      "score": 7.0,
      "final_score": 7.0,
      "grading_method": "composite",
      "review_status": "need_review" | "high_confidence" | ...,
      "sub_results": [
        {
          "sub_question_id": "s1",
          "question": "子干",
          "student_answer": "...",
          "reference_answer": "...",
          "max_score": 4,
          "score": 3.0,
          "final_score": 3.0,
          "grading_method": "...",
          "review_status": "...",
          "reason": "...",
          "confidence": 0.9,
          # 与 detail_from_scoring_result 相同的 matched_points 等字段
        }
      ],
      "reason": "子题得分汇总: s1=3/4; s2=4/6",
    }
    ```
  - 父题 `review_status`：任一子题需复核 → 父题取更严重状态（`low_confidence` > `need_review` > `high_confidence`）

- [ ] **Step 1: 写失败测试（mock score_async）**

```python
import asyncio
from unittest.mock import AsyncMock, patch

def test_build_scoring_request_passes_code_language():
    from backend.grader import build_scoring_request
    q = {
        "id": "q1",
        "type": "short_answer",
        "question": "写 python",
        "answer": "print(1)",
        "score": 5,
        "scoring_mode": "code",
        "code_language": "python",
    }
    req = build_scoring_request(q, "print(1)")
    assert req.code_language == "python"
    assert req.scoring_mode.value == "code"  # 或 str(req.scoring_mode) 含 code


def test_grade_composite_sums_sub_scores():
    from backend import grader
    from subjective_scoring.models.schemas import ScoringResult, ReviewLevel

    parent = {
        "id": "c1",
        "type": "short_answer",
        "question": "父",
        "score": 10,
        "sub_questions": [
            {"id": "s1", "question": "子1", "answer": "A", "score": 4, "scoring_mode": "text"},
            {"id": "s2", "question": "子2", "answer": "B", "score": 6, "scoring_mode": "text"},
        ],
    }

    async def fake_score(req):
        # 按 question_id 返回不同分
        score = 4.0 if req.question_id.endswith("s1") or req.question_id == "c1:s1" else 3.0
        return ScoringResult(
            score=score,
            max_score=req.max_score,
            confidence=0.9,
            need_manual_review=False,
            review_level=ReviewLevel.AUTO_PASS,  # 按库实际枚举名调整
            reason="ok",
            matched_points=[],
            missed_points=[],
            scoring_method="text",
        )

    # 实现后 grade 路径会把子题 id 编入 ScoringRequest.question_id，例如 f"{parent_id}:{sub_id}"
    # 测试应断言 detail["score"] == sum(sub scores)、len(sub_results)==2、student_answer 为 map
```

实现时先读 `ScoringResult` 真实字段，测试里构造合法对象；若构造困难，可 mock `detail_from_scoring_result` 外围的 `score_one_question`。

更稳妥的单测方式：

```python
def test_grade_composite_unit(monkeypatch):
    from backend import grader

    calls = []

    async def fake_score_subjective(question, student_answer: str):
        calls.append((question.get("id"), student_answer))
        return {
            "question_id": question.get("id"),
            "type": question.get("type"),
            "question": question.get("question"),
            "student_answer": student_answer,
            "reference_answer": question.get("answer"),
            "max_score": float(question.get("score") or 0),
            "score": 2.0 if question.get("id") == "s1" else 5.0,
            "final_score": 2.0 if question.get("id") == "s1" else 5.0,
            "grading_method": "subjective_scoring",
            "review_status": "high_confidence",
            "reason": "ok",
            "confidence": 0.95,
        }

    # 将 grade_submission 内对单题主观评分提取为可 patch 函数 grade_subjective_question
    monkeypatch.setattr(grader, "grade_subjective_question", fake_score_subjective)

    # 同步包装：asyncio.run(grader.grade_submission(...)) 仅含复合一题
    ...
    assert detail["score"] == 7.0
    assert detail["is_composite"] is True
    assert len(detail["sub_results"]) == 2
```

- [ ] **Step 2: 运行确认失败**

```bash
.venv/bin/python -m pytest tests/test_core.py -k "composite or code_language" -v
```

- [ ] **Step 3: 实现评分逻辑**

1. 抽出 `async def grade_subjective_question(question, student_answer: str) -> dict`：内部 `build_scoring_request` + `score_async` + `detail_from_scoring_result`（把现有循环体搬进去）。
2. `build_scoring_request` 确认：
   - `code_language=question.get("code_language")` 已传入（已有则补测试即可）
   - 子题评分时：用子题 dict 作为 `question`，但 `question_id` 建议 `f"{parent_id}:{sub_id}"` 便于日志；detail 里 `sub_question_id` 仍用裸 `sub_id`。
3. 在 `grade_submission` 主观题分支：

```python
if question_loader.is_composite_question(q):
    raw = answers.get(qid) or {}
    if not isinstance(raw, dict):
        raw = {}
    sub_results = []
    for sub in q["sub_questions"]:
        sid = str(sub["id"])
        sub_ans = raw.get(sid, "")
        if sub_ans is None:
            sub_ans = ""
        # 合并父级 paper_id 等到子题副本
        sub_q = {**sub, "type": q.get("type"), "paper_id": q.get("paper_id")}
        sub_detail = await grade_subjective_question(sub_q, str(sub_ans))
        sub_detail["sub_question_id"] = sid
        sub_results.append(sub_detail)
    parent_score = sum(float(s.get("score") or 0) for s in sub_results)
    parent_final = sum(float(s.get("final_score", s.get("score") or 0)) for s in sub_results)
    detail = {
        "question_id": qid,
        "type": q.get("type"),
        "question": q.get("question", ""),
        "is_composite": True,
        "student_answer": raw,
        "reference_answer": {
            str(s.get("id")): s.get("answer") for s in q["sub_questions"]
        },
        "max_score": float(q.get("score") or 0),
        "score": parent_score,
        "final_score": parent_final,
        "grading_method": "composite",
        "sub_results": sub_results,
        "review_status": _aggregate_review_status([s.get("review_status") for s in sub_results]),
        "reason": "; ".join(
            f"{s.get('sub_question_id')}={s.get('score')}/{s.get('max_score')}"
            for s in sub_results
        ),
        "low_confidence": any(s.get("low_confidence") for s in sub_results),
    }
else:
    # 现有单题路径；student_answer 统一 str(...)
```

4. 实现 `_aggregate_review_status(statuses: list[str]) -> str`：

```python
_ORDER = ["low_confidence", "need_review", "pending", "high_confidence", "auto_scored", "reviewed"]
# 取列表中优先级最高者；空 → high_confidence
```

5. 单题路径继续 `str(answers.get(qid) or "")`；map 不应到达此处（submit 已拦）。

- [ ] **Step 4: 测试通过**

```bash
.venv/bin/python -m pytest tests/test_core.py -k "composite or code_language or grade" -v
```

- [ ] **Step 5: Commit**

```bash
git add backend/grader.py tests/test_core.py
git commit -m "feat(grader): score composite sub_questions and pass code_language"
```

---

### Task 4: 人工复核支持子题改分

**Files:**
- Modify: `backend/database.py`（`apply_review`）
- Modify: `backend/main.py`（Review 请求体）
- Modify: `frontend/js/detail.js`
- Test: `tests/test_core.py`

**Interfaces:**
- Consumes: `grading_detail` 含可选 `sub_results`
- Produces:
  - `apply_review(..., sub_question_id: str | None = None)`
  - API body: `{ submission_id, question_id, new_score, note, sub_question_id? }`
  - 改子题后：子题 `final_score`/`score`/`review_status=reviewed`，父题 `score`/`final_score` 重算为子题 `final_score` 之和；若全部子题 reviewed → 父 `review_status=reviewed`，否则 `need_review`

- [ ] **Step 1: 写失败测试**

```python
def test_apply_review_sub_question(tmp_path, monkeypatch):
    from backend import database
    # init isolated db, insert submission with composite grading_detail
    detail = [{
        "question_id": "c1",
        "type": "short_answer",
        "is_composite": True,
        "max_score": 10,
        "score": 5,
        "final_score": 5,
        "review_status": "need_review",
        "sub_results": [
            {"sub_question_id": "s1", "max_score": 4, "score": 2, "final_score": 2, "review_status": "need_review"},
            {"sub_question_id": "s2", "max_score": 6, "score": 3, "final_score": 3, "review_status": "high_confidence"},
        ],
    }]
    # insert row objective_score=0, subjective_score_machine=5, grading_detail_json=detail
    r = database.apply_review(
        submission_id=1,
        question_id="c1",
        new_score=4,
        note="满分口径",
        sub_question_id="s1",
    )
    assert r["success"] is True
    # reload detail: s1 final_score==4, parent final_score==7
```

- [ ] **Step 2: 运行失败**

```bash
.venv/bin/python -m pytest tests/test_core.py::test_apply_review_sub_question -v
```

- [ ] **Step 3: 实现 `apply_review` 扩展**

```python
def apply_review(
    *,
    submission_id: int,
    question_id: str,
    new_score: float,
    note: str | None,
    operator: str = "human",
    sub_question_id: str | None = None,
) -> dict[str, Any]:
    ...
    if sub_question_id:
        if not target.get("is_composite") or not isinstance(target.get("sub_results"), list):
            return {"success": False, "code": "NOT_COMPOSITE", "message": "题目不是复合题"}
        sub = next((s for s in target["sub_results"] if s.get("sub_question_id") == sub_question_id), None)
        if sub is None:
            return {"success": False, "code": "QUESTION_NOT_FOUND", "message": "未找到子题"}
        max_score = float(sub.get("max_score", 0))
        if new_score < 0 or new_score > max_score:
            return {"success": False, "code": "REVIEW_SCORE_INVALID", "message": "复核分数非法"}
        old_score = float(sub.get("final_score", sub.get("score", 0)))
        sub["score"] = new_score
        sub["final_score"] = new_score
        sub["reviewed_by"] = operator
        sub["review_note"] = note or ""
        sub["review_status"] = "reviewed"
        target["score"] = sum(float(s.get("score") or 0) for s in target["sub_results"])
        target["final_score"] = sum(
            float(s.get("final_score", s.get("score") or 0)) for s in target["sub_results"]
        )
        if all(s.get("review_status") == "reviewed" for s in target["sub_results"]):
            target["review_status"] = "reviewed"
        else:
            target["review_status"] = "need_review"
        target["reviewed_by"] = operator
        # log question_id 可用 f"{question_id}#{sub_question_id}"
    else:
        # 现有整题改分逻辑；若 is_composite，仍允许整题改父分（可选）或拒绝
        # 推荐：无 sub_question_id 时保持整题改分（兼容旧 UI），并清空/不同步 sub_results
        # 设计要求分项复核：前端始终传 sub_question_id
        ...  # 保留原逻辑
```

`main.py` Review 模型：

```python
class ReviewRequest(BaseModel):
    submission_id: int
    question_id: str
    new_score: float
    note: str | None = None
    sub_question_id: str | None = None
```

调用：

```python
database.apply_review(
    submission_id=req.submission_id,
    question_id=req.question_id,
    new_score=req.new_score,
    note=req.note,
    sub_question_id=req.sub_question_id,
)
```

- [ ] **Step 4: 更新 `detail.js` 渲染**

对 `d.is_composite && Array.isArray(d.sub_results)`：

```javascript
const subHtml = (d.sub_results || []).map(s => {
  const sid = s.sub_question_id;
  const inputId = `score_${questionId}__${sid}`;
  return `<div class="sub-result card nested">
    <h4>子题 ${esc(sid)}. ${esc(s.question || '')}</h4>
    <p class="muted">满分 ${esc(s.max_score)}，机器分 ${esc(s.score)}，最终分 <b>${esc(s.final_score ?? s.score)}</b> ${badge(s.review_status)}</p>
    <div class="answer-box"><b>学生：</b><br>${esc(s.student_answer ?? '')}</div>
    <div class="answer-box"><b>参考：</b><br>${esc(s.reference_answer ?? '')}</div>
    ${s.reason ? `<p><b>理由：</b>${esc(s.reason)}</p>` : ''}
    <div class="toolbar">
      <input class="score-input" type="number" id="${esc(inputId)}" value="${esc(s.final_score ?? s.score)}">
      <input class="note-input" type="text" id="note_${esc(questionId)}__${esc(sid)}" placeholder="子题复核备注">
      <button class="btn review-btn" data-qid="${esc(questionId)}" data-sid="${esc(sid)}">保存子题复核</button>
    </div>
  </div>`;
}).join('');
```

`review` 函数读 `data-sid`，body 带 `sub_question_id`。

父卡仍显示合计分与总状态。

- [ ] **Step 5: 测试 + 手动烟雾**

```bash
.venv/bin/python -m pytest tests/test_core.py::test_apply_review_sub_question -v
```

- [ ] **Step 6: Commit**

```bash
git add backend/database.py backend/main.py frontend/js/detail.js tests/test_core.py
git commit -m "feat(review): support per-subquestion human review for composite items"
```

---

### Task 5: 导出 Excel 展示复合题分项

**Files:**
- Modify: `backend/exporter.py`
- Test: `tests/test_core.py` 或新建 `tests/test_exporter.py`

**Interfaces:**
- Consumes: `grading_detail` 的 `is_composite` / `sub_results`
- Produces: 导出单元格中学生答案、得分说明含子题分行

- [ ] **Step 1: 写失败测试**

```python
def test_format_answer_for_export_composite():
    from backend.exporter import format_student_answer_for_export  # 若无则新增 helper

    d = {
        "is_composite": True,
        "sub_results": [
            {"sub_question_id": "s1", "student_answer": "答1", "final_score": 2, "max_score": 4},
            {"sub_question_id": "s2", "student_answer": "答2", "final_score": 6, "max_score": 6},
        ],
        "student_answer": {"s1": "答1", "s2": "答2"},
    }
    text = format_student_answer_for_export(d)
    assert "s1" in text and "答1" in text
    assert "s2" in text
```

- [ ] **Step 2: 运行失败**

```bash
.venv/bin/python -m pytest tests/test_core.py::test_format_answer_for_export_composite -v
```

- [ ] **Step 3: 实现**

在 `exporter.py`：

```python
def format_student_answer_for_export(detail: dict[str, Any]) -> str:
    if detail.get("is_composite") and isinstance(detail.get("sub_results"), list):
        lines = []
        for s in detail["sub_results"]:
            sid = s.get("sub_question_id", "?")
            ans = s.get("student_answer", "")
            lines.append(f"[{sid}] {ans}")
        return "\n".join(lines)
    ans = detail.get("student_answer", "")
    if isinstance(ans, list):
        return ",".join(str(x) for x in ans)
    if isinstance(ans, dict):
        return json.dumps(ans, ensure_ascii=False)
    return str(ans or "")


def format_score_note_for_export(detail: dict[str, Any]) -> str:
    if detail.get("is_composite") and isinstance(detail.get("sub_results"), list):
        parts = [
            f"{s.get('sub_question_id')}={s.get('final_score', s.get('score'))}/{s.get('max_score')}"
            for s in detail["sub_results"]
        ]
        return "复合题: " + "; ".join(parts)
    return str(detail.get("reason") or "")
```

在写 Excel 行时改用上述 helper（定位现有写入 `student_answer` 的循环并替换）。

- [ ] **Step 4: 测试通过 + Commit**

```bash
.venv/bin/python -m pytest tests/test_core.py::test_format_answer_for_export_composite -v
git add backend/exporter.py tests/test_core.py
git commit -m "feat(export): render composite sub-question answers and scores"
```

---

### Task 6: 管理端试卷编辑器 — 子题 CRUD

**Files:**
- Modify: `frontend/admin.html`
- Modify: `frontend/js/papers.js`

**Interfaces:**
- Consumes: 题目对象可选 `sub_questions: Array<sub>`
- Produces: 保存试卷时复合题 payload 符合 Task 1 校验

- [ ] **Step 1: 扩展 HTML 表单**

在 `admin.html` 的 `#qSubjectiveBlock` 内：

1. `qScoringMode` 增加 `<option value="calculation">计算题</option>`（若尚未有）
2. 增加子题区块：

```html
<div id="qSubQuestionsBlock" class="q-block">
  <div class="section-head">
    <h3>子题（复合题，可选）</h3>
    <button class="btn secondary" id="addSubQuestionBtn" type="button">加子题</button>
  </div>
  <p class="muted form-note">添加子题后，父题参考答案可留空；父分值必须等于子题分值之和（保存时自动汇总）。</p>
  <div id="qSubQuestionsList"></div>
</div>
```

- [ ] **Step 2: `papers.js` 读写**

增加：

```javascript
function isCompositeFromUI() {
  return document.querySelectorAll('#qSubQuestionsList .sub-q-row').length > 0;
}

function renderSubQuestionsEditor(subs) {
  const list = $('qSubQuestionsList');
  const items = Array.isArray(subs) ? subs : [];
  list.innerHTML = items.map((s, i) => `
    <div class="sub-q-row panel nested-panel" data-i="${i}">
      <div class="form-grid">
        <label>子题 ID <input class="sq-id" value="${esc(s.id || `s${i+1}`)}"></label>
        <label>分值 <input class="sq-score" type="number" min="0.5" step="0.5" value="${esc(s.score ?? 1)}"></label>
        <label>评分模式
          <select class="sq-mode">
            <option value="text" ${s.scoring_mode==='text'?'selected':''}>文本</option>
            <option value="sql" ${s.scoring_mode==='sql'?'selected':''}>SQL</option>
            <option value="code" ${s.scoring_mode==='code'?'selected':''}>代码</option>
            <option value="calculation" ${s.scoring_mode==='calculation'?'selected':''}>计算</option>
          </select>
        </label>
        <label>代码语言 <input class="sq-lang" value="${esc(s.code_language || '')}" placeholder="python / sql"></label>
      </div>
      <label class="full-label">子题题干 <textarea class="sq-stem" rows="2">${esc(s.question || '')}</textarea></label>
      <label class="full-label">子题参考答案 <textarea class="sq-answer code-answer" rows="3">${esc(s.answer || '')}</textarea></label>
      <button class="btn danger sq-del" type="button" data-i="${i}">删除子题</button>
    </div>
  `).join('') || '<p class="muted">无子题（单题模式）</p>';
}

function collectSubQuestionsFromUI() {
  return [...document.querySelectorAll('#qSubQuestionsList .sub-q-row')].map((row, i) => {
    const mode = row.querySelector('.sq-mode').value || 'text';
    const item = {
      id: row.querySelector('.sq-id').value.trim() || `s${i+1}`,
      question: row.querySelector('.sq-stem').value.trim(),
      answer: row.querySelector('.sq-answer').value,
      score: Number(row.querySelector('.sq-score').value) || 0,
      scoring_mode: mode,
    };
    const lang = row.querySelector('.sq-lang').value.trim();
    if (lang) item.code_language = lang;
    return item;
  }).filter(s => s.question);
}
```

修改 `fillQuestionForm` / `showQuestionForm`：调用 `renderSubQuestionsEditor(q.sub_questions || [])`。

修改 `applyQuestionFromForm` 主观题分支：

```javascript
const subs = collectSubQuestionsFromUI();
if (subs.length) {
  // 复合题
  const ids = new Set();
  for (const s of subs) {
    if (!s.answer || !String(s.answer).trim()) { toast(`子题 ${s.id} 需要参考答案`); return; }
    if (!(s.score > 0)) { toast(`子题 ${s.id} 分值须 > 0`); return; }
    if (ids.has(s.id)) { toast(`子题 ID 重复: ${s.id}`); return; }
    ids.add(s.id);
    if (s.scoring_mode === 'code' && !s.code_language) {
      toast(`子题 ${s.id} 代码模式必须填写语言`); return;
    }
  }
  q.sub_questions = subs;
  q.score = subs.reduce((a, s) => a + (Number(s.score) || 0), 0);
  $('qScore').value = q.score;
  // 不设置 q.answer 或设为空
} else {
  // 现有单题逻辑：answer + scoring_mode + code_language + points
  ...
}
```

`addSubQuestionBtn`：在列表后追加空子题并 `renderSubQuestionsEditor`。

列表展示 `renderQuestionList`：若有 `sub_questions`，副标题显示 `复合题 · N 子题`。

- [ ] **Step 3: 浏览器手测清单（无自动化则手动）**

1. 新建复合题 2 子题，父分自动 10，保存成功  
2. `code` 子题不填语言 → 前端 toast 或后端 400  
3. 重开编辑器字段回显正确  

- [ ] **Step 4: Commit**

```bash
git add frontend/admin.html frontend/js/papers.js
git commit -m "feat(admin): edit composite sub_questions in paper editor"
```

---

### Task 7: 考试端复合题作答 UI

**Files:**
- Modify: `frontend/js/exam.js`
- Optional CSS: `frontend/css/*.css`（仅当需要间距）

**Interfaces:**
- Consumes: `question.sub_questions[]` from `/api/exam`
- Produces: `answers[parentId] = { subId: text, ... }`

- [ ] **Step 1: 改 `renderQuestions` 主观题分支**

```javascript
} else {
  const subs = Array.isArray(q.sub_questions) ? q.sub_questions : [];
  if (subs.length) {
    const blocks = subs.map(s => {
      const sid = s.id;
      const val = (answers[q.id] && answers[q.id][sid]) || '';
      return `<div class="sub-answer">
        <label class="sub-label"><b>(${esc(sid)})</b> ${esc(s.question || '')}
          <span class="muted">（${esc(s.score)} 分）</span>
        </label>
        <textarea class="answer-input code-answer" rows="6"
          data-qid="${esc(q.id)}" data-sid="${esc(sid)}"
          placeholder="输入子题答案">${esc(val)}</textarea>
      </div>`;
    }).join('');
    body = `<div class="composite-answers">${blocks}</div>`;
  } else {
    body = `<textarea class="answer-input code-answer" rows="8" data-qid="${esc(q.id)}"
      placeholder="输入答案">${esc(answers[q.id] || '')}</textarea>`;
  }
}
```

- [ ] **Step 2: 改事件委托与 `collectAnswers`**

```javascript
// input/change 监听
if (t.classList.contains('answer-input')) {
  const qid = t.dataset.qid;
  const sid = t.dataset.sid;
  if (sid) {
    if (!answers[qid] || typeof answers[qid] !== 'object' || Array.isArray(answers[qid])) {
      answers[qid] = {};
    }
    answers[qid][sid] = t.value;
  } else {
    answers[qid] = t.value;
  }
  // 答题卡状态：复合题任一子题非空即 answered
}
```

`collectAnswers` 对复合题保留 object；提交前可选校验每个 sid 都有键（空字符串允许，但键需齐全）：

```javascript
function ensureCompositeKeys(questions, answers) {
  for (const q of questions) {
    const subs = q.sub_questions || [];
    if (!subs.length) continue;
    const m = (answers[q.id] && typeof answers[q.id] === 'object') ? answers[q.id] : {};
    const filled = {};
    for (const s of subs) filled[s.id] = m[s.id] != null ? String(m[s.id]) : '';
    answers[q.id] = filled;
  }
}
```

- [ ] **Step 3: 手测**

1. 打开含复合题试卷，见多个 textarea  
2. 填写后 localStorage 恢复 map  
3. 提交网络面板 answers 为对象  

- [ ] **Step 4: Commit**

```bash
git add frontend/js/exam.js
git commit -m "feat(exam): multi-textarea answers for composite questions"
```

---

### Task 8: 样例试卷 + 端到端回归

**Files:**
- Create or Modify: `data/papers/` 下某一非生产样例或测试夹具 JSON（若仓库禁止改正式卷，则仅测试内联 JSON）
- Test: `tests/test_papers.py` 端到端

- [ ] **Step 1: 端到端测试**

```python
def test_e2e_composite_submit_and_detail_shape(papers_env, monkeypatch):
    """
    1. 写入含复合题的 paper 并 open
    2. start + submit map 答案
    3. mock subjective score 快速完成（或 sync grade）
    4. GET admin submission detail → is_composite / sub_results
    """
```

对 LLM 评分：在测试中 `monkeypatch` `grader.grade_subjective_question` 或 `score_async` 返回固定分，避免外网。

- [ ] **Step 2: 全量相关测试**

```bash
.venv/bin/python -m pytest tests/test_papers.py tests/test_core.py -v --tb=short
```

Expected: 全部 PASS

- [ ] **Step 3: Commit**

```bash
git add tests/ data/papers/  # 仅当确有样例变更
git commit -m "test: e2e coverage for composite sub_questions grading path"
```

---

### Task 9: 文档对齐与自检

**Files:**
- Modify: `docs/superpowers/specs/2026-07-18-composite-subquestions-and-code-language-design.md` 仅当实现与设计有意偏差时追加 “Implementation Notes”
- 不强制改用户文档

- [ ] **Step 1: 对照设计验收清单**

| 设计要求 | 对应 Task |
|---------|-----------|
| sub_questions schema / 分值之和 | Task 1 |
| 答案 map / 422 | Task 2 |
| 子题独立评分 + 父分汇总 | Task 3 |
| code_language → ScoringRequest | Task 3 |
| 管理端编辑子题 | Task 6 |
| 考试端多框作答 | Task 7 |
| 复核子题 | Task 4 |
| 导出分项 | Task 5 |
| 旧题兼容 | Task 1/3 单题路径不变 |

- [ ] **Step 2: 最终回归**

```bash
.venv/bin/python -m pytest tests/ -v --tb=line
```

- [ ] **Step 3: Commit（若有文档）**

```bash
git add docs/superpowers/specs/2026-07-18-composite-subquestions-and-code-language-design.md
git commit -m "docs: note composite implementation status"
```

---

## Self-Review

1. **Spec coverage:** 复合结构、答案形状、评分、语言透传、编辑器、考试端、复核、导出、兼容均有 Task；无 DB migration 符合设计。  
2. **Placeholder scan:** 测试与核心实现片段已给出；E2E 需对接项目既有 paper open helper（实现时搜索 `assert_paper_open` / `exam/start` 测试用法补全，避免空步骤）。  
3. **Type consistency:**  
   - `sub_question_id` 贯穿 detail / review API / frontend `data-sid`  
   - `is_composite: bool` + `sub_results: list`  
   - `validate_answer_shape` / `is_composite_question` 名称在 loader 与 grader/main 共用  

---

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-07-18-composite-subquestions-and-code-language.md`.

**Two execution options:**

1. **Subagent-Driven (recommended)** — 每个 Task 新开 subagent，Task 间审查，迭代快  
2. **Inline Execution** — 本会话用 executing-plans 按 Task 批量执行并设检查点  

**Which approach?**
