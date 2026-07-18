# v0.1.7 Calculation Mode Integration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** 让 examSystem 正确使用 subjective-scoring v0.1.7 的 calculation 模式，并保持 text/sql/code 兼容。

**Architecture:** 题库以 `calculation` 对象描述步骤和最终数值，grader 将其放入 `ScoringRequest.scoring_config.calculation`；loader 负责结构校验，converter 负责题型默认模式，grader 负责运行时映射。

**Tech Stack:** Python、Pydantic、pytest、subjective-scoring v0.1.7。

## Global Constraints

- 已启用 calculation 的题目必须显式配置 `steps` 或 `final_answers`；缺少配置时 loader 保留兼容性，grader 回退到 `text`，不得静默按 calculation 给零分。
- `calculation` 只允许 `static_values` 策略；学生提交内容不执行。
- 普通简答保持 `text`，SQL保持 `sql`，程序题使用 `code`。

### Tasks

- [ ] 为 loader、grader 和 converter 添加失败测试。
- [ ] 扩展题库校验和 grader 请求映射。
- [ ] 让 Word 转换器区分 calculation/code/text。
- [ ] 为代表性计算题补充配置并验证评分。
- [ ] 运行全套测试、检查 diff 并提交。
