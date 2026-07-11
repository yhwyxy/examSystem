# 主观题自动评分独立模块第一版设计

## 1. 背景与目标

本设计用于将考试系统中的主观题判题逻辑抽象为独立评分模块。第一版目标不是构建一个“所有题目统一丢给模型”的模型堆叠式方案，而是构建一个面向实际阅卷场景的模块化、多评分引擎分流系统。

系统采用 Haystack Pipeline 作为流程编排层，通过题目元数据和评分模式将答案分流到文本、SQL、通用代码三类评分引擎。各评分引擎使用不同的判分策略，并统一输出中间评分结果，最后由 ScoreAggregator 汇总为标准评分结果。

第一版重点目标：

1. 支持文本主观题基于结构化评分点自动评分。
2. 支持 SQL 题基于 sqlglot AST 结构比较评分。
3. 支持通用代码题基于 Code CrossEncoder 与 tree-sitter AST 的混合评分。
4. 保持评分过程可解释、可复核、可扩展。
5. 避免第一版引入过重的逻辑关系抽取、推理规则或自动评分点生成能力。

## 2. 总体架构

第一版采用：

```text
Haystack Pipeline 作为流程编排层 + 多评分引擎分流架构
```

核心流程：

```text
InputNormalizer
        ↓
QuestionTypeRouter
        ↓
 ┌──────────────┬──────────────┬──────────────┐
 Text          SQL            Code
 ↓             ↓              ↓
BGE           sqlglot        Code CrossEncoder
Reranker      AST            +
+             Comparator     tree-sitter
Rules                        AST
 ↓             ↓              ↓
TextScore     SQLScore       HybridScore
        ↓
ScoreAggregator
        ↓
ScoringResult
```

对应模块：

```text
SubjectiveScoringService
        ↓
Haystack Pipeline
        ↓
InputNormalizerComponent
        ↓
QuestionTypeRouter / ConditionalRouter
        ├── TextRerankerScorer
        ├── SQLStructureScorer
        └── CodeHybridScorer
        ↓
ScoreAggregatorComponent
        ↓
ScoringResult
```

## 3. 设计原则

第一版冻结以下设计原则：

1. Haystack 只作为流程编排层，不承载具体判分逻辑。
2. Router 优先依赖试卷元数据，而不是自动猜测答案类型。
3. 文本题以结构化评分点为核心判分单位。
4. SQL 题不走模型，优先使用 sqlglot AST 结构比较。
5. 通用代码题采用语义分与结构分融合。
6. 删除独立 LogicRelationScorer，不在第一版引入句法依存、逻辑关系抽取和推理规则。
7. ScoreAggregator 只负责合并结果、总分计算和解释生成，不再次调用模型。
8. 所有评分模块必须输出统一中间结果。
9. 低置信度结果必须进入人工复核流程。
10. 第一版默认关闭自动生成评分点能力。

## 4. 输入数据结构

推荐统一请求结构：

```json
{
  "questionId": "q001",
  "paperId": "p001",
  "questionType": "subjective",
  "scoringMode": "text",
  "codeLanguage": null,
  "courseType": "database",
  "maxScore": 10,
  "question": "索引的作用是什么？",
  "referenceAnswer": "索引可以提高查询效率，减少全表扫描。",
  "scoringPoints": [
    {
      "id": "p1",
      "text": "提高查询效率",
      "score": 5,
      "required": true
    },
    {
      "id": "p2",
      "text": "减少全表扫描",
      "score": 5,
      "required": false
    }
  ],
  "studentAnswer": "索引可以让数据库查得更快。",
  "scoringConfig": {
    "manualReviewThresholds": {
      "autoPass": 0.85,
      "review": 0.6
    },
    "codeScoreWeights": {
      "semantic": 0.7,
      "structure": 0.3
    },
    "allowAutoScoringPointGeneration": false
  }
}
```

字段说明：

- `scoringMode`：评分模式，第一版建议显式配置，可选值包括 `text`、`sql`、`code`。
- `codeLanguage`：代码语言，例如 `sql`、`python`、`java`、`javascript`、`cpp`。
- `scoringPoints`：人工配置评分点，文本题第一版推荐必配。
- `scoringConfig.allowAutoScoringPointGeneration`：是否允许自动生成评分点，第一版默认 `false`。

## 5. InputNormalizerComponent

InputNormalizerComponent 负责输入归一化，但必须按题型差异化处理，避免统一清洗破坏语义。

### 5.1 文本题归一化

文本题可处理：

