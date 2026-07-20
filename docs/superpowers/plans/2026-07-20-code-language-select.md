# 顶层代码题语言选择 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 顶层代码题支持编程语言下拉，并按学生所选语言匹配参考答案与评分。

**Architecture:** 复用复合题已有模式：`allowed_languages` 控制下拉；提交 `{answer, language}`；`normalize_submitted_answer` 校验语言；评分前用 `answers_by_language` / `scoring_points_by_language` 覆盖默认参考。

**Tech Stack:** Vanilla JS (frontend/js/exam.js)、FastAPI/Python (backend/grader.py, question_loader.py)、pytest。

## Global Constraints

- 不改复合题既有行为。
- 无 `allowed_languages` 的旧代码题保持字符串答案兼容。
- 敏感字段不得下发学生端。

---

### Task 1: question_loader 规范化与脱敏

**Files:**
- Modify: `backend/question_loader.py`
- Test: `tests/test_papers.py`

- [x] 规范化顶层/子题 `answers_by_language`、`scoring_points_by_language`
- [x] `sanitize_for_student` 剥离上述字段
- [x] 测试通过

### Task 2: grader 顶层 code 题语言解析与参考覆盖

**Files:**
- Modify: `backend/grader.py`
- Test: `tests/test_core.py`

- [x] `normalize_submitted_answer` 支持顶层 code + `{answer, language}`
- [x] 评分前 `apply_language_reference(question, language)` 覆盖 answer/scoring_points/code_language
- [x] 测试：选 js 时用 js 参考；非法语言拒绝

### Task 3: 前端顶层语言下拉

**Files:**
- Modify: `frontend/js/exam.js`
- Test: `tests/test_frontend_static.py`

- [x] `renderQuestion` 渲染语言 select
- [x] `collectAnswers` 提交 `{answer, language}`
- [x] 静态断言

### Task 4: 试卷数据 q16/q21

**Files:**
- Modify: `data/papers/software-development.json`

- [x] q16/q21 增加 `allowed_languages` 与 `answers_by_language`
- [x] q21 改为 `scoring_mode: code`

### Task 5: 回归

- [x] `pytest tests/test_papers.py tests/test_core.py tests/test_frontend_static.py tests/test_submission_auto_submit.py -q` → 100 passed
- [x] commit
