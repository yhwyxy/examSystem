# 切屏自动交卷（方案 A）Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在第 3 次页面失焦或任意一次失焦满 30 秒时自动提交当前答案并正常判分，仅在该成绩记录中显示自动交卷原因。

**Architecture:** 浏览器端在 `frontend/js/exam.js` 中维护非持久化的失焦次数和单次 30 秒计时，并复用现有 `submitExam` 答案收集与提交路径。`backend/main.py` 只接受白名单内的自动交卷原因，`backend/database.py` 在提交记录中持久化该可选字段；管理端从列表响应读取字段，仅在有值的行展示状态。

**Tech Stack:** Python 3、FastAPI、Pydantic、SQLite、原生 JavaScript、HTML/CSS、pytest。

## Global Constraints

- 自动交卷原因只能是 `third_blur` 或 `blur_timeout_30s`；其他值不得持久化。
- 第 3 次有效页面离开在失焦回调中立即自动交卷。
- 任意单次连续页面离开达到 `30_000` 毫秒（含）自动交卷。
- 普通交卷及未达到阈值的切屏不传输、不保存、不显示切屏数据。
- 自动交卷必须使用既有服务端判分流程，不能接受浏览器传入的分数。
- 同一次离开触发的 `visibilitychange` 与 `blur` 必须只计数一次；自动交卷请求必须至多一次。
- 不增加切屏事件审计、心跳、草稿表或额外前端依赖。

---

## File Structure

- `backend/database.py` — 为既有 `submissions` 表添加可空的 `auto_submit_reason` 列，并在写入/查询提交记录时处理它。
- `backend/main.py` — 在 `SubmitRequest` 中声明可选原因，对提交请求白名单校验，并将其传递至数据库写入。
- `frontend/js/exam.js` — 在现有考试生命周期和 `submitExam` 中加入去重的离开检测、30 秒定时与自动提交调用。
- `frontend/admin.html` — 在提交记录表添加“交卷状态”表头。
- `frontend/js/admin.js` — 在成绩行中仅为自动交卷记录渲染固定中文标签。
- `frontend/css/style.css` — 为新的自动交卷状态标签补充可读的警示样式（若现有 `.badge-*` 不能复用）。
- `tests/test_submission_auto_submit.py` — 覆盖请求模型、原因校验、数据库读写和列表响应的后端行为。
- `tests/test_frontend_static.py` — 静态断言考试脚本的检测/去重/提交契约和管理端的条件展示契约。

## Task 1: 持久化可选自动交卷原因

**Files:**
- Modify: `backend/database.py`
- Create: `tests/test_submission_auto_submit.py`

**Interfaces:**
- Produces: `insert_submission_pending(..., auto_submit_reason: str | None = None) -> int`。
- Produces: `list_submissions()` 返回的每个字典可包含 `auto_submit_reason`，普通记录为 `None`。
- Consumes: 现有 SQLite 初始化/迁移模式和提交记录字段。

- [ ] **Step 1: 编写数据库读写失败测试**

在 `tests/test_submission_auto_submit.py` 中，使用 `tmp_path` 与 `monkeypatch` 指向临时 SQLite 文件，调用真实 `init_db()`。按项目现有 `database.py` 的连接配置调用 `insert_submission_pending`，并确认 `list_submissions()`：

```python
from backend import database


def test_submission_persists_auto_submit_reason(tmp_path, monkeypatch):
    monkeypatch.setattr(database, "DB_PATH", tmp_path / "exam.db")
    database.init_db()

    submission_id = database.insert_submission_pending(
        student_name="张三",
        paper_id="paper-1",
        answers={"q1": "A"},
        auto_submit_reason="third_blur",
    )

    rows = database.list_submissions()
    row = next(item for item in rows if item["id"] == submission_id)
    assert row["auto_submit_reason"] == "third_blur"


def test_normal_submission_has_no_auto_submit_reason(tmp_path, monkeypatch):
    monkeypatch.setattr(database, "DB_PATH", tmp_path / "exam.db")
    database.init_db()

    submission_id = database.insert_submission_pending(
        student_name="李四", paper_id="paper-1", answers={"q1": "A"}
    )

    row = next(item for item in database.list_submissions() if item["id"] == submission_id)
    assert row["auto_submit_reason"] is None
```

- [ ] **Step 2: 运行测试，确认失败**

Run: `PYTHONPATH=. pytest tests/test_submission_auto_submit.py -q`

Expected: FAIL，因为 `insert_submission_pending()` 尚不接受 `auto_submit_reason`，且表/查询尚无此列。