- 去除无意义空格。
- 全角半角转换。
- 标点统一。
- 中文数字规范化。
- Unicode 异常修复。
- 繁简转换，可选。

推荐依赖：

- `ftfy`：修复 Unicode 和异常字符。
- 自定义清洗函数：处理业务相关的空白、标点、中文数字、否定词保留等。
- `LTP` / `HanLP`：第一版不作为默认必跑组件，只作为后续中文分析增强能力保留接口。

### 5.2 SQL 归一化

SQL 题只做有利于解析和结构比较的规范化：

- 关键字大小写统一。
- SQL 格式化。
- 去除无意义空白。
- 保留字段名、表名、操作符、条件结构。

示例：

```sql
select * from student
SELECT * FROM student
```

归一化后交给 `sqlglot` 解析。

### 5.3 通用代码归一化

代码题禁止过度清洗。

禁止：

- 删除变量名。
- 重写变量名。
- 改写代码结构。
- 删除关键语句。
- 自动修复逻辑。

允许：

- 编码统一。
- 去除注释，可配置。
- 格式化，可配置。
- 去除首尾无意义空白。
- 保留缩进、变量名、函数名、调用关系。

代码中的变量名、缩进、函数调用、语法结构可能是评分依据，因此不能像自然语言一样 aggressive normalization。

## 6. QuestionTypeRouter / ConditionalRouter

Router 使用 Haystack 自带 `ConditionalRouter` 实现。

第一版不建议主要依赖答案内容自动猜测题型，而应优先依赖试卷元数据。

推荐路由优先级：

```text
1. scoringMode 显式字段
2. questionType 题型字段
3. codeLanguage 代码语言字段
4. courseType / subjectType 课程字段
5. referenceAnswer 标准答案特征
6. studentAnswer 学生答案特征兜底
```

推荐逻辑：

```text
if scoringMode == "text":
    route to TextRerankerScorer
elif scoringMode == "sql":
    route to SQLStructureScorer
elif scoringMode == "code":
    route to CodeHybridScorer
elif codeLanguage == "sql":
    route to SQLStructureScorer
elif codeLanguage in ["python", "java", "javascript", "typescript", "cpp", "go"]:
    route to CodeHybridScorer
else:
    route to TextRerankerScorer
```

当元数据缺失时，才允许通过答案内容特征兜底判断。

## 7. TextRerankerScorer

### 7.1 核心策略

文本题采用：

```text
结构化评分点优先 + BGE Reranker 语义匹配 + 规则拦截
```

判分核心单位不是：

```text
学生答案 vs 标准答案全文
```

而是：

```text
学生答案 vs 每个评分点
```

原因是人工阅卷本质上是判断学生覆盖了哪些知识点，而不是判断两段文字整体像不像。

### 7.2 流程

```text
ScoringPointResolver
        ↓
学生答案 vs 每个评分点
        ↓
FlagEmbedding BGE Reranker / CrossEncoder
        ↓
RuleInterceptor
        ↓
TextScoreMapper
        ↓
TextScoreResult
```

### 7.3 评分点策略

第一版策略：

```text
人工配置 scoringPoints > 标准答案全文兜底
```

虽然系统结构预留自动拆分评分点能力，但第一版默认关闭自动生成评分点：

```json
{
  "allowAutoScoringPointGeneration": false
}
```

原因：

- 当前场景试卷、专业、题目相对固定。
- 人工配置一次评分点成本较低。
- 人工评分点更稳定、更可解释。
- 自动拆分评分点容易引入额外误差。

第二版可以考虑：

```text
LLM/规则辅助生成评分点 -> 教师确认 -> 入库使用
```

### 7.4 逐点评分示例

10 分题：

```json
{
  "scoringPoints": [
    {"id": "p1", "text": "掌握知识点 A", "score": 3},
    {"id": "p2", "text": "掌握知识点 B", "score": 3},
    {"id": "p3", "text": "掌握知识点 C", "score": 4}
  ]
}
```

评分：

```text
学生答案 vs point1 -> similarity 0.90 -> 3.0 分
学生答案 vs point2 -> similarity 0.50 -> 1.5 分
学生答案 vs point3 -> similarity 0.80 -> 3.2 分
最终文本分 = 7.7 / 10
```

### 7.5 RuleInterceptor

虽然删除独立 LogicRelationScorer，但文本题仍保留轻量规则拦截。

规则拦截包括：

- 否定词冲突检查。
- 反义词冲突检查。
- 数字一致性检查。
- 单位一致性检查。
- 方向词检查。
- 关键限定词检查。

示例：

