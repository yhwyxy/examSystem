# 企业在线考试兼批改系统

**Go + PostgreSQL** 后端 (`cmd/exam-server/`) + **Python 评分 worker** (`scoring_worker/`) 双进程栈：
员工扫码答题、客观题自动判分、主观题多引擎评分（独立库
[subjective-scoring](https://github.com/yhwyxy/subjective-scoring)：文本评分点 / SQL AST / 代码混合）、
管理员人工复核与成绩导出。

- 部署: [DEPLOY.md](DEPLOY.md) → [docs/deployment-go-pg.md](docs/deployment-go-pg.md)
- 系统设计: [docs/design.md](docs/design.md)
- 部署后自检: `python -m scoring_worker --preflight` + `GET /api/health`

> 历史说明: 本仓库曾为 Go+PG 与 Python+SQLite 双栈并存，旧 Python 栈 (`backend/`,
> `main.py`) 已于切换完成后移除，如需查阅参见 git 历史。

## 快速启动

```bash
# 1. 数据库 (本机已有 PostgreSQL 时跳过; compose 版映射到宿主 5433)
docker compose -f docker-compose.postgres.yml up -d

# 2. 迁移 + 启动 Go API (静态页一并托管)
go run ./cmd/exam-server migrate --config config.dev.yaml
go run ./cmd/exam-server serve   --config config.dev.yaml --bind :8000 --static frontend

# 3. 评分 worker (主观题; 独立进程, 经 PostgreSQL grading_jobs 队列衔接)
DATABASE_URL='postgres://...' python -m scoring_worker
```

访问：

- 员工考试端：`http://localhost:8000/exam?paper=专业编码&run=轮次token`（由管理端发布后生成）
- 管理后台：http://localhost:8000/admin（试卷录入 / 考试管理）

## Windows 10 源码部署（非 Docker）

### 1. 软件清单（按顺序安装）

- `Git` — 拉取仓库；`uv sync` 拉取 `subjective-scoring`（GitHub tag）也需要它
- `Go 1.25+` — `go.mod` 写死 `go 1.25.0`，装低了会直接报错
- `PostgreSQL 18` — EDB 官方安装包，自带 `psql` / `pg_isready`，需加入 PATH
- `Python 3.12.x` — **必须 3.12**，`scoring_worker` 限定 `>=3.12,<3.13`，3.13 不支持
- `uv` — 依赖管理；老版本没有 `uv pip download` / `uv sync --find-links`，装完先 `uv self update`
- `PowerShell 7 (pwsh)` — 所有 `scripts/windows/*.ps1` 均按 pwsh 编写（无 winget 时从
  GitHub Releases 下载 `PowerShell-*-win-x64.msi`）

不需要 `Node.js`（前端是纯静态文件，由 Go 托管），不需要 Docker。

### 2. 一次性初始化（建库 + 迁移）

```powershell
# 建库 exam_system 与角色 exam_app / exam_migrator（可重入）
pwsh scripts\windows\setup-dev.ps1
# 或手动: psql -U postgres -d postgres -f scripts\pg-bootstrap.sql

# 执行迁移建表
go run ./cmd/exam-server migrate --config config.dev.yaml
```

`config.dev.yaml` 默认连本机 PostgreSQL 的 **5432**（docker compose 才映射 5433），换机部署按实际端口改。

### 3. 依赖安装

联网环境（推荐）：

```powershell
uv sync --extra scoring --extra dev --extra embedding
```

完全离线（`packages/` 离线 wheel）：

```powershell
# 1) 在联网机器补齐 Windows 条件依赖 + 测试依赖（见下方"常见报错"）
python -m pip download colorama tzdata pytest -d .\packages

# 2) 离线同步（--no-sources 会忽略 git 源、从 packages/ 解析，并重写 uv.lock）
uv sync --extra scoring --extra embedding --offline --no-index --no-sources --find-links .\packages --no-dev
```

生产部署 worker 用官方离线脚本（pip 模式，不碰 uv.lock）：

```powershell
pwsh scripts\windows\install-worker.ps1
```

### 4. 手动启动（两个进程，两个窗口）

窗口 A — Go API（仓库根目录）：

```powershell
go run ./cmd/exam-server serve --config config.dev.yaml --bind :8000 --static frontend
```

验证：`Invoke-RestMethod http://127.0.0.1:8000/api/health` 返回 ok；管理后台
http://127.0.0.1:8000/admin（dev 密码 `admin123`）。

窗口 B — 评分 worker（一条命令，环境变量已内置）：

```powershell
pwsh scripts\windows\start-worker-dev.ps1
```

macOS/Linux 对应 `bash scripts/start-worker-dev.sh`。启动日志出现 `[dev-worker-1]`
即环境变量生效。手动方式等价于：

```powershell
$env:DATABASE_URL = "postgres://exam_app:exam_app_dev@127.0.0.1:5432/exam_system?sslmode=disable"
$env:WORKER_ID = "dev-worker-1"
$env:MULTIPLE_CHOICE_PARTIAL = "true"
$env:RERANK_USE_REMOTE = "false"
.\.venv\Scripts\python.exe -m scoring_worker
```

注意：worker **不读取 `.env`**，环境变量必须在启动前设好。改过 `data/papers/*.json`
后先跑 `python scripts\sync_run_snapshots.py` 再启动 worker，否则评分会因
snapshot hash mismatch 被跳过。

### 5. 常见报错对照

| 报错 | 原因 / 处理 |
|---|---|
| `fe_sendauth: no password supplied` | 没设 `DATABASE_URL`，走了默认 DSN；用 `start-worker-dev.ps1` 或手动 `$env:DATABASE_URL` |
| `No solution found: colorama / tzdata / pytest` | `packages/` 缺 Windows 条件依赖（`colorama`←qrcode、`tzdata`←psycopg）或 dev 组（`pytest`）；`python -m pip download colorama tzdata pytest -d .\packages` 补齐 |
| `unrecognized subcommand 'download'` / `unexpected argument '--find-links'` | uv 版本太老；`uv self update` 后重试 |
| 启动日志显示 `[worker-default]` | 环境变量没设；用启动脚本 |
| 用 Python 3.13 建的 venv | 项目限定 3.12；`rm -rf .venv && uv venv --python 3.12 && uv sync --extra scoring --extra dev --extra embedding` 重建 |
| `pg_isready`/`psql` 报命令不存在 | PostgreSQL 的 bin 目录未加入 PATH |

生产部署（计划任务 + 18080 端口 + 离线 worker）另见 [docs/deployment-go-pg.md](docs/deployment-go-pg.md)。

## Python 环境（评分 worker 与测试）

项目使用 `uv` 管理依赖，虚拟环境位于 `.venv/`；`scoring_worker/` 为独立 uv 工程。

```bash
uv sync --extra scoring --extra dev --extra embedding
```

主观题评分为独立库，`pyproject.toml` 钉选 GitHub tag（当前 `v0.1.13`）确保构建可复现：

```toml
[tool.uv.sources]
subjective-scoring = { git = "https://github.com/yhwyxy/subjective-scoring", tag = "v0.1.13" }
```

本地修改评分库时，可用相邻目录的 editable 安装临时覆盖固定版本：

```bash
uv pip install -e "../subjective-scoring[text,sql,code]"
```

## 主观题语义模型（可选）

默认 CrossEncoder 为 `BAAI/bge-reranker-base`，需安装 `sentence-transformers`
（`uv sync --extra embedding`）。未安装或加载失败时自动回退词法相似度，提交流程不中断。

无法下载本地模型时，可改用 Cohere-compatible 云端 Reranker。注意 worker **不读取
`.env`**，需在启动前设置环境变量（推荐 `bash scripts/start-worker-dev.sh`，或手动
`export` / `$env:`，见上文「4. 手动启动」）：

```bash
export RERANK_USE_REMOTE=true
export RERANK_API_URL=https://router.tumuer.me/v1/rerank
export RERANK_API_KEY=your-api-key
export RERANK_MODEL=Pro/BAAI/bge-reranker-v2-m3
```

`RERANK_USE_REMOTE` 未配置或设为 `false` 时使用本地模型；设为 `true` 时，另外三个云端变量必须
同时设置。API Key 不应写入任何受版本控制的文件；Docker 生产部署的 env 模板见
`.env.production.example`（复制为 `.env` 后填写，`.env` 已被 Git 忽略）。

## 题库 / 多专业试卷

系统按**专业**管理试卷：每个专业一份当前卷。

```text
data/papers/
  mech.json           # 某专业可编辑试卷
  elec.json
data/exam_runs/
  paper-{slug}-run-{id}.json   # 发布时不可变快照 (路径+sha256 存 exam_runs 表)
  tokens/{run_id}.token        # 管理端重建链接用的公开 token（仅文件系统）
```

- 管理后台 → **试卷 / 专业**：新建专业、录入题目、保存整卷；支持批量发布/结束。
- **考试管理**：查看各专业当前/最近轮次、链接与答题人数；「发布」创建新轮次，链接形如 `/exam?paper=slug&run=token`。
- 考生答题过程自动保存服务器草稿；管理员结束考试后进入收卷缓冲，到期按最新草稿自动提交（`admin_closed`）。
- 员工端必须通过带 `paper` 的链接进入；接口自动脱敏（移除 `answer` / `scoring_rubric` / `scoring_points`）。
- 同一工号可考多个专业；同一专业不可重复提交。

## 测试

```bash
go build ./... && go vet ./... && go test ./...
pytest -q          # scoring_worker 单测 + subjective-scoring 契约
```

`tests/contract/` 为前后端契约黑盒套件（待改造为经 Go admin API 注入数据的纯 Go 模式，
当前整体 skip）。

## Docker 部署

开发/单机快速起 Go 服务:

```bash
docker compose up -d          # 构建 Go 镜像并启动
docker compose logs -f
docker compose down
```

挂载说明：
- `config.yaml`（只读）：宿主修改后需 `docker compose restart` 生效
- `data/`（可写）：试卷 JSON 与 run 快照
- 本地开发库: `docker compose -f docker-compose.postgres.yml up -d` (映射宿主 5433)

**生产容器化部署**（Linux 服务器, 三容器: postgres + exam + worker, 独立伸缩）:

```bash
cp .env.production.example .env
cp config.production.example.yaml config.production.yaml
docker compose -f docker-compose.prod.yml build
docker compose -f docker-compose.prod.yml up -d postgres
docker compose -f docker-compose.prod.yml run --rm exam migrate --config /app/config.yaml
docker compose -f docker-compose.prod.yml up -d
```

完整步骤、反代 TLS、备份升级与扩容见
[docs/deployment-docker.md](docs/deployment-docker.md)。
（Windows 计划任务方案见 [docs/deployment-go-pg.md](docs/deployment-go-pg.md)。）

## 安全配置

管理后台认证默认关闭。生产环境建议启用管理员认证：

```yaml
admin:
  enable_auth: true
  password: "your_secure_password"
```

启用后，前端管理页会先显示登录面板；后端所有 `/api/admin/*` 管理接口（包括导出、复核、
重载配置/题库）都需要 `Authorization: Bearer <token>`。管理员 Token 为进程内存储，
进程重启需重新登录。

CORS 默认只允许同源本地地址。部署到指定域名时，请在 `config.yaml` 中显式配置允许的源；
不要在生产环境使用 `allow_origins: ["*"]`。
