# Scoring System Validation Report

- Generated: `2026-07-12T07:43:11.081374+00:00`
- Remote URL: `https://router.tumuer.me/v1/rerank`
- Remote model: `Pro/BAAI/bge-reranker-v2-m3`
- Planned submissions: `7`
- Completed submissions: `7`
- Workflow success rate: `100.0%`
- Band hit rate: `47.6%`
- Mean absolute error: `2.643`
- Ordering accuracy: `88.0%`
- Scoring errors: `0`
- Production data unchanged: `True`

## Per-answer Results

| Paper | Candidate | Quality | Question | Expected | Actual | In band | Method | Confidence |
|---|---|---|---|---:|---:|:---:|---|---:|
| validation-fundamentals | 基础卷-完整答案 | complete | fund-choice | 5.0-5.0 | 5.0 | yes | objective_rule | 1.0000 |
| validation-fundamentals | 基础卷-完整答案 | complete | fund-rest | 9.0-10.0 | 1.4 | no | subjective_scoring:TextRerankerScorer | 0.2805 |
| validation-fundamentals | 基础卷-完整答案 | complete | fund-status | 9.0-10.0 | 5.3 | no | subjective_scoring:TextRerankerScorer | 0.4000 |
| validation-fundamentals | 基础卷-同义改写 | paraphrase | fund-choice | 5.0-5.0 | 5.0 | yes | objective_rule | 1.0000 |
| validation-fundamentals | 基础卷-同义改写 | paraphrase | fund-rest | 8.0-10.0 | 0.0 | no | subjective_scoring:TextRerankerScorer | 0.0861 |
| validation-fundamentals | 基础卷-同义改写 | paraphrase | fund-status | 8.0-10.0 | 2.2 | no | subjective_scoring:TextRerankerScorer | 0.2786 |
| validation-fundamentals | 基础卷-部分正确 | partial | fund-choice | 0.0-0.0 | 0.0 | yes | objective_rule | 1.0000 |
| validation-fundamentals | 基础卷-部分正确 | partial | fund-rest | 3.0-5.0 | 2.8 | no | subjective_scoring:TextRerankerScorer | 0.2845 |
| validation-fundamentals | 基础卷-部分正确 | partial | fund-status | 4.0-7.0 | 2.0 | no | subjective_scoring:TextRerankerScorer | 0.2661 |
| validation-fundamentals | 基础卷-错误答案 | wrong | fund-choice | 0.0-0.0 | 0.0 | yes | objective_rule | 1.0000 |
| validation-fundamentals | 基础卷-错误答案 | wrong | fund-rest | 0.0-2.0 | 0.0 | yes | subjective_scoring:TextRerankerScorer | 0.0019 |
| validation-fundamentals | 基础卷-错误答案 | wrong | fund-status | 0.0-1.0 | 0.0 | yes | subjective_scoring:TextRerankerScorer | 0.0024 |
| validation-backend | 后端卷-完整答案 | complete | back-tx | 9.0-10.0 | 6.9 | no | subjective_scoring:TextRerankerScorer | 0.4000 |
| validation-backend | 后端卷-完整答案 | complete | back-code | 9.0-10.0 | 10.0 | yes | subjective_scoring:CodeHybridScorer | 0.9983 |
| validation-backend | 后端卷-完整答案 | complete | back-sql | 9.0-10.0 | 10.0 | yes | subjective_scoring:SQLStructureScorer | 1.0000 |
| validation-backend | 后端卷-部分正确 | partial | back-tx | 3.0-5.0 | 4.0 | yes | subjective_scoring:TextRerankerScorer | 0.4000 |
| validation-backend | 后端卷-部分正确 | partial | back-code | 4.0-6.0 | 8.6 | no | subjective_scoring:CodeHybridScorer | 0.8630 |
| validation-backend | 后端卷-部分正确 | partial | back-sql | 5.0-7.0 | 7.9 | no | subjective_scoring:SQLStructureScorer | 0.7900 |
| validation-backend | 后端卷-错误答案 | wrong | back-tx | 0.0-2.0 | 0.2 | yes | subjective_scoring:TextRerankerScorer | 0.0576 |
| validation-backend | 后端卷-错误答案 | wrong | back-code | 0.0-2.0 | 8.2 | no | subjective_scoring:CodeHybridScorer | 0.8198 |
| validation-backend | 后端卷-错误答案 | wrong | back-sql | 0.0-1.0 | 4.6 | no | subjective_scoring:SQLStructureScorer | 0.4600 |

## Failed Ordering Checks

- `validation-fundamentals/fund-rest`: expected 基础卷-完整答案 > 基础卷-部分正确
- `validation-fundamentals/fund-rest`: expected 基础卷-同义改写 > 基础卷-部分正确
- `validation-fundamentals/fund-rest`: expected 基础卷-同义改写 > 基础卷-错误答案

## Interpretation Notes

- Band hit rate measures agreement with predeclared expert-style score ranges.
- Ordering accuracy checks whether stronger answers score above weaker answers.
- Semantic reranking can overvalue related wording with an incorrect conclusion; review failed ordering checks manually.