- [ ] **Step 3: 实现最小数据库迁移与读写**

在 `backend/database.py` 中：

1. 在新库 `CREATE TABLE submissions` 语句加入：

```sql
auto_submit_reason TEXT
```

2. 在 `init_db()` 已有的兼容迁移区域，读取 `PRAGMA table_info(submissions)`；若列名集合不含 `auto_submit_reason`，执行：

```python
conn.execute("ALTER TABLE submissions ADD COLUMN auto_submit_reason TEXT")
```

3. 扩展现有函数签名，最后一个参数保持默认值以避免影响既有调用：

```python
def insert_submission_pending(
    student_name: str,
    paper_id: str,
    answers: dict,
    *,
    auto_submit_reason: str | None = None,
) -> int:
```

4. 在该函数已有的 `INSERT INTO submissions (...) VALUES (...)` 中，把 `auto_submit_reason` 加到列、占位符和参数元组；不要修改已有答案 JSON 序列化与状态默认值。
5. 在 `list_submissions()` 的现有 `SELECT` 字段中选出 `auto_submit_reason`，并在构造返回字典时添加同名键。若项目另有 `get_submission()` 或导出查询，亦选出并返回同名字段，防止详情/导出丢失该数据。

- [ ] **Step 4: 运行数据库测试，确认通过**

Run: `PYTHONPATH=. pytest tests/test_submission_auto_submit.py -q`

Expected: PASS（2 passed）。

- [ ] **Step 5: 提交数据库任务**

```bash
git add backend/database.py tests/test_submission_auto_submit.py
git commit -m "feat: persist automatic submission reasons"
```

## Task 2: 限制 API 原因并保留既有判分路径

**Files:**
- Modify: `backend/main.py`
- Modify: `tests/test_submission_auto_submit.py`

**Interfaces:**
- Consumes: `SubmitRequest.auto_submit_reason: str | None`。
- Consumes: `database.insert_submission_pending(..., auto_submit_reason=...)`。
- Produces: `/api/submit` 的自动提交与手动提交均进入同一 `grade_submission(request.answers, request.paper_id)` 路径。

- [ ] **Step 1: 编写 API 校验和传递失败测试**

向 `tests/test_submission_auto_submit.py` 增加直接调用路由函数所需的测试。用 `monkeypatch` 替换 `backend.main.database.insert_submission_pending`，捕获关键字参数；用最小假的 `grade_submission` 返回项目当前路由期望的评分结果。断言合法原因被透传、缺省值为 `None`，非法原因被 Pydantic 拒绝：

```python
import pytest
from pydantic import ValidationError
from backend.main import SubmitRequest


def test_submit_request_accepts_only_known_auto_submit_reasons():
    assert SubmitRequest(
        student_name="张三",
        paper_id="paper-1",
        answers={"q1": "A"},
        auto_submit_reason="third_blur",
    ).auto_submit_reason == "third_blur"

    with pytest.raises(ValidationError):
        SubmitRequest(
            student_name="张三",
            paper_id="paper-1",
            answers={"q1": "A"},
            auto_submit_reason="client_supplied_score",
        )
```

再针对项目现有 `submit` 路由函数，以其实际参数（包括 `Request`）调用，断言捕获到：

```python
assert captured["auto_submit_reason"] == "blur_timeout_30s"
```

同时写一例没有该字段的请求，断言 `captured["auto_submit_reason"] is None`。两例都断言 mock 的评分函数调用了一次且参数仅为服务端需要的 `answers` 和 `paper_id`，证明浏览器原因不会绕过判分。

- [ ] **Step 2: 运行测试，确认失败**

Run: `PYTHONPATH=. pytest tests/test_submission_auto_submit.py -q`

Expected: FAIL，因为 `SubmitRequest` 还没有字段或没有 `Literal` 白名单，路由也没有传参。

- [ ] **Step 3: 实现请求模型和路由传递**

在 `backend/main.py`：

1. 从 `typing` 导入 `Literal`（若文件已有其他 typing 导入则合并）。
2. 定义常量，供路由/展示相关代码复用：

```python
AUTO_SUBMIT_REASONS = {"third_blur", "blur_timeout_30s"}
```

3. 在 `SubmitRequest` 中添加默认可空、白名单类型的字段：

```python
auto_submit_reason: Literal["third_blur", "blur_timeout_30s"] | None = None
```

4. 在现有 `/api/submit` 路由对 `database.insert_submission_pending(...)` 的调用中增加：

```python
auto_submit_reason=request.auto_submit_reason,
```

