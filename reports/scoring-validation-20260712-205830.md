# Scoring System Validation Report

- Generated: `2026-07-12T12:58:30.280321+00:00`
- Remote URL: `https://router.tumuer.me/v1/rerank`
- Remote model: `Pro/BAAI/bge-reranker-v2-m3`
- Planned submissions: `16`
- Completed submissions: `15`
- Workflow success rate: `93.8%`
- Band hit rate: `83.6%`
- Mean absolute error: `7.348`
- Ordering accuracy: `28.0%`
- Scoring errors: `0`
- Production data unchanged: `True`

## Per-answer Results

| Paper | Candidate | Quality | Question | Expected | Actual | In band | Method | Confidence |
|---|---|---|---|---:|---:|:---:|---|---:|
| validation-fundamentals | 基础卷-完整答案 | complete | fund-choice | 5.0-5.0 | 5.0 | yes | objective_rule | 1.0000 |
| validation-fundamentals | 基础卷-完整答案 | complete | fund-rest | 9.0-10.0 | 0.0 | no | subjective_scoring:TextRerankerScorer | 0.0000 |
| validation-fundamentals | 基础卷-完整答案 | complete | fund-status | 9.0-10.0 | 0.0 | no | subjective_scoring:TextRerankerScorer | 0.0000 |
| validation-fundamentals | 基础卷-同义改写 | paraphrase | fund-choice | 5.0-5.0 | 5.0 | yes | objective_rule | 1.0000 |
| validation-fundamentals | 基础卷-同义改写 | paraphrase | fund-rest | 8.0-10.0 | 0.0 | no | subjective_scoring:TextRerankerScorer | 0.0000 |
| validation-fundamentals | 基础卷-同义改写 | paraphrase | fund-status | 8.0-10.0 | 0.0 | no | subjective_scoring:TextRerankerScorer | 0.0000 |
| validation-fundamentals | 基础卷-部分正确 | partial | fund-choice | 0.0-0.0 | 0.0 | yes | objective_rule | 1.0000 |
| validation-fundamentals | 基础卷-部分正确 | partial | fund-rest | 3.0-5.0 | 0.0 | no | subjective_scoring:TextRerankerScorer | 0.0000 |
| validation-fundamentals | 基础卷-部分正确 | partial | fund-status | 4.0-7.0 | 0.0 | no | subjective_scoring:TextRerankerScorer | 0.0000 |
| validation-fundamentals | 基础卷-错误答案 | wrong | fund-choice | 0.0-0.0 | 0.0 | yes | objective_rule | 1.0000 |
| validation-fundamentals | 基础卷-错误答案 | wrong | fund-rest | 0.0-2.0 | 0.0 | yes | subjective_scoring:TextRerankerScorer | 0.0000 |
| validation-fundamentals | 基础卷-错误答案 | wrong | fund-status | 0.0-1.0 | 0.0 | yes | subjective_scoring:TextRerankerScorer | 0.0000 |
| validation-backend | 后端卷-完整答案 | complete | back-tx | 9.0-10.0 | 0.0 | no | subjective_scoring:TextRerankerScorer | 0.0000 |
| validation-backend | 后端卷-完整答案 | complete | back-code | 9.0-10.0 | 0.0 | no | subjective_scoring:CodeHybridScorer | 0.0000 |
| validation-backend | 后端卷-完整答案 | complete | back-sql | 9.0-10.0 | 10.0 | yes | subjective_scoring:SQLStructureScorer | 1.0000 |
| validation-backend | 后端卷-部分正确 | partial | back-tx | 3.0-5.0 | 0.0 | no | subjective_scoring:TextRerankerScorer | 0.0000 |
| validation-backend | 后端卷-部分正确 | partial | back-code | 4.0-6.0 | 0.0 | no | subjective_scoring:CodeHybridScorer | 0.0000 |
| validation-backend | 后端卷-部分正确 | partial | back-sql | 5.0-7.0 | 6.9 | yes | subjective_scoring:SQLStructureScorer | 0.6912 |
| validation-backend | 后端卷-错误答案 | wrong | back-tx | 0.0-2.0 | 0.0 | yes | subjective_scoring:TextRerankerScorer | 0.0000 |
| validation-backend | 后端卷-错误答案 | wrong | back-code | 0.0-2.0 | 0.0 | yes | subjective_scoring:CodeHybridScorer | 0.0000 |
| validation-backend | 后端卷-错误答案 | wrong | back-sql | 0.0-1.0 | 0.0 | yes | subjective_scoring:SQLStructureScorer | 0.0000 |
| text-scoring-specialist | 文本主观题评分专项卷-complete | complete | text-1 | 0.0-20.0 | 0.0 | yes | subjective_scoring:TextRerankerScorer | 0.0000 |
| text-scoring-specialist | 文本主观题评分专项卷-complete | complete | text-2 | 0.0-20.0 | 0.0 | yes | subjective_scoring:TextRerankerScorer | 0.0000 |
| text-scoring-specialist | 文本主观题评分专项卷-complete | complete | text-3 | 0.0-20.0 | 0.0 | yes | subjective_scoring:TextRerankerScorer | 0.0000 |
| text-scoring-specialist | 文本主观题评分专项卷-complete | complete | text-4 | 0.0-20.0 | 0.0 | yes | subjective_scoring:TextRerankerScorer | 0.0000 |
| text-scoring-specialist | 文本主观题评分专项卷-complete | complete | text-5 | 0.0-20.0 | 0.0 | yes | subjective_scoring:TextRerankerScorer | 0.0000 |
| text-scoring-specialist | 文本主观题评分专项卷-partial | partial | text-1 | 0.0-20.0 | 0.0 | yes | subjective_scoring:TextRerankerScorer | 0.0000 |
| text-scoring-specialist | 文本主观题评分专项卷-partial | partial | text-2 | 0.0-20.0 | 0.0 | yes | subjective_scoring:TextRerankerScorer | 0.0000 |
| text-scoring-specialist | 文本主观题评分专项卷-partial | partial | text-3 | 0.0-20.0 | 0.0 | yes | subjective_scoring:TextRerankerScorer | 0.0000 |
| text-scoring-specialist | 文本主观题评分专项卷-partial | partial | text-4 | 0.0-20.0 | 0.0 | yes | subjective_scoring:TextRerankerScorer | 0.0000 |
| text-scoring-specialist | 文本主观题评分专项卷-partial | partial | text-5 | 0.0-20.0 | 0.0 | yes | subjective_scoring:TextRerankerScorer | 0.0000 |
| text-scoring-specialist | 文本主观题评分专项卷-wrong | wrong | text-1 | 0.0-20.0 | 0.0 | yes | subjective_scoring:TextRerankerScorer | 0.0000 |
| text-scoring-specialist | 文本主观题评分专项卷-wrong | wrong | text-2 | 0.0-20.0 | 0.0 | yes | subjective_scoring:TextRerankerScorer | 0.0000 |
| text-scoring-specialist | 文本主观题评分专项卷-wrong | wrong | text-3 | 0.0-20.0 | 0.0 | yes | subjective_scoring:TextRerankerScorer | 0.0000 |
| text-scoring-specialist | 文本主观题评分专项卷-wrong | wrong | text-4 | 0.0-20.0 | 0.0 | yes | subjective_scoring:TextRerankerScorer | 0.0000 |
| text-scoring-specialist | 文本主观题评分专项卷-wrong | wrong | text-5 | 0.0-20.0 | 0.0 | yes | subjective_scoring:TextRerankerScorer | 0.0000 |
| sql-scoring-specialist | SQL 主观题评分专项卷-complete | complete | sql-1 | 0.0-20.0 | 20.0 | yes | subjective_scoring:SQLStructureScorer | 1.0000 |
| sql-scoring-specialist | SQL 主观题评分专项卷-complete | complete | sql-2 | 0.0-20.0 | 20.0 | yes | subjective_scoring:SQLStructureScorer | 1.0000 |
| sql-scoring-specialist | SQL 主观题评分专项卷-complete | complete | sql-3 | 0.0-20.0 | 20.0 | yes | subjective_scoring:SQLStructureScorer | 1.0000 |
| sql-scoring-specialist | SQL 主观题评分专项卷-complete | complete | sql-4 | 0.0-20.0 | 20.0 | yes | subjective_scoring:SQLStructureScorer | 1.0000 |
| sql-scoring-specialist | SQL 主观题评分专项卷-complete | complete | sql-5 | 0.0-20.0 | 20.0 | yes | subjective_scoring:SQLStructureScorer | 1.0000 |
| sql-scoring-specialist | SQL 主观题评分专项卷-partial | partial | sql-1 | 0.0-20.0 | 17.3 | yes | subjective_scoring:SQLStructureScorer | 0.8667 |
| sql-scoring-specialist | SQL 主观题评分专项卷-partial | partial | sql-2 | 0.0-20.0 | 8.2 | yes | subjective_scoring:SQLStructureScorer | 0.4097 |
| sql-scoring-specialist | SQL 主观题评分专项卷-partial | partial | sql-3 | 0.0-20.0 | 13.0 | yes | subjective_scoring:SQLStructureScorer | 0.6500 |
| sql-scoring-specialist | SQL 主观题评分专项卷-partial | partial | sql-4 | 0.0-20.0 | 4.5 | yes | subjective_scoring:SQLStructureScorer | 0.2258 |
| sql-scoring-specialist | SQL 主观题评分专项卷-partial | partial | sql-5 | 0.0-20.0 | 9.3 | yes | subjective_scoring:SQLStructureScorer | 0.4667 |
| sql-scoring-specialist | SQL 主观题评分专项卷-wrong | wrong | sql-1 | 0.0-20.0 | 0.0 | yes | subjective_scoring:SQLStructureScorer | 0.0000 |
| sql-scoring-specialist | SQL 主观题评分专项卷-wrong | wrong | sql-2 | 0.0-20.0 | 0.0 | yes | subjective_scoring:SQLStructureScorer | 0.0000 |
| sql-scoring-specialist | SQL 主观题评分专项卷-wrong | wrong | sql-3 | 0.0-20.0 | 0.0 | yes | subjective_scoring:SQLStructureScorer | 0.0000 |
| sql-scoring-specialist | SQL 主观题评分专项卷-wrong | wrong | sql-4 | 0.0-20.0 | 0.0 | yes | subjective_scoring:SQLStructureScorer | 0.0000 |
| sql-scoring-specialist | SQL 主观题评分专项卷-wrong | wrong | sql-5 | 0.0-20.0 | 0.0 | yes | subjective_scoring:SQLStructureScorer | 0.0000 |
| code-scoring-specialist | Python 代码评分专项卷-complete | complete | code-1 | 0.0-20.0 | 0.0 | yes | subjective_scoring:CodeHybridScorer | 0.0000 |
| code-scoring-specialist | Python 代码评分专项卷-complete | complete | code-2 | 0.0-20.0 | 0.0 | yes | subjective_scoring:CodeHybridScorer | 0.0000 |
| code-scoring-specialist | Python 代码评分专项卷-complete | complete | code-3 | 0.0-20.0 | 0.0 | yes | subjective_scoring:CodeHybridScorer | 0.0000 |
| code-scoring-specialist | Python 代码评分专项卷-complete | complete | code-4 | 0.0-20.0 | 0.0 | yes | subjective_scoring:CodeHybridScorer | 0.0000 |
| code-scoring-specialist | Python 代码评分专项卷-complete | complete | code-5 | 0.0-20.0 | 0.0 | yes | subjective_scoring:CodeHybridScorer | 0.0000 |
| code-scoring-specialist | Python 代码评分专项卷-partial | partial | code-1 | 0.0-20.0 | 0.0 | yes | subjective_scoring:CodeHybridScorer | 0.0000 |
| code-scoring-specialist | Python 代码评分专项卷-partial | partial | code-2 | 0.0-20.0 | 0.0 | yes | subjective_scoring:CodeHybridScorer | 0.0000 |
| code-scoring-specialist | Python 代码评分专项卷-partial | partial | code-3 | 0.0-20.0 | 0.0 | yes | subjective_scoring:CodeHybridScorer | 0.0000 |
| code-scoring-specialist | Python 代码评分专项卷-partial | partial | code-4 | 0.0-20.0 | 0.0 | yes | subjective_scoring:CodeHybridScorer | 0.0000 |
| code-scoring-specialist | Python 代码评分专项卷-partial | partial | code-5 | 0.0-20.0 | 0.0 | yes | subjective_scoring:CodeHybridScorer | 0.0000 |

