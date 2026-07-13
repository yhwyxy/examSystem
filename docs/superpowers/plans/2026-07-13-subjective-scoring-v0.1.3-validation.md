# subjective-scoring v0.1.3 同卷验证实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 将评分依赖升级到 v0.1.3，使用现有专项卷和固定答案重新评分，并生成与 v0.1.2 的可复现对比。

**Architecture:** 依赖声明、锁文件和实际安装版本统一指向 annotated tag v0.1.3。现有隔离验证脚本保持试卷与答案不变，新建一个纯报告对比脚本读取两份 JSON，输出专项卷总分和题目级变化。

**Tech Stack:** Python 3.13、uv、pytest、Git、subjective-scoring v0.1.3、JSON、Markdown

## Global Constraints

- `v0.1.3^{}` 必须指向提交 `2cf6b24`。
- 不修改三套专项卷、参考答案、评分点或固定考生答案。
- 复用 v0.1.2 基线 `reports/scoring-validation-20260712-210421.json`。
- 不调整评分参数或业务评分规则。
- 所有远程请求仅使用 HTTP/HTTPS 代理，不使用 SOCKS。

---

### Task 1: 验证并升级依赖

**Files:**
- Modify: `tests/test_dependency_boundaries.py`
- Modify: `pyproject.toml`
- Modify: `requirements.txt`
- Modify: `README.md`
- Modify: `uv.lock`

**Interfaces:**
- Consumes: Git tag `v0.1.3` 和解引用提交 `2cf6b24`
- Produces: 安装版本 `subjective-scoring==0.1.3`

- [ ] **Step 1:** 使用 `git ls-remote` 验证 annotated tag 及解引用提交。
- [ ] **Step 2:** 将依赖边界测试预期版本改为 `v0.1.3`。
- [ ] **Step 3:** 运行边界测试并确认因项目仍声明 `v0.1.2` 而失败。
- [ ] **Step 4:** 更新项目声明、requirements 和 README 为 `v0.1.3`。
- [ ] **Step 5:** 运行 `uv lock` 与 `uv sync --extra scoring --extra dev`。
- [ ] **Step 6:** 验证边界测试及安装包版本通过。

### Task 2: 运行同卷真实评分

**Files:**
- Create: `reports/scoring-validation-<timestamp>.json`
- Create: `reports/scoring-validation-<timestamp>.md`

**Interfaces:**
- Consumes: `scripts/validate_scoring_system.py`、三套专项卷、固定候选答案
- Produces: v0.1.3 的 16 次隔离提交报告

- [ ] **Step 1:** 运行验证脚本单元测试，确认案例仍覆盖三个 slug 和三档质量。
- [ ] **Step 2:** 通过 HTTP/HTTPS 代理运行端到端验证脚本。
- [ ] **Step 3:** 确认 16/16 提交完成、评分错误为 0、生产数据未变化。

### Task 3: 生成版本对比摘要

**Files:**
- Create: `scripts/compare_scoring_reports.py`
- Create: `tests/test_compare_scoring_reports.py`
- Create: `reports/scoring-comparison-v0.1.2-v0.1.3.md`

**Interfaces:**
- Consumes: 两份评分验证 JSON
- Produces: `compare_reports(baseline: dict, candidate: dict) -> dict` 与 Markdown 对比报告

- [ ] **Step 1:** 编写失败测试，断言比较函数输出专项卷总分 delta、指标 delta 和变化最大的题目。
- [ ] **Step 2:** 运行测试并确认比较模块不存在或接口缺失。
- [ ] **Step 3:** 实现纯 JSON 比较与 Markdown 渲染，不发起评分请求。
- [ ] **Step 4:** 运行测试通过并生成 v0.1.2→v0.1.3 对比报告。

### Task 4: 回归验证

**Files:**
- Verify: `tests/`
- Verify: 新评分报告与对比报告

**Interfaces:**
- Consumes: Tasks 1-3 的全部结果
- Produces: 最终验证结论

- [ ] **Step 1:** 运行完整 `pytest -q`，要求零失败。
- [ ] **Step 2:** 对本次文件运行 `git diff --check`。
- [ ] **Step 3:** 验证安装版本、锁文件提交、工作流指标和三套专项卷排序。