5. 保持现有 `grade_submission(request.answers, request.paper_id)` 调用和所有评分结果写入不变；不得增加来自请求的 `score`、`grading_detail` 等字段。

- [ ] **Step 4: 运行后端定向测试，确认通过**

Run: `PYTHONPATH=. pytest tests/test_submission_auto_submit.py tests/test_core.py -q`

Expected: PASS；自动原因只作为提交元数据，原有判分测试继续通过。

- [ ] **Step 5: 提交 API 任务**

```bash
git add backend/main.py tests/test_submission_auto_submit.py
git commit -m "feat: accept validated automatic submit reasons"
```

## Task 3: 前端切屏检测与一次性自动交卷

**Files:**
- Modify: `frontend/js/exam.js`
- Modify: `tests/test_frontend_static.py`

**Interfaces:**
- Produces: `submitExam(autoSubmitReason = null)`；手动点击仍调用 `submitExam()`。
- Produces: `startAwayTimer()`、`handlePageAway()`、`handlePageReturn()` 和 `setupAntiSwitchAutoSubmit()`，全部仅在考试已经成功加载后安装。
- Consumes: 现有答案收集、学生姓名、`currentPaper`、提交按钮、`/api/submit` 请求和成功面板。

- [ ] **Step 1: 编写前端静态契约失败测试**

在 `tests/test_frontend_static.py` 增加如下检查，使用现有 `read_frontend_file()`：

```python
def test_exam_script_implements_deduplicated_anti_switch_auto_submit():
    source = read_frontend_file("frontend/js/exam.js")

    assert "const AUTO_SUBMIT_AFTER_BLURS = 3" in source
    assert "const AWAY_TIMEOUT_MS = 30_000" in source
    assert "let blurCount = 0" in source
    assert "let isPageAway = false" in source
    assert "let autoSubmitStarted = false" in source
    assert "function handlePageAway" in source
    assert "function handlePageReturn" in source
    assert "document.addEventListener('visibilitychange'" in source
    assert "window.addEventListener('blur'" in source
    assert "window.addEventListener('focus'" in source
    assert "submitExam('third_blur')" in source
    assert "submitExam('blur_timeout_30s')" in source


def test_exam_submission_sends_reason_only_for_auto_submit():
    source = read_frontend_file("frontend/js/exam.js")
    submit_body = function_body(source, "async function submitExam", "function ")

    assert "async function submitExam(autoSubmitReason = null)" in source
    assert "...(autoSubmitReason ? { auto_submit_reason: autoSubmitReason } : {})" in submit_body
    assert "autoSubmitStarted" in submit_body
```

若 `function_body()` 的结束标记不适用于当前 `exam.js` 的函数顺序，使用紧邻 `submitExam` 后的真实函数名作结束标记，避免测试误截取。

- [ ] **Step 2: 运行静态测试，确认失败**

Run: `PYTHONPATH=. pytest tests/test_frontend_static.py -q`

Expected: FAIL，因为防切屏状态、监听和自动原因尚不存在。

- [ ] **Step 3: 实现防切屏状态和提交契约**

在 `frontend/js/exam.js` 的模块状态区新增：

```javascript
const AUTO_SUBMIT_AFTER_BLURS = 3;
const AWAY_TIMEOUT_MS = 30_000;
let blurCount = 0;
let isPageAway = false;
let awayTimeoutId = null;
let autoSubmitStarted = false;
```

将现有函数签名改为：

```javascript
async function submitExam(autoSubmitReason = null) {
```

在函数开头实现一次性锁：自动提交已经开始时直接返回；自动原因存在时先设置 `autoSubmitStarted = true`。保留手动提交的现有确认弹窗；自动提交必须跳过确认弹窗。将构造请求体替换为（合并现有 `student_name`、`paper_id`、`answers`）：

```javascript
const payload = {
  student_name: studentName,
  paper_id: currentPaper.slug,
  answers: collectAnswers(),
  ...(autoSubmitReason ? { auto_submit_reason: autoSubmitReason } : {}),
};
```

在自动提交开始时禁用现有提交按钮、清理 `awayTimeoutId` 并以现有页面提示机制显示“检测到切屏，正在自动交卷…”。若请求失败，解除 `autoSubmitStarted`、恢复按钮和现有错误提示，使考生可继续手动提交；不要把失败自动提交伪装为成功。

- [ ] **Step 4: 实现离开/返回检测与安装时机**

在 `exam.js` 中新增：