## Failed Ordering Checks

- `validation-fundamentals/fund-rest`: expected 基础卷-完整答案 > 基础卷-同义改写
- `validation-fundamentals/fund-rest`: expected 基础卷-完整答案 > 基础卷-部分正确
- `validation-fundamentals/fund-rest`: expected 基础卷-完整答案 > 基础卷-错误答案
- `validation-fundamentals/fund-rest`: expected 基础卷-同义改写 > 基础卷-部分正确
- `validation-fundamentals/fund-rest`: expected 基础卷-同义改写 > 基础卷-错误答案
- `validation-fundamentals/fund-rest`: expected 基础卷-部分正确 > 基础卷-错误答案
- `validation-fundamentals/fund-status`: expected 基础卷-完整答案 > 基础卷-同义改写
- `validation-fundamentals/fund-status`: expected 基础卷-完整答案 > 基础卷-部分正确
- `validation-fundamentals/fund-status`: expected 基础卷-完整答案 > 基础卷-错误答案
- `validation-fundamentals/fund-status`: expected 基础卷-同义改写 > 基础卷-部分正确
- `validation-fundamentals/fund-status`: expected 基础卷-同义改写 > 基础卷-错误答案
- `validation-fundamentals/fund-status`: expected 基础卷-部分正确 > 基础卷-错误答案
- `validation-backend/back-tx`: expected 后端卷-完整答案 > 后端卷-部分正确
- `validation-backend/back-tx`: expected 后端卷-完整答案 > 后端卷-错误答案
- `validation-backend/back-tx`: expected 后端卷-部分正确 > 后端卷-错误答案
- `validation-backend/back-code`: expected 后端卷-完整答案 > 后端卷-部分正确
- `validation-backend/back-code`: expected 后端卷-完整答案 > 后端卷-错误答案
- `validation-backend/back-code`: expected 后端卷-部分正确 > 后端卷-错误答案

## Workflow Errors

- `VAL-CODE-003`: HTTPStatusError: Client error '429 Too Many Requests' for url 'http://testserver/api/submit'
For more information check: https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/429

## Interpretation Notes

- Band hit rate measures agreement with predeclared expert-style score ranges.
- Ordering accuracy checks whether stronger answers score above weaker answers.
- Semantic reranking can overvalue related wording with an incorrect conclusion; review failed ordering checks manually.
