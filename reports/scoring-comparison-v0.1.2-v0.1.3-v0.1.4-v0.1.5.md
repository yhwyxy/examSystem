# Scoring Comparison: v0.1.2 → v0.1.3 → v0.1.4 → v0.1.5

## Version Metrics

| Version | Workflow | Errors | Band hit | MAE | Ordering |
|---|---:|---:|---:|---:|---:|
| v0.1.2 | 100.0% | 0 | 89.4% | 4.911 | 92.0% |
| v0.1.3 | 100.0% | 0 | 90.9% | 5.386 | 92.0% |
| v0.1.4 | 100.0% | 0 | 90.9% | 5.388 | 88.0% |
| v0.1.5 | 100.0% | 0 | 92.4% | 5.583 | 88.0% |

## Specialized Paper Totals

| Paper | Quality | v0.1.2 | v0.1.3 | v0.1.4 | v0.1.5 | Δ v0.1.2→v0.1.3 | Δ v0.1.3→v0.1.4 | Δ v0.1.4→v0.1.5 | Δ overall |
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|
| code-scoring-specialist | complete | 99.0 | 99.0 | 99.0 | 99.0 | +0.0 | +0.0 | +0.0 | +0.0 |
| code-scoring-specialist | partial | 74.6 | 74.6 | 74.6 | 74.6 | +0.0 | +0.0 | +0.0 | +0.0 |
| code-scoring-specialist | wrong | 14.4 | 14.4 | 14.4 | 14.4 | +0.0 | +0.0 | +0.0 | +0.0 |
| sql-scoring-specialist | complete | 100.0 | 100.0 | 100.0 | 100.0 | +0.0 | +0.0 | +0.0 | +0.0 |
| sql-scoring-specialist | partial | 52.3 | 52.3 | 52.3 | 52.3 | +0.0 | +0.0 | +0.0 | +0.0 |
| sql-scoring-specialist | wrong | 0.0 | 0.0 | 0.0 | 0.0 | +0.0 | +0.0 | +0.0 | +0.0 |
| text-scoring-specialist | complete | 62.4 | 42.8 | 85.0 | 89.4 | -19.6 | +42.2 | +4.4 | +27.0 |
| text-scoring-specialist | partial | 45.4 | 36.5 | 54.8 | 59.6 | -8.9 | +18.3 | +4.8 | +14.2 |
| text-scoring-specialist | wrong | 26.8 | 15.3 | 16.6 | 0.0 | -11.5 | +1.3 | -16.6 | -26.8 |

## Quality Ordering

- `v0.1.2` `code-scoring-specialist`: pass
- `v0.1.2` `sql-scoring-specialist`: pass
- `v0.1.2` `text-scoring-specialist`: pass
- `v0.1.3` `code-scoring-specialist`: pass
- `v0.1.3` `sql-scoring-specialist`: pass
- `v0.1.3` `text-scoring-specialist`: pass
- `v0.1.4` `code-scoring-specialist`: pass
- `v0.1.4` `sql-scoring-specialist`: pass
- `v0.1.4` `text-scoring-specialist`: pass
- `v0.1.5` `code-scoring-specialist`: pass
- `v0.1.5` `sql-scoring-specialist`: pass
- `v0.1.5` `text-scoring-specialist`: pass

## Largest Question Changes: v0.1.4 → v0.1.5

| Paper | Quality | Question | Previous | Latest | Delta |
|---|---|---|---:|---:|---:|
| text-scoring-specialist | wrong | text-5 | 9.9 | 0.0 | -9.9 |
| text-scoring-specialist | wrong | text-3 | 6.7 | 0.0 | -6.7 |
| text-scoring-specialist | partial | text-2 | 0.0 | 5.0 | +5.0 |
| text-scoring-specialist | complete | text-4 | 15.0 | 19.4 | +4.4 |
| text-scoring-specialist | partial | text-1 | 18.9 | 17.1 | -1.8 |
| text-scoring-specialist | partial | text-5 | 10.9 | 12.5 | +1.6 |
| code-scoring-specialist | partial | code-1 | 16.4 | 16.4 | +0.0 |
| text-scoring-specialist | complete | text-2 | 20.0 | 20.0 | +0.0 |
| sql-scoring-specialist | wrong | sql-2 | 0.0 | 0.0 | +0.0 |
| code-scoring-specialist | complete | code-3 | 19.9 | 19.9 | +0.0 |