```javascript
function clearAwayTimer() {
  if (awayTimeoutId !== null) {
    window.clearTimeout(awayTimeoutId);
    awayTimeoutId = null;
  }
}

function startAwayTimer() {
  clearAwayTimer();
  awayTimeoutId = window.setTimeout(() => {
    if (isPageAway && !autoSubmitStarted) {
      submitExam('blur_timeout_30s');
    }
  }, AWAY_TIMEOUT_MS);
}

function handlePageAway() {
  if (isPageAway || autoSubmitStarted) return;
  isPageAway = true;
  blurCount += 1;

  if (blurCount >= AUTO_SUBMIT_AFTER_BLURS) {
    clearAwayTimer();
    submitExam('third_blur');
    return;
  }
  startAwayTimer();
}

function handlePageReturn() {
  if (!isPageAway) return;
  isPageAway = false;
  clearAwayTimer();
}

function setupAntiSwitchAutoSubmit() {
  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState === 'hidden') handlePageAway();
    else handlePageReturn();
  });
  window.addEventListener('blur', handlePageAway);
  window.addEventListener('focus', handlePageReturn);
}
```

在现有考试数据成功加载、题目渲染、倒计时启动后调用一次 `setupAntiSwitchAutoSubmit()`。不要在页面加载失败或未进入考试状态时安装监听。保留现有倒计时到零的提交行为；由于 `autoSubmitStarted`，它不会与自动提交重复请求。

- [ ] **Step 5: 运行前端静态测试，确认通过**

Run: `PYTHONPATH=. pytest tests/test_frontend_static.py -q`

Expected: PASS，包括已有 DOM 安全和页面结构断言。

- [ ] **Step 6: 手工浏览器验证一次性与判分路径**

Run: `PYTHONPATH=. uvicorn backend.main:app --reload`

在浏览器打开一个考试页并填写至少一题：

1. 两次切换标签并各自在 30 秒内返回，随后手动交卷；确认请求负载不含 `auto_submit_reason`。
2. 再开启一场考试，第 3 次切走时在 DevTools Network 确认仅出现一次 `/api/submit`，负载含 `auto_submit_reason: "third_blur"`。
3. 再开启一场考试，切走至少 30 秒；确认仅出现一次请求，负载含 `auto_submit_reason: "blur_timeout_30s"`。
4. 确认返回页面后的失焦/聚焦组合不会将单次操作计为两次。

Expected: 所有自动提交均进入现有成功页并显示由服务端评分的分数。

- [ ] **Step 7: 提交前端检测任务**

```bash
git add frontend/js/exam.js tests/test_frontend_static.py
git commit -m "feat: auto-submit exams after prohibited page switches"
```

## Task 4: 成绩列表仅标注自动交卷记录

**Files:**
- Modify: `frontend/admin.html`
- Modify: `frontend/js/admin.js`
- Modify: `frontend/css/style.css`
- Modify: `tests/test_frontend_static.py`

**Interfaces:**
- Consumes: 成绩列表记录的可空 `auto_submit_reason`。
- Produces: `formatAutoSubmitReason(reason)`，仅将 `third_blur` 和 `blur_timeout_30s` 映射到批准的中文文案。
- Produces: 管理端每一行始终有交卷状态单元格；普通记录该单元格为空，绝不显示“正常”或切屏信息。

- [ ] **Step 1: 编写列表条件展示失败测试**

在 `tests/test_frontend_static.py` 中增加：

```python
def test_admin_shows_auto_submit_reason_only_when_present():
    html = read_frontend_file("frontend/admin.html")
    source = read_frontend_file("frontend/js/admin.js")

    assert "交卷状态" in html
    assert "function formatAutoSubmitReason" in source
    assert "切屏达到 3 次，自动交卷" in source
    assert "单次切屏达到 30 秒，自动交卷" in source
    assert "submission.auto_submit_reason" in source
    assert "return ''" in source
```

- [ ] **Step 2: 运行测试，确认失败**

Run: `PYTHONPATH=. pytest tests/test_frontend_static.py::test_admin_shows_auto_submit_reason_only_when_present -q`

Expected: FAIL，因为表头、格式化函数和条件渲染尚不存在。

- [ ] **Step 3: 实现表头、映射和条件单元格**

1. 在 `frontend/admin.html` 中，提交记录表现有的得分/状态/时间列附近新增：

```html
<th>交卷状态</th>
```

2. 在 `frontend/js/admin.js` 的提交列表相关函数之前新增：

```javascript
function formatAutoSubmitReason(reason) {
  if (reason === 'third_blur') return '切屏达到 3 次，自动交卷';
  if (reason === 'blur_timeout_30s') return '单次切屏达到 30 秒，自动交卷';
  return '';
}
```

