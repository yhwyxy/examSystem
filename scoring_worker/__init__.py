"""scoring_worker: 异步主观题评分 worker (plan Task 7).

独立 uv 工程, 不复用根目录 backend (防 import 耦合); 通过 PostgreSQL
grading_jobs 队列 (claim/lease/complete) + 调用第三方 subjective-scoring
包给主观题打分; 客观 / 主观 detail 合并按快照问题顺序重建完整数组.

依赖:
    - PostgreSQL (psycopg 3) grading_jobs / submissions / exam_runs
    - subjective-scoring>=0.1.7 (PyPI 拉取 git tag v0.1.7)
"""
