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

## Python 环境（评分 worker 与测试）

项目使用 `uv` 管理依赖，虚拟环境位于 `.venv/`；`scoring_worker/` 为独立 uv 工程。

```bash
uv sync --extra scoring --extra dev --extra embedding
```

主观题评分为独立库，`pyproject.toml` 钉选 GitHub tag（当前 `v0.1.7`）确保构建可复现：

```toml
[tool.uv.sources]
subjective-scoring = { git = "https://github.com/yhwyxy/subjective-scoring", tag = "v0.1.7" }
```

本地修改评分库时，可用相邻目录的 editable 安装临时覆盖固定版本：

```bash
uv pip install -e "../subjective-scoring[text,sql,code]"
```

## 主观题语义模型（可选）

默认 CrossEncoder 为 `BAAI/bge-reranker-base`，需安装 `sentence-transformers`
（`uv sync --extra embedding`）。未安装或加载失败时自动回退词法相似度，提交流程不中断。

无法下载本地模型时，可在项目根目录 `.env` 中启用 Cohere-compatible 云端 Reranker：

```dotenv
RERANK_USE_REMOTE=true
RERANK_API_URL=https://router.tumuer.me/v1/rerank
RERANK_API_KEY=your-api-key
RERANK_MODEL=Pro/BAAI/bge-reranker-v2-m3
```

`RERANK_USE_REMOTE` 未配置或设为 `false` 时使用本地模型；设为 `true` 时，另外三个云端变量必须
同时设置。`.env` 已被 Git 忽略，API Key 不应写入其他受版本控制的文件。

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

```bash
docker compose up -d          # 构建 Go 镜像并启动
docker compose logs -f
docker compose down
```

挂载说明：
- `config.yaml`（只读）：宿主修改后需 `docker compose restart` 生效
- `data/`（可写）：试卷 JSON 与 run 快照
- PostgreSQL 与 scoring_worker 需另行部署（见 docs/deployment-go-pg.md）

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
