# Scoring Comparison: v0.1.2 → v0.1.3 → v0.1.4

## Version Metrics

| Version | Workflow | Errors | Band hit | MAE | Ordering |
|---|---:|---:|---:|---:|---:|
| v0.1.2 | 100.0% | 0 | 89.4% | 4.911 | 92.0% |
| v0.1.3 | 100.0% | 0 | 90.9% | 5.386 | 92.0% |
| v0.1.4 | 100.0% | 0 | 90.9% | 5.388 | 88.0% |

## Specialized Paper Totals

| Paper | Quality | v0.1.2 | v0.1.3 | v0.1.4 | Δ v0.1.2→v0.1.3 | Δ v0.1.3→v0.1.4 | Δ overall |
|---|---|---:|---:|---:|---:|---:|---:|
| code-scoring-specialist | complete | 99.0 | 99.0 | 99.0 | +0.0 | +0.0 | +0.0 |
| code-scoring-specialist | partial | 74.6 | 74.6 | 74.6 | +0.0 | +0.0 | +0.0 |
| code-scoring-specialist | wrong | 14.4 | 14.4 | 14.4 | +0.0 | +0.0 | +0.0 |
| sql-scoring-specialist | complete | 100.0 | 100.0 | 100.0 | +0.0 | +0.0 | +0.0 |
| sql-scoring-specialist | partial | 52.3 | 52.3 | 52.3 | +0.0 | +0.0 | +0.0 |
| sql-scoring-specialist | wrong | 0.0 | 0.0 | 0.0 | +0.0 | +0.0 | +0.0 |
| text-scoring-specialist | complete | 62.4 | 42.8 | 85.0 | -19.6 | +42.2 | +22.6 |
| text-scoring-specialist | partial | 45.4 | 36.5 | 54.8 | -8.9 | +18.3 | +9.4 |
| text-scoring-specialist | wrong | 26.8 | 15.3 | 16.6 | -11.5 | +1.3 | -10.2 |

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

## Largest Question Changes: v0.1.3 → v0.1.4

| Paper | Quality | Question | Previous | Latest | Delta |
|---|---|---|---:|---:|---:|
| text-scoring-specialist | complete | text-1 | 0.0 | 15.0 | +15.0 |
| text-scoring-specialist | complete | text-3 | 0.0 | 15.0 | +15.0 |
| text-scoring-specialist | partial | text-3 | 0.0 | 10.0 | +10.0 |
| text-scoring-specialist | complete | text-4 | 5.0 | 15.0 | +10.0 |
| text-scoring-specialist | partial | text-1 | 12.4 | 18.9 | +6.5 |
| text-scoring-specialist | partial | text-2 | 5.0 | 0.0 | -5.0 |
| text-scoring-specialist | partial | text-4 | 10.0 | 15.0 | +5.0 |
| text-scoring-specialist | complete | text-5 | 17.8 | 20.0 | +2.2 |
| text-scoring-specialist | partial | text-5 | 9.1 | 10.9 | +1.8 |
| text-scoring-specialist | wrong | text-3 | 5.4 | 6.7 | +1.3 |
