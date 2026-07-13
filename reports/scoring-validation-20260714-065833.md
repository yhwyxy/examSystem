# Scoring System Validation Report

- Generated: `2026-07-13T22:58:33.042724+00:00`
- Remote URL: `https://router.tumuer.me/v1/rerank`
- Remote model: `Pro/BAAI/bge-reranker-v2-m3`
- Planned submissions: `16`
- Completed submissions: `16`
- Workflow success rate: `100.0%`
- Band hit rate: `92.4%`
- Mean absolute error: `5.583`
- Ordering accuracy: `88.0%`
- Scoring errors: `0`
- Production data unchanged: `True`

## Per-answer Results

| Paper | Candidate | Quality | Question | Expected | Actual | In band | Method | Confidence |
|---|---|---|---|---:|---:|:---:|---|---:|
| validation-fundamentals | 基础卷-完整答案 | complete | fund-choice | 5.0-5.0 | 5.0 | yes | objective_rule | 1.0000 |
| validation-fundamentals | 基础卷-完整答案 | complete | fund-rest | 9.0-10.0 | 10.0 | yes | subjective_scoring:TextRerankerScorer | 1.0000 |
| validation-fundamentals | 基础卷-完整答案 | complete | fund-status | 9.0-10.0 | 10.0 | yes | subjective_scoring:TextRerankerScorer | 1.0000 |
| validation-fundamentals | 基础卷-同义改写 | paraphrase | fund-choice | 5.0-5.0 | 5.0 | yes | objective_rule | 1.0000 |
| validation-fundamentals | 基础卷-同义改写 | paraphrase | fund-rest | 8.0-10.0 | 3.0 | no | subjective_scoring:TextRerankerScorer | 0.5500 |
| validation-fundamentals | 基础卷-同义改写 | paraphrase | fund-status | 8.0-10.0 | 10.0 | yes | subjective_scoring:TextRerankerScorer | 1.0000 |
| validation-fundamentals | 基础卷-部分正确 | partial | fund-choice | 0.0-0.0 | 0.0 | yes | objective_rule | 1.0000 |
| validation-fundamentals | 基础卷-部分正确 | partial | fund-rest | 3.0-5.0 | 4.0 | yes | subjective_scoring:TextRerankerScorer | 0.8159 |
| validation-fundamentals | 基础卷-部分正确 | partial | fund-status | 4.0-7.0 | 9.8 | no | subjective_scoring:TextRerankerScorer | 0.9764 |
| validation-fundamentals | 基础卷-错误答案 | wrong | fund-choice | 0.0-0.0 | 0.0 | yes | objective_rule | 1.0000 |
| validation-fundamentals | 基础卷-错误答案 | wrong | fund-rest | 0.0-2.0 | 0.0 | yes | subjective_scoring:TextRerankerScorer | 0.5500 |
| validation-fundamentals | 基础卷-错误答案 | wrong | fund-status | 0.0-1.0 | 0.0 | yes | subjective_scoring:TextRerankerScorer | 0.4000 |
| validation-backend | 后端卷-完整答案 | complete | back-tx | 9.0-10.0 | 10.0 | yes | subjective_scoring:TextRerankerScorer | 1.0000 |
| validation-backend | 后端卷-完整答案 | complete | back-code | 9.0-10.0 | 10.0 | yes | subjective_scoring:CodeHybridScorer | 0.9983 |
| validation-backend | 后端卷-完整答案 | complete | back-sql | 9.0-10.0 | 10.0 | yes | subjective_scoring:SQLStructureScorer | 1.0000 |
| validation-backend | 后端卷-部分正确 | partial | back-tx | 3.0-5.0 | 4.0 | yes | subjective_scoring:TextRerankerScorer | 0.4000 |
| validation-backend | 后端卷-部分正确 | partial | back-code | 4.0-6.0 | 8.7 | no | subjective_scoring:CodeHybridScorer | 0.5500 |
| validation-backend | 后端卷-部分正确 | partial | back-sql | 5.0-7.0 | 6.9 | yes | subjective_scoring:SQLStructureScorer | 0.6912 |
| validation-backend | 后端卷-错误答案 | wrong | back-tx | 0.0-2.0 | 5.2 | no | subjective_scoring:TextRerankerScorer | 0.4000 |
| validation-backend | 后端卷-错误答案 | wrong | back-code | 0.0-2.0 | 8.4 | no | subjective_scoring:CodeHybridScorer | 0.5500 |
| validation-backend | 后端卷-错误答案 | wrong | back-sql | 0.0-1.0 | 0.0 | yes | subjective_scoring:SQLStructureScorer | 0.0000 |
| text-scoring-specialist | 文本主观题评分专项卷-complete | complete | text-1 | 0.0-20.0 | 15.0 | yes | subjective_scoring:TextRerankerScorer | 0.4000 |
| text-scoring-specialist | 文本主观题评分专项卷-complete | complete | text-2 | 0.0-20.0 | 20.0 | yes | subjective_scoring:TextRerankerScorer | 1.0000 |
| text-scoring-specialist | 文本主观题评分专项卷-complete | complete | text-3 | 0.0-20.0 | 15.0 | yes | subjective_scoring:TextRerankerScorer | 0.4000 |
| text-scoring-specialist | 文本主观题评分专项卷-complete | complete | text-4 | 0.0-20.0 | 19.4 | yes | subjective_scoring:TextRerankerScorer | 0.9691 |
| text-scoring-specialist | 文本主观题评分专项卷-complete | complete | text-5 | 0.0-20.0 | 20.0 | yes | subjective_scoring:TextRerankerScorer | 1.0000 |
| text-scoring-specialist | 文本主观题评分专项卷-partial | partial | text-1 | 0.0-20.0 | 17.1 | yes | subjective_scoring:TextRerankerScorer | 0.8524 |
| text-scoring-specialist | 文本主观题评分专项卷-partial | partial | text-2 | 0.0-20.0 | 5.0 | yes | subjective_scoring:TextRerankerScorer | 0.4000 |
| text-scoring-specialist | 文本主观题评分专项卷-partial | partial | text-3 | 0.0-20.0 | 10.0 | yes | subjective_scoring:TextRerankerScorer | 0.9097 |
| text-scoring-specialist | 文本主观题评分专项卷-partial | partial | text-4 | 0.0-20.0 | 15.0 | yes | subjective_scoring:TextRerankerScorer | 0.9987 |
| text-scoring-specialist | 文本主观题评分专项卷-partial | partial | text-5 | 0.0-20.0 | 12.5 | yes | subjective_scoring:TextRerankerScorer | 0.8680 |
| text-scoring-specialist | 文本主观题评分专项卷-wrong | wrong | text-1 | 0.0-20.0 | 0.0 | yes | subjective_scoring:TextRerankerScorer | 0.5500 |
| text-scoring-specialist | 文本主观题评分专项卷-wrong | wrong | text-2 | 0.0-20.0 | 0.0 | yes | subjective_scoring:TextRerankerScorer | 0.4000 |
| text-scoring-specialist | 文本主观题评分专项卷-wrong | wrong | text-3 | 0.0-20.0 | 0.0 | yes | subjective_scoring:TextRerankerScorer | 0.5500 |
| text-scoring-specialist | 文本主观题评分专项卷-wrong | wrong | text-4 | 0.0-20.0 | 0.0 | yes | subjective_scoring:TextRerankerScorer | 0.5500 |
| text-scoring-specialist | 文本主观题评分专项卷-wrong | wrong | text-5 | 0.0-20.0 | 0.0 | yes | subjective_scoring:TextRerankerScorer | 0.5500 |
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
| code-scoring-specialist | Python 代码评分专项卷-complete | complete | code-1 | 0.0-20.0 | 20.0 | yes | subjective_scoring:CodeHybridScorer | 1.0000 |
| code-scoring-specialist | Python 代码评分专项卷-complete | complete | code-2 | 0.0-20.0 | 20.0 | yes | subjective_scoring:CodeHybridScorer | 0.9991 |
| code-scoring-specialist | Python 代码评分专项卷-complete | complete | code-3 | 0.0-20.0 | 19.9 | yes | subjective_scoring:CodeHybridScorer | 0.9936 |
| code-scoring-specialist | Python 代码评分专项卷-complete | complete | code-4 | 0.0-20.0 | 19.8 | yes | subjective_scoring:CodeHybridScorer | 0.9909 |
| code-scoring-specialist | Python 代码评分专项卷-complete | complete | code-5 | 0.0-20.0 | 19.3 | yes | subjective_scoring:CodeHybridScorer | 0.9665 |
| code-scoring-specialist | Python 代码评分专项卷-partial | partial | code-1 | 0.0-20.0 | 16.4 | yes | subjective_scoring:CodeHybridScorer | 0.8202 |
| code-scoring-specialist | Python 代码评分专项卷-partial | partial | code-2 | 0.0-20.0 | 16.7 | yes | subjective_scoring:CodeHybridScorer | 0.5500 |
| code-scoring-specialist | Python 代码评分专项卷-partial | partial | code-3 | 0.0-20.0 | 14.7 | yes | subjective_scoring:CodeHybridScorer | 0.7327 |
| code-scoring-specialist | Python 代码评分专项卷-partial | partial | code-4 | 0.0-20.0 | 16.8 | yes | subjective_scoring:CodeHybridScorer | 0.5500 |
| code-scoring-specialist | Python 代码评分专项卷-partial | partial | code-5 | 0.0-20.0 | 10.0 | yes | subjective_scoring:CodeHybridScorer | 0.5500 |
| code-scoring-specialist | Python 代码评分专项卷-wrong | wrong | code-1 | 0.0-20.0 | 4.0 | yes | subjective_scoring:CodeHybridScorer | 0.2014 |
| code-scoring-specialist | Python 代码评分专项卷-wrong | wrong | code-2 | 0.0-20.0 | 2.6 | yes | subjective_scoring:CodeHybridScorer | 0.1280 |
| code-scoring-specialist | Python 代码评分专项卷-wrong | wrong | code-3 | 0.0-20.0 | 2.6 | yes | subjective_scoring:CodeHybridScorer | 0.1282 |
| code-scoring-specialist | Python 代码评分专项卷-wrong | wrong | code-4 | 0.0-20.0 | 2.6 | yes | subjective_scoring:CodeHybridScorer | 0.1288 |
| code-scoring-specialist | Python 代码评分专项卷-wrong | wrong | code-5 | 0.0-20.0 | 2.6 | yes | subjective_scoring:CodeHybridScorer | 0.1279 |

## Failed Ordering Checks

- `validation-fundamentals/fund-rest`: expected 基础卷-同义改写 > 基础卷-部分正确
- `validation-fundamentals/fund-status`: expected 基础卷-完整答案 > 基础卷-同义改写
- `validation-backend/back-tx`: expected 后端卷-部分正确 > 后端卷-错误答案

## Interpretation Notes

- Band hit rate measures agreement with predeclared expert-style score ranges.
- Ordering accuracy checks whether stronger answers score above weaker answers.
- Semantic reranking can overvalue related wording with an incorrect conclusion; review failed ordering checks manually.
