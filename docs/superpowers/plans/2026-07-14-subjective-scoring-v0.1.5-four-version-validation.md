# subjective-scoring v0.1.5 四版本验证实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 升级到 v0.1.5，复用固定专项卷完成同卷核验，并输出 v0.1.2–v0.1.5 四版本报告。

**Architecture:** 依赖声明和锁文件固定到 v0.1.5/`9968d12aeab48968e32e23a8dd80dfe1859cb5f3`。现有验证脚本负责生成 v0.1.5 JSON/Markdown，四版本比较工具读取四份固定报告并严格校验共同题目键。

**Tech Stack:** Python 3.13、uv、pytest、Git、subjective-scoring v0.1.5、JSON、Markdown

## Global Constraints

- 不修改专项卷、固定答案或评分参数。
- 基线固定为 v0.1.2、v0.1.3、v0.1.4 已生成报告。
- 使用 HTTP/HTTPS 代理，不使用 SOCKS。
- 隔离验证轮询可清理 TestClient 限流状态，但不改生产限流策略。
- 最终依赖版本为 v0.1.5。

---

### Task 1: 升级依赖

**Files:** `pyproject.toml`, `requirements.txt`, `README.md`, `uv.lock`, `tests/test_dependency_boundaries.py`

- [ ] 更新依赖边界测试到 v0.1.5，运行确认旧声明失败。
- [ ] 更新项目声明和 README。
- [ ] 运行 `uv lock`、`uv sync --extra scoring --extra dev`。
- [ ] 验证安装版本和锁定提交。

### Task 2: v0.1.5 同卷核验

**Files:** 新增 `reports/scoring-validation-<timestamp>.json/.md`

- [ ] 运行专项卷和验证脚本测试。
- [ ] 通过 HTTP/HTTPS 运行 16 次隔离提交。
- [ ] 确认 16/16、评分错误 0、生产数据未变化。

### Task 3: 四版本报告

**Files:** `scripts/compare_scoring_reports.py`, `tests/test_compare_scoring_reports.py`, `reports/scoring-comparison-v0.1.2-v0.1.3-v0.1.4-v0.1.5.md`

- [ ] 先增加失败测试，要求四版本列、严格共同键校验和最新版本题目 delta。
- [ ] 扩展比较函数和 Markdown 渲染器，保留现有双/三版本测试。
- [ ] 生成并检查四版本报告。

### Task 4: 回归验证

- [ ] 运行完整 `pytest -q`。
- [ ] 运行本次文件 `git diff --check`。
- [ ] 核对 tag、安装版本、16/16 指标、四版本表格和工作区状态。