3. 在生成每个成绩行的现有 DOM 创建逻辑中，以 `formatAutoSubmitReason(submission.auto_submit_reason)` 得到文本；新增一个 `td`。仅当文本非空时，创建/附加标签元素，标签文本为该函数返回值，并添加新样式类 `badge-auto-submit`。文本为空时保留空单元格，不能渲染“正常”、`-`、切屏次数或原因。
4. 保持表头与每行单元格数一致；若空状态列表有 `colspan`，按新增列数加一。

- [ ] **Step 4: 添加标签样式**

在 `frontend/css/style.css` 与既有 `.badge-*` 样式相邻处加入：

```css
.badge-auto-submit {
  color: #9a3412;
  background: #ffedd5;
  border: 1px solid #fdba74;
}
```

若项目已有深色模式或 CSS 变量，使用对应变量表达同等的橙色警示对比度，而不是引入未使用的独立样式体系。

- [ ] **Step 5: 运行前端测试，确认通过**

Run: `PYTHONPATH=. pytest tests/test_frontend_static.py -q`

Expected: PASS，且既有 `test_admin_badge_classes_are_defined_in_stylesheet` 也通过。

- [ ] **Step 6: 手工验证成绩列表**

使用 Task 3 的三个提交记录刷新管理端成绩列表：

- 正常提交记录：交卷状态单元格为空，不显示任何切屏信息。
- `third_blur` 记录：仅显示“切屏达到 3 次，自动交卷”。
- `blur_timeout_30s` 记录：仅显示“单次切屏达到 30 秒，自动交卷”。

Expected: 分数、阅卷状态和其他既有列不受影响。

- [ ] **Step 7: 提交列表任务**

```bash
git add frontend/admin.html frontend/js/admin.js frontend/css/style.css tests/test_frontend_static.py
git commit -m "feat: label automatic submissions in results"
```

## Task 5: 全量回归与交付验证

**Files:**
- Modify: 无（仅在必要时修复前述文件的测试发现）。

**Interfaces:**
- Consumes: Tasks 1–4 的全部实现。
- Produces: 已验证的功能分支，无新增未跟踪文件被纳入提交。

- [ ] **Step 1: 使用项目正确导入路径运行全部测试**

先从 `pyproject.toml`、`pytest.ini`、`tox.ini` 或项目 README 确认测试所需的安装命令和包布局。若项目使用 `src/` 布局，先执行项目定义的 editable install，例如：

```bash
python -m pip install -e .
pytest -q
```

若未定义安装配置但模块目录位于项目根，执行：

```bash
PYTHONPATH=. pytest -q
```

Expected: 所有测试通过。记录实际通过的命令与结果。

- [ ] **Step 2: 处理基线环境问题，不掩盖失败**

若测试在收集阶段因 `ModuleNotFoundError: backend`、`subjective_scoring` 或 `scripts` 失败，确认其为项目包未安装/`PYTHONPATH` 配置问题；按项目元数据安装后重试。不得通过删除测试、修改测试导入、或跳过新增测试来使其绿灯。若依赖不可安装，报告完整命令、环境错误和已通过的定向测试。

- [ ] **Step 3: 检查差异与仓库状态**

```bash
git diff main...HEAD --check
git status --short
git log --oneline main..HEAD
```

Expected: 无空白错误；仅包含本功能提交和用户原先已存在、未被 `git add` 的未跟踪文件。

- [ ] **Step 4: 修复验证发现的问题并提交（仅在有修复时）**

若回归或手工验证发现问题，先为该问题添加失败测试，再进行最小修复，运行相关测试和全量测试，最后：

```bash
git add backend frontend tests
git commit -m "fix: harden automatic submission flow"
```

若没有问题，不创建空提交。

## Plan Self-Review

- **规格覆盖：** Task 1/2 实现“仅自动原因持久化”和服务端正常评分；Task 3 覆盖第 3 次即时触发、30 秒含阈值、去重、一次提交及不保存短时切屏；Task 4 覆盖仅自动记录显示中文原因；Task 5 覆盖完整回归和仓库卫生。
- **占位符：** 无 TBD/TODO、模糊实现步骤或未定义的函数名称；动态数据库函数以现有名称为准，并要求实施者沿用现有连接/迁移模式。
- **类型一致性：** 前端仅发送 `auto_submit_reason`；请求模型、数据库参数与列表 JSON 使用同一 snake_case 字段；允许值在前后端完全一致。