```text
标准评分点：索引可以提高查询效率。
学生答案：索引不能提高查询效率。
```

即使 BGE Reranker 给出较高相关性，RuleInterceptor 也应触发冲突拦截、降低得分或标记人工复核。

## 8. 取消独立 LogicRelationScorer

第一版删除独立 LogicRelationScorer。

原因：

- 句法依存、逻辑关系抽取、推理规则会显著增加工程复杂度。
- 错误来源增加，难以评估收益。
- 第一版优先追求稳定、可解释、可落地。

替代策略：

```text
BGE Reranker
+
RuleInterceptor
+
confidence 机制
+
人工复核
```

后续版本如果有充足标注数据和明确需求，再考虑引入 LTP/HanLP 的句法分析或逻辑关系抽取。

## 9. SQLStructureScorer

### 9.1 核心策略

SQL 题确认不走模型。

采用：

```text
SQL
 ↓
sqlglot
 ↓
AST
 ↓
结构比较
 ↓
评分
```

SQL 是高度结构化语言，AST 方法比神经模型更可靠、更可解释。

### 9.2 流程

```text
SQLNormalizer
        ↓
sqlglot Parser
        ↓
SQL AST Comparator
        ↓
SQLScoreMapper
        ↓
SQLScoreResult
```

### 9.3 结构评分点

SQL 结构评分点包括：

- SELECT 字段是否正确。
- FROM 表是否正确。
- JOIN 表和连接条件是否正确。
- WHERE 条件是否正确。
- GROUP BY 是否正确。
- HAVING 是否正确。
- ORDER BY 是否正确。
- LIMIT 是否正确。
- 聚合函数是否正确。
- 子查询结构是否正确。
- 比较运算符方向是否正确。

示例：

```sql
SELECT name FROM student
select name from student
```

这两个 SQL 的 AST 基本一致，应判为等价或高相似。

但是：

```sql
WHERE age > 18
WHERE age < 18
```

虽然文本非常接近，但 AST 条件方向不同，应明显扣分。

## 10. CodeHybridScorer

### 10.1 核心策略

通用代码题采用：

```text
Code CrossEncoder + tree-sitter AST 双通道融合
```

即：

- 语义分：代码是否表达类似算法思想。
- 结构分：代码是否实现对应程序结构。

### 10.2 流程

```text
CodeNormalizer
        ↓
tree-sitter AST Extractor
        ↓
StructureScoreCalculator
        ↓
Code CrossEncoder
        ↓
HybridScoreAggregator
        ↓
CodeScoreResult
```

### 10.3 结构分

由 `tree-sitter` 提取 AST 特征。

可评分结构包括：

- 是否使用循环。
- 是否使用条件判断。
- 是否定义函数。
- 是否调用指定 API。
- 是否包含返回语句。
- 是否使用指定数据结构。
- 是否包含异常处理。
- 是否存在递归调用。
- 是否包含输入输出逻辑。
- 是否处理边界条件。

示例：

```python
for i in range(10):
    pritn(i)
```

即使 `print` 拼写错误，tree-sitter 仍可识别循环结构，因此结构分可以较高。

### 10.4 语义分

由 Code CrossEncoder 判断学生代码和参考代码/参考意图之间的整体语义相似度。

适合处理：

- 代码写法不同但思路接近。
- 变量名不同但逻辑一致。
- 结构不完全相同但算法意图相似。
- 短代码片段容错。

### 10.5 融合策略

代码分不要简单平均。

默认：

```text
FinalCodeScore = semantic_score × 0.7 + structure_score × 0.3
```

权重必须支持按课程、题型、题目配置调整。

示例配置：

```json
{
  "codeScoreWeights": {
    "semantic": 0.7,
    "structure": 0.3
  }
}
```

不同场景建议：

- 算法设计题：结构权重可提高，例如 semantic 0.5 / structure 0.5。
- 语法练习题：结构或语法点权重可提高。
- 开放代码题：语义权重可提高，例如 semantic 0.7 / structure 0.3。
- API 使用题：结构/API 调用权重可提高。

## 11. 统一中间结果接口

所有评分模块必须输出统一中间结果，避免 ScoreAggregator 面对不同格式。

统一结构：

```json
{
  "score": 8.5,
  "maxScore": 10,
  "confidence": 0.9,
  "matchedEvidence": [],
  "missedEvidence": [],
  "warnings": []
}
```

推荐完整结构：

