# 顶层代码题语言选择与多语言参考答案设计

**日期:** 2026-07-20  
**状态:** 已按既定方案落地（用户要求连续执行，不再等待确认）

## 背景

复合题子题已支持 `allowed_languages` + `{answer, language}` 提交与评分；顶层 `essay` 代码题（如软件开发 q16/q21）只有 textarea，`code_language` 写死在试卷里，学生无法选择实现语言，评分也只对单一参考答案。

## 目标

1. 顶层代码题在答题页展示语言下拉框（与复合题一致）。
2. 学生提交 `{answer, language}`；评分使用所选语言。
3. 支持按语言切换参考答案与评分点（混合方案 C）。
4. 为软件开发 q16/q21 补齐多语言参考。

## 方案选择：C（混合）

| 方案 | 描述 | 结论 |
|------|------|------|
| A | 每语言完整 `answers_by_language` | 冗余大，适合差异大的实现 |
| B | 只存结构评分点，语言无关 | 无法做 AST/语言相关评分 |
| **C** | 默认 `answer`/`scoring_points` + `answers_by_language`/`scoring_points_by_language` 覆盖 | **采用**：兼容旧数据，可按需补语言 |

## 数据模型

顶层代码题扩展字段：

```json
{
  "id": "q16",
  "type": "essay",
  "scoring_mode": "code",
  "code_language": "python",
  "allowed_languages": ["python", "javascript", "java", "c", "csharp"],
  "answer": "...python 默认参考...",
  "scoring_points": [...],
  "answers_by_language": {
    "javascript": "...",
    "java": "...",
    "c": "...",
    "csharp": "..."
  },
  "scoring_points_by_language": {
    "javascript": [...]
  }
}
```

规则：

- `allowed_languages` 为空时：不展示下拉；沿用 `code_language`（兼容旧题）。
- 选中语言若在 `answers_by_language` 中有值，覆盖 `answer`；否则用默认 `answer`。
- 选中语言若在 `scoring_points_by_language` 中有值，覆盖 `scoring_points`。
- 学生端 `sanitize_for_student` 剥离 `answers_by_language` / `scoring_points_by_language`。

## 前端

- `renderQuestion`：顶层 `scoring_mode===code` 且 `allowed_languages.length>0` 时渲染语言 select。
- `collectAnswers`：对应题提交 `{answer, language}`。
- 语言列表：优先 `allowed_languages`，否则回退 `code_language` 单值（不展示下拉）。

## 后端

- `normalize_submitted_answer`：顶层 code 题同样解析 `{answer, language}`，校验白名单。
- `build_scoring_request` / `grade_question`：注入 `selected_language` → `code_language`，并应用语言参考覆盖。
- `question_loader`：规范化 `answers_by_language` / `scoring_points_by_language`。

## 非目标（本迭代不做）

- 管理端 UI 编辑多语言答案。
- 全量主观题 ≤2 分误差修复（另开任务；本迭代只修语言路径导致的代码题误差）。
