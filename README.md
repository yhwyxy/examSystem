# 企业在线考试兼批改系统

基于 FastAPI + SQLite 的企业内部在线考试 MVP，支持员工扫码答题、客观题自动判分、主观题多引擎评分（独立库 [subjective-scoring](https://github.com/yhwyxy/subjective-scoring)：文本评分点 / SQL AST / 代码混合）、管理员人工复核与 Excel 导出。

## 环境安装

项目使用 `uv` 管理依赖，虚拟环境位于 `.venv/`。

```bash
# 安装核心依赖 + 全部可选组（scoring / dev / embedding）
uv sync --extra scoring --extra dev --extra embedding

# 仅安装核心依赖 + 单个可选组
uv sync --extra scoring
uv sync --extra dev

# 安装完成后启动
python main.py
```

可选依赖组说明：

| 组名 | 用途 | 关键包 |
|------|------|--------|
| `scoring` | 主观题多引擎评分（独立库 `subjective-scoring`） | ftfy, sqlglot, tree-sitter* |
| `dev` | 开发与测试 | pytest |
| `embedding` / `semantic` | 主观题 CrossEncoder 语义（sentence-transformers） | sentence-transformers, torch |

主观题多引擎评分已拆为独立库：

- GitHub（public）：https://github.com/yhwyxy/subjective-scoring
- 当前钉选版本：`v0.1.5`
- 正常安装：`pyproject.toml` 固定使用 GitHub tag，确保构建可复现

```toml
[tool.uv.sources]
subjective-scoring = { git = "https://github.com/yhwyxy/subjective-scoring", tag = "v0.1.5" }
```

```bash
pip install "subjective-scoring[text,sql,code,remote] @ git+https://github.com/yhwyxy/subjective-scoring.git@v0.1.5"
```

```python
from subjective_scoring import SubjectiveScoringService
```

本地修改评分库时，可用相邻目录的 editable 安装临时覆盖固定版本：

```bash
uv pip install -e "../subjective-scoring[text,sql,code]"
```

此后修改 `../subjective-scoring` 会立即生效。执行 `uv sync` 会恢复 GitHub 固定版本；发布新的评分库 tag 后，在本项目中更新 tag 并重新执行 `uv lock`。

更换 CrossEncoder：

```python
SubjectiveScoringService(
    allow_model_load=True,
    text_model="BAAI/bge-reranker-base",
    code_model="BAAI/bge-reranker-base",
)
```

访问：

- 员工考试端：`http://localhost:8000/exam?paper=专业编码&run=轮次token`（由管理端发布后生成）
- 管理后台：http://localhost:8000/admin（试卷录入 / 考试管理）
- API 文档：http://localhost:8000/docs

## 安全配置

管理后台认证默认关闭。生产环境建议启用管理员认证，并配置强密码或 SHA-256 哈希密码：

```yaml
admin:
  enable_auth: true
  password: "your_secure_password"
```

启用后，前端管理页会先显示登录面板；后端所有 `/api/admin/*` 管理接口（包括导出、复核、重载配置/题库）都需要 `Authorization: Bearer <token>`。

CORS 默认只允许同源本地地址。部署到指定域名时，请在 `config.yaml` 中显式配置允许的源：

```yaml
server:
  allow_origins:
    - "http://127.0.0.1:8000"
    - "http://localhost:8000"
    - "https://your-domain.example.com"
```

不要在生产环境使用 `allow_origins: ["*"]`。

## 主观题语义模型（可选）

主观题由独立库 `subjective-scoring` 评分。默认 CrossEncoder 为 `BAAI/bge-reranker-base`，需安装 `sentence-transformers`（`uv sync --extra embedding` 或 `pip install "subjective-scoring[semantic]"`）。

未安装或加载失败时自动回退词法相似度，提交流程不中断。

无法下载本地模型时，可在项目根目录 `.env` 中启用 Cohere-compatible 云端 Reranker：

```dotenv
RERANK_USE_REMOTE=true
RERANK_API_URL=https://router.tumuer.me/v1/rerank
RERANK_API_KEY=your-api-key
RERANK_MODEL=Pro/BAAI/bge-reranker-v2-m3
```

`.env` 会在本地启动时自动加载，已有 shell / Docker 环境变量不会被覆盖。`RERANK_USE_REMOTE` 未配置或设为 `false` 时使用本地模型；设为 `true` 时，另外三个云端变量必须同时设置。启用后文本题和代码题语义评分使用云端 API，并自动关闭本地 CrossEncoder 加载；SQL 结构评分不受影响。`.env` 已被 Git 忽略，API Key 不应写入其他受版本控制的文件。

更换模型：

```python
from backend.grader import set_subjective_service
from subjective_scoring import SubjectiveScoringService

set_subjective_service(SubjectiveScoringService(
    allow_model_load=True,
    text_model="BAAI/bge-reranker-base",
))
```


## 题库 / 多专业试卷

系统按**专业**管理试卷：每个专业一份当前卷。

```text
data/papers/
  index.json          # 专业索引（兼容字段；运行态以 SQLite 轮次为准）
  mech.json           # 某专业可编辑试卷
  elec.json
data/exam_runs/
  {run_id}.json       # 发布时不可变快照
  {run_id}.token      # 管理端重建链接用的公开 token（仅文件系统）
```

- 管理后台 → **试卷 / 专业**：新建专业、录入题目、保存整卷；支持批量发布/结束。
- **考试管理**：查看各专业当前/最近轮次、链接与答题人数；「发布」创建新轮次，链接形如 `/exam?paper=slug&run=token`。
- **非考试阶段**可自由改题；**考试中 / 收卷中**禁止改题；存在历史轮次时禁止硬删除试卷。
- 考生答题过程每约 2 秒自动保存服务器草稿；管理员结束考试后进入 5 秒收卷缓冲，到期按最新草稿自动提交（`admin_closed`）。
- 部署升级前请备份 `data/exam.db`，并确保没有进行中的考试。
- 员工端必须通过带 `paper` 的链接进入；接口自动脱敏（移除 `answer` / `scoring_rubric` / `scoring_points`）。
- 首次启动若仅有旧文件 `data/questions.json`，会迁移为 `papers/default.json`。
- 同一工号可考多个专业；同一专业不可重复提交。


## 测试

```bash
pytest -q
python -m compileall backend main.py
```

## Docker 部署

```bash
# 构建并启动（首次会自动构建镜像）
docker compose up -d

# 查看日志 / 健康状态
docker compose logs -f
docker compose ps

# 停止 / 重建
docker compose down
docker compose up -d --build
```

挂载说明：
- `config.yaml`（只读）：宿主修改后需 `docker compose restart` 生效
- `data/papers/`（可写）：多专业试卷；`data/exam.db`：成绩库
- `exam-db` volume：SQLite `exam.db` 持久化，容器重建不丢数据

## 安全注意事项

- `admin.enable_auth: true` 时必须设置 `password`，否则管理员无法登录（启动时日志会告警）
- CORS 默认 `allow_credentials=false`（Token 走 Authorization header，无需 cookie），避免 CSRF 凭证泄漏
- 速率限制按 IP 维度，管理端独立配额（120/min）、考生端 60/min
- 管理员 Token 为进程内存储，进程重启需重新登录
