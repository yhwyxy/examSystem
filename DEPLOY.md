# 部署导向索引

生产栈: **Go (exam-server) + PostgreSQL + scoring_worker (Python 评分)** 双进程.

> 旧 Python+SQLite 栈已从仓库移除 (切换完成), 如需查阅参见 git 历史.

## Go + PostgreSQL 栈

- 完整部署流程: [docs/deployment-go-pg.md](docs/deployment-go-pg.md)
- 部署后健康自检: `scoring_worker --preflight` + `Invoke-RestMethod /api/health`

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
