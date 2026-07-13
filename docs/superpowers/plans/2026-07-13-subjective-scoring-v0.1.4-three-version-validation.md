# subjective-scoring v0.1.4 三版本验证实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 升级到 v0.1.4，使用固定专项卷复验，并生成 v0.1.2、v0.1.3、v0.1.4 三版本横向报告。

**Architecture:** 依赖声明最终固定到 annotated tag v0.1.4 和提交 `c559c1ca720d8f1c1f48ce716707af57c2b65d52`。现有端到端脚本生成 v0.1.4 报告；三版本比较工具严格校验共同数据键后输出指标、总分、排序和题目级变化。

**Tech Stack:** Python 3.13、uv、pytest、Git、subjective-scoring v0.1.4、JSON、Markdown

## Global Constraints

- 不修改专项卷、固定答案或评分参数。
- 使用 v0.1.2 `20260712-210421` 与 v0.1.3 `20260713-070002` 报告作为基线。
- 远程请求仅使用 HTTP/HTTPS 代理。
- 三版本缺失共同 submission 或 record key 时比较必须失败。
- 最终项目依赖版本为 v0.1.4。

---

### Task 1: 升级到 v0.1.4

**Files:**
- Modify: `tests/test_dependency_boundaries.py`
- Modify: `pyproject.toml`
- Modify: `requirements.txt`
- Modify: `README.md`
- Modify: `uv.lock`

**Interfaces:**
- Consumes: tag `v0.1.4`、commit `c559c1ca720d8f1c1f48ce716707af57c2b65d52`
- Produces: installed `subjective-scoring==0.1.4`

- [ ] **Step 1:** 将边界测试期望改为 v0.1.4 并确认对当前 v0.1.3 声明失败。
- [ ] **Step 2:** 更新项目依赖声明、README 和 requirements。
- [ ] **Step 3:** 运行 `uv lock`、`uv sync --extra scoring --extra dev`。
- [ ] **Step 4:** 验证边界测试、安装版本与锁文件提交。

### Task 2: 运行 v0.1.4 同卷复验

**Files:**
- Create: `reports/scoring-validation-<timestamp>.json`
- Create: `reports/scoring-validation-<timestamp>.md`

**Interfaces:**
- Consumes: 原三套专项卷和 9 个固定专项候选
- Produces: v0.1.4 隔离评分报告

- [ ] **Step 1:** 运行专项卷和验证案例单元测试。
- [ ] **Step 2:** 通过 HTTP/HTTPS 运行 16 次端到端提交。
- [ ] **Step 3:** 确认 16/16、评分错误 0、生产数据未变化。

### Task 3: 扩展为三版本比较

**Files:**
- Modify: `scripts/compare_scoring_reports.py`
- Modify: `tests/test_compare_scoring_reports.py`
- Create: `reports/scoring-comparison-v0.1.2-v0.1.3-v0.1.4.md`

**Interfaces:**
- Consumes: `compare_versions(reports: dict[str, dict]) -> dict`
- Produces: 三版本指标、专项总分、排序状态、相邻及首尾 delta

- [ ] **Step 1:** 编写失败测试，要求三个版本列、严格 key 校验和排序状态。
- [ ] **Step 2:** 运行测试确认现有双版本接口不足。
- [ ] **Step 3:** 实现三版本比较，同时保留双版本函数兼容已有测试。
- [ ] **Step 4:** 生成三版本 Markdown 报告并检查表格完整。

### Task 4: 最终回归

**Files:**
- Verify: `tests/`
- Verify: 三份验证 JSON 与三版本 Markdown

**Interfaces:**
- Produces: 可复现的 v0.1.4 验证结论

- [ ] **Step 1:** 运行完整 `pytest -q`。
- [ ] **Step 2:** 对本次文件运行 `git diff --check`。
- [ ] **Step 3:** 验证安装版本、锁定提交、报告指标和三类质量排序。
