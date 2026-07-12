# subjective-scoring v0.1.2 专项试卷实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 将评分依赖升级到 v0.1.2，新增文本、SQL、代码三套 100 分专项卷，并生成可审阅的评分效果报告。

**Architecture:** 依赖版本在项目声明、锁文件、文档和边界测试中保持一致。专项卷使用现有 `data/papers/*.json` 格式与 `paper_store` 索引，不新增运行时代码；验证脚本通过现有评分入口对三档答案进行真实评分。

**Tech Stack:** Python 3.13、uv、pytest、FastAPI、SQLite、subjective-scoring v0.1.2、JSON、Markdown

## Global Constraints

- `subjective-scoring` 必须固定到公开 GitHub tag `v0.1.2`。
- 每套专项卷包含 5 道主观题，每题 20 分，总分 100 分，及格线 60 分。
- 新增试卷默认状态为 `closed`。
- 不修改评分算法、不新增题型、不自动开放试卷。
- 远程评分不可用时必须报告真实错误或降级状态。

---

### Task 1: 固定 v0.1.2 依赖

**Files:**
- Modify: `tests/test_dependency_boundaries.py`
- Modify: `pyproject.toml`
- Modify: `requirements.txt`
- Modify: `README.md`
- Modify: `uv.lock`

**Interfaces:**
- Consumes: GitHub tag `v0.1.2`
- Produces: 所有安装入口一致解析到 `subjective-scoring` v0.1.2

- [ ] **Step 1: 将依赖边界测试预期改为 v0.1.2**

把 uv source 断言和 requirements URL 断言中的 `v0.1.1` 改为 `v0.1.2`。

- [ ] **Step 2: 运行测试确认失败**

Run: `env PYTHONPATH=. uv run pytest tests/test_dependency_boundaries.py -q`
Expected: FAIL，指出 `pyproject.toml` 或 `requirements.txt` 仍为 `v0.1.1`。

- [ ] **Step 3: 更新声明和文档**

将 `pyproject.toml`、`requirements.txt`、README 安装示例中的 tag 统一改为 `v0.1.2`。

- [ ] **Step 4: 重新生成锁文件并同步环境**

Run: `uv lock`
Expected: `uv.lock` 中 source query 与 commit revision 对应 `v0.1.2`。

Run: `uv sync --extra scoring`
Expected: 环境成功安装 v0.1.2 及项目评分依赖。

- [ ] **Step 5: 验证边界测试通过**

Run: `env PYTHONPATH=. uv run pytest tests/test_dependency_boundaries.py -q`
Expected: PASS。

### Task 2: 定义三套专项卷的结构测试

**Files:**
- Create: `tests/test_specialized_papers.py`
- Create: `data/papers/text-scoring-specialist.json`
- Create: `data/papers/sql-scoring-specialist.json`
- Create: `data/papers/code-scoring-specialist.json`
- Modify: `data/papers/index.json`

**Interfaces:**
- Consumes: `backend.question_loader.validate_questions(data: dict) -> None`
- Produces: 三个合法试卷 JSON 与三个关闭状态索引项

- [ ] **Step 1: 编写失败测试**

测试逐个加载三个固定 slug，断言文件存在、`paper_id` 匹配、共 5 题、总分 100、及格线 60、所有题都是 `short_answer`、每题 20 分、`scoring_mode` 分别一致，并调用 `validate_questions`。另断言索引包含三个 `closed` 条目。

- [ ] **Step 2: 运行测试确认失败**

Run: `env PYTHONPATH=. uv run pytest tests/test_specialized_papers.py -q`
Expected: FAIL，指出专项卷文件不存在。

- [ ] **Step 3: 新增文本专项卷**

创建 `text-scoring-specialist.json`，题目覆盖 REST 幂等性、数据库事务、缓存一致性、认证与授权、故障排查；每题提供 4 个 5 分评分点，`scoring_mode` 为 `text`。

- [ ] **Step 4: 新增 SQL 专项卷**

创建 `sql-scoring-specialist.json`，题目覆盖筛选排序、分组聚合、连接、子查询、窗口函数；每题提供可解析参考 SQL，`scoring_mode` 为 `sql`，`code_language` 为 `sql`。

- [ ] **Step 5: 新增代码专项卷**

创建 `code-scoring-specialist.json`，题目覆盖安全除法、稳定去重、括号校验、合并区间、带 TTL 的缓存；参考答案均为 Python，`scoring_mode` 为 `code`，`code_language` 为 `python`。

- [ ] **Step 6: 注册索引并运行测试**

在 `data/papers/index.json` 增加三条 `closed` 记录，`question_count` 为 5，`total_score` 为 100。

Run: `env PYTHONPATH=. uv run pytest tests/test_specialized_papers.py -q`
Expected: PASS。

### Task 3: 扩展评分效果验证数据

**Files:**
- Modify: `scripts/validate_scoring_system.py`
- Modify: `tests/test_scoring_validation_script.py`
- Create: `reports/scoring-validation-<timestamp>.json`
- Create: `reports/scoring-validation-<timestamp>.md`

**Interfaces:**
- Consumes: 三套专项卷及现有提交/轮询评分流程
- Produces: 每卷完整、部分正确、错误三类答案的评分明细报告

- [ ] **Step 1: 编写失败测试**

扩展验证脚本测试，断言案例集合包含三个专项卷 slug，每个 slug 都有 `complete`、`partial`、`wrong` 三类案例，并且答案覆盖全部 5 个题号。

- [ ] **Step 2: 运行测试确认失败**

Run: `env PYTHONPATH=. uv run pytest tests/test_scoring_validation_script.py -q`
Expected: FAIL，指出专项卷案例缺失。

- [ ] **Step 3: 添加专项卷案例**

在验证脚本中加入三卷共 9 个提交案例。完整答案使用参考解法，部分答案保留核心结构但遗漏关键要求，错误答案使用语义或结构明显不符的内容。

- [ ] **Step 4: 验证脚本测试通过**

Run: `env PYTHONPATH=. uv run pytest tests/test_scoring_validation_script.py -q`
Expected: PASS。

- [ ] **Step 5: 运行真实效果验证**

Run: `env PYTHONPATH=. uv run python scripts/validate_scoring_system.py`
Expected: 在 `reports/` 写入带时间戳的 JSON 和 Markdown；远程文本评分不可用时报告中明确记录错误状态，SQL 与代码案例继续完成。

### Task 4: 回归验证与结果审阅

**Files:**
- Verify: `tests/`
- Verify: `reports/scoring-validation-<timestamp>.md`

**Interfaces:**
- Consumes: Tasks 1-3 的全部变更
- Produces: 可复现的测试与评分效果结论

- [ ] **Step 1: 运行相关测试**

Run: `env PYTHONPATH=. uv run pytest tests/test_dependency_boundaries.py tests/test_specialized_papers.py tests/test_scoring_validation_script.py tests/test_core.py -q`
Expected: PASS。

- [ ] **Step 2: 运行完整测试**

Run: `env PYTHONPATH=. uv run pytest -q`
Expected: PASS；若存在与本次无关的既有失败，记录具体测试名和证据。

- [ ] **Step 3: 检查工作区与报告**

Run: `git diff --check`
Expected: 无空白错误。

人工检查 Markdown 报告中三类专项卷均存在，并总结各评分轨道对完整、部分正确和错误答案的区分效果。
