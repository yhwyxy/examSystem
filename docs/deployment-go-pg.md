# 部署指南: Go + PostgreSQL 后端 (Task 12 Step 6)

适用: exam-server.exe (Go) + PostgreSQL 18 + scoring_worker (Python 评分) 双进程栈.
替代 Python+SQLite 旧栈, 支撑更高并发. Python 旧栈保留作回滚备份 (见 rollback-go-pg.md).

## 前置 (一次性)

1. PostgreSQL 18 已起, 库 exam_system 已建, 角色 exam_app (DML) / exam_migrator (DDL+DML).
2. migrations/0001_initial.sql 已由 exam_migrator 执行 (无 GRANT, 需手动 `GRANT` 全 DML 给 exam_app).
3. 产 dist 包: `pwsh scripts/windows/package.ps1` → `dist/windows/exam-system/`.
4. 一次性创建 ScheduledTask (命令见下文"启动").

## 目录布局 (不携带 Data/Model)

- 部署根 `D:\exam-system\` (举例; 实际由 dist 解压)
  - `exam-server.exe`           Go API 二进制 (~22MB PE32+)
  - `config.production.yaml`    从 config.production.example.yaml 复制并填值
  - `frontend/`                  Go serve 静态资源
  - `scoring_worker/`            Python 评分 worker 包
  - `scripts/windows/`           start.ps1 / stop.ps1 / package.ps1
  - `docs/`                      本文档 + rollback-go-pg.md
  - `manifest.sha256`            完整性核对清单
- DataRoot (独立持久目录, **不在包内**): `D:\exam-data\` (PG 数据 / uploads / backups)
- ModelRoot (独立持久目录, **不在包内**): `D:\exam-models\bge-reranker-v2-m3\`
  - 跨版本持久, 升级不覆盖 (升级只替换部署根的程序文件)

## 升级 (无 install.ps1, 手动 zip + 哈希校验)

1. 产新 dist: `pwsh scripts/windows/package.ps1` → `dist/windows/exam-system/`.
2. 传输到部署点, 解压到临时目录 `D:\exam-new\`.
3. 核对完整性: `Get-FileHash` 重算 dist 内文件 → 与 `manifest.sha256` 比对, 任一不符中止.
4. stop.ps1 停服务 (见下).
5. 覆盖部署根程序文件 (白名单: exam-server.exe / config.production.example.yaml /
   frontend/ / scoring_worker/ / scripts/windows/*.ps1 / docs/*.md).
   - **禁止**覆盖 `config.production.yaml` (含生产密钥, 不在包内).
   - **禁止**触碰 DataRoot / ModelRoot.
6. start.ps1 起服务, 等 /api/health 绿.
7. 失败回滚: 见 docs/rollback-go-pg.md.

## 启动 (start.ps1)

前提: API / Worker 两个 ScheduledTask 需提前一次性创建. 一次性命令 (PowerShell 管理员):

```powershell
# API task (运行 exam-server.exe serve)
$api = New-ScheduledTaskAction -Execute "D:\exam-system\exam-server.exe" `
    -Argument "serve -bind 0.0.0.0:18080"
$trig = New-ScheduledTaskTrigger -AtStartup
Register-ScheduledTask -TaskName "ExamAPI" -Action $api -Trigger $trig `
    -RunLevel Highest -Force

# Worker task (运行 scoring_worker 轮询主循环)
$py = "C:\exam-venv\Scripts\python.exe"   # 生产 Python 3.13 venv
$worker = New-ScheduledTaskAction -Execute $py `
    -Argument "-m scoring_worker" -WorkingDirectory "D:\exam-system"
Register-ScheduledTask -TaskName "ExamWorker" -Action $worker -Trigger $trig `
    -RunLevel Highest -Force
