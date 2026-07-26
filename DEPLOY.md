# 部署导向索引

本仓库现为**双栈并存**: 新 Go+PostgreSQL 栈 (主力) 与 Python+SQLite 旧栈 (回滚备份).

## Go + PostgreSQL 栈 (主力)

- 完整部署流程: [docs/deployment-go-pg.md](docs/deployment-go-pg.md)
- 回滚流程: [docs/rollback-go-pg.md](docs/rollback-go-pg.md)
- 迁移收尾 + 已知遗留清单: [docs/cutover-go-pg.md](docs/cutover-go-pg.md)
- 部署后健康自检: `scoring_worker --preflight` + `Invoke-RestMethod /api/health`

## Python + SQLite 旧栈 (回滚备份)

旧栈仅保留作 Go 标线上线后的应急回退通道, 不再新增功能. 维护说明见仓库根 README.md 旧栈段落.

## 关键运维命令速查

```powershell
# 打生产包
pwsh scripts/windows/package.ps1
# 起服务 (前提: ScheduledTask 已一次性创建, 见 deployment-go-pg.md)
pwsh scripts/windows/start.ps1
# 停服务
pwsh scripts/windows/stop.ps1
# 评分链路健康自检 (不污染数据)
python -m scoring_worker --preflight
# API 健康
Invoke-RestMethod http://127.0.0.1:18080/api/health
```