```json
{
  "scorer": "TextRerankerScorer",
  "scoringMode": "text",
  "score": 8.5,
  "maxScore": 10,
  "confidence": 0.9,
  "matchedEvidence": [
    {
      "pointId": "p1",
      "score": 3,
      "maxScore": 3,
      "evidence": "学生答案中的对应片段",
      "reason": "命中评分点：提高查询效率",
      "similarity": 0.92
    }
  ],
  "missedEvidence": [
    {
      "pointId": "p2",
      "score": 0,
      "maxScore": 2,
      "reason": "未明确表达减少全表扫描"
    }
  ],
  "warnings": [],
  "metadata": {
    "model": "BAAI/bge-reranker-base",
    "parser": null
  }
}
```

无论 Text、SQL、Code，最终都必须转换为该中间格式。

## 12. ScoreAggregatorComponent

ScoreAggregatorComponent 只负责合并结果，不再调用模型，也不进行二次智能评分。

职责：

- 汇总模块得分。
- 计算总分。
- 处理扣分项。
- 处理封顶规则。
- 处理最低分规则。
- 合并 matchedEvidence / missedEvidence。
- 生成 warnings。
- 计算最终 confidence。
- 判断 needManualReview。
- 输出统一 ScoringResult。

禁止：

- 再次调用 BGE。
- 再次调用 Code CrossEncoder。
- 再次调用 LLM。
- 重新解释答案语义。
- 覆盖子模块评分逻辑。

## 13. 人工复核机制

第一版必须引入 confidence threshold。

推荐默认阈值：

```text
confidence >= 0.85        自动通过
0.60 <= confidence < 0.85 提示复核
confidence < 0.60         人工处理
```

对应结果字段：

```json
{
  "confidence": 0.72,
  "needManualReview": true,
  "reviewLevel": "suggested_review"
}
```

推荐等级：

```text
auto_pass：自动通过
suggested_review：建议复核
manual_required：必须人工处理
```

人工复核触发条件包括：

- confidence 低于阈值。
- RuleInterceptor 检测到否定冲突。
- SQL 解析失败。
- Code AST 提取失败。
- 模型分数与结构分冲突较大。
- 未配置评分点且使用全文兜底。

## 14. 第一版不启用自动评分点生成

虽然架构支持：

```text
标准答案全文 -> 自动拆分评分点
```

但第一版默认关闭。

第一版推荐：

```text
人工评分点 -> 稳定评分
```

原因：

- 试卷固定、专业固定、题目固定时，人工配置一次评分点成本可控。
- 自动拆分评分点质量不稳定。
- 自动评分点会降低可解释性与可追责性。
- 第一版应优先保证评分稳定性。

第二版可扩展为：

```text
LLM/规则辅助生成评分点 -> 教师确认 -> 存入 scoringPoints -> 后续自动评分
```

## 15. 最终输出 ScoringResult

统一输出结构：

```json
{
  "questionId": "q001",
  "score": 7.7,
  "maxScore": 10,
  "scoringMode": "text",
  "track": "TextRerankerScorer",
  "confidence": 0.86,
  "needManualReview": false,
  "reviewLevel": "auto_pass",
  "matchedPoints": [
    {
      "pointId": "p1",
      "score": 3.0,
      "maxScore": 3.0,
      "similarity": 0.9,
      "evidence": "学生答案中对应知识点 A 的表达",
      "reason": "命中评分点：掌握知识点 A"
    }
  ],
  "missedPoints": [
    {
      "pointId": "p2",
      "score": 0,
      "maxScore": 3.0,
      "reason": "未明确表达知识点 B"
    }
  ],
  "warnings": []
}
```

## 16. 后续扩展方向

第一版不实现但保留接口的能力：

1. 自动评分点生成：LLM/规则生成后由教师确认。
2. LTP/HanLP 逻辑关系增强：用于部分因果题、条件题。
3. 向量召回 + Reranker 两阶段评分：用于评分点较多的大题。
4. CodeBERT/GraphCodeBERT 微调：用于代码语义评分增强。
5. 历史人工判分样本回流：用于阈值优化和模型微调。
6. 人工复核后台：展示证据、模型分数、结构分、规则拦截原因。

## 17. 自检结论

本设计已完成以下自检：

- 无 TBD/TODO 占位内容。
- 总体架构、模块职责、输入输出结构保持一致。
- 第一版范围聚焦于可落地能力，没有引入独立逻辑关系抽取和自动评分点生成。
- Text、SQL、Code 三类评分路径边界明确。
- ScoreAggregator 职责限定清晰，不承担二次智能评分。
- 人工复核机制和统一中间结果接口已纳入第一版基线。