```

日常启停: `pwsh scripts/windows/start.ps1` / `pwsh scripts/windows/stop.ps1`.
start.ps1 会轮询 `/api/health` 最多 `$HealthTimeoutSec` 秒确认 API 就绪.

## 本地模型加载 (RERANKER_MODEL, 方案 C 双形态)

`scoring_worker/grader_bridge.py` 自动按值类型分流:

| `RERANKER_MODEL` 值形态 | 行为 | 适用场景 |
|---|---|---|
| 存在的目录路径 (绝对或相对部署根) | 按本地目录加载 CrossEncoder | 容器化 / 离线生产部署 |
| 不存在的字符串 (如 `BAAI/bge-reranker-v2-m3`) | 当 HF repo id, 先查 `HF_HOME` cache, 未命中走 HF Hub 下载 | 开发 / 复用已下载 cache |

⚠️ **易踩坑**: `RERANKER_MODEL` 指向 HF cache 顶层目录 (如 `models/models--BAAI--bge-reranker-v2-m3`)
**会失败** — 该目录无 config.json. 必须指向 `snapshots/<hash>/` 子目录, 或直接用 repo id 让
sentence-transformers 自己解析 cache.

生产推荐: 显式绝对路径指向解包好的模型目录, 配合 `HF_HUB_OFFLINE=1` 强制离线.

| 变量 | 示例值 | 说明 |
|---|---|---|
| `RERANKER_MODEL` | `D:\exam-models\bge-reranker-v2-m3` | 本地路径 / HF repo id |
| `RERANK_USE_REMOTE` | `false` | 本地模型; `true` 用远程 reranker API |
| `RERANK_MODEL` | `Pro/BAAI/bge-reranker-v2-m3` | 远程模式下的 model 名 (本地模式忽略) |
| `RERANK_API_URL` / `RERANK_API_KEY` | — | 远程模式用 |
| `HF_HOME` | `D:\exam-models` | 本地 repo id 模式的 cache 根 |
| `DATABASE_URL` | `postgres://exam_app:***@127.0.0.1:5432/exam_system` | worker 必须 |
| `WORKER_ID` | `prod-worker-1` | 多实例区分 claim/续租 |
| `LOG_LEVEL` | `INFO` | DEBUG/INFO/WARNING |

## 部署后健康自检

### scoring_worker --preflight (评分链路健康)

不 claim job / 不写 DB, 用固定无敏感 fixture 跑一次真实评分, 三层断言拦:
1. lexical fallback (reranker/judge 完全不可用)
2. reranker 降级 warning (如 "语义模型不可用, 已回退到词法相似度")
3. score=0 退化 (fixture 有合理答案不应 0 分)

```powershell
$env:RERANKER_MODEL="D:\exam-models\bge-reranker-v2-m3"
$env:RERANK_USE_REMOTE="false"
$env:DATABASE_URL="postgres://exam_app:***@127.0.0.1:5432/exam_system"
$env:WORKER_ID="preflight"; $env:LOG_LEVEL="INFO"
C:\exam-venv\Scripts\python.exe -m scoring_worker --preflight
# exit 0 = 通过; exit 2 = 检测到降级/异常, 不应放行
```

### API health

```powershell
Invoke-RestMethod http://127.0.0.1:18080/api/health
```

## 日志轮转

- API: `exam-server` 内置 lumberjack, 20MB × 10 保留, 自动滚动 (见 cmd/exam-server main.go).
- Worker: ScheduledTask 输出重定向到日志文件, 运维侧按需配置轮转.

## 备份

- PG 逻辑备份: `pg_dump -U exam_migrator exam_system > backup_$(date).sql`.
- DataRoot: `D:\exam-data\uploads\` 直接归档.
- 升级前**强制**先全量备份 (rollback 回退点).
- 日志 ≠ 备份: 按 12-Factor 把日志当事件流, 不依赖日志做数据恢复.

## 常见故障

| 现象 | 根因 / 处置 |
|---|---|
| preflight exit 2 + warning "回退到词法相似度" | `RERANKER_MODEL` 指向 HF cache 顶层 (无 config.json); 改指 `snapshots/<hash>/` 或用 repo id |
| preflight exit 2 + score=0 | reranker 加载失败但 warnings 未显形; 检查模型路径 / venv 依赖 / 磁盘 |
| API 起不来 "ScheduledTask 不存在" | 未一次性 New-ScheduledTask; 见"启动"章节 |
| worker claim 拒绝 "exam_app 无 INSERT 权限" | migrations 无 GRANT; 手动 `GRANT ALL ON ALL ... TO exam_app` |
| Go serve 404 前端 | dist 缺 frontend/; package.ps1 时 frontend/ 不存在会 warning 但继续 |
