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
- 当前钉选版本：`v0.1.0`
- 本地开发：`pyproject.toml` 中 `[tool.uv.sources]` 使用 path editable（`../subjective-scoring`）
- 无本地克隆时改为 git 依赖：

```toml
[tool.uv.sources]
subjective-scoring = { git = "https://github.com/yhwyxy/subjective-scoring", tag = "v0.1.0" }
```

```bash
pip install "subjective-scoring[text,sql,code] @ git+https://github.com/yhwyxy/subjective-scoring.git@v0.1.0"
```

```python
# 推荐
from subjective_scoring import SubjectiveScoringService

# 兼容旧导入
from backend.scoring import SubjectiveScoringService
```

更换 CrossEncoder：

```python
SubjectiveScoringService(
    allow_model_load=True,
    text_model="BAAI/bge-reranker-base",
    code_model="BAAI/bge-reranker-base",
)
```

访问：

- 员工考试端：http://localhost:8000/
- 管理后台：http://localhost:8000/admin
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

更换模型：

```python
from backend.grader import set_subjective_service
from subjective_scoring import SubjectiveScoringService

set_subjective_service(SubjectiveScoringService(
    allow_model_load=True,
    text_model="BAAI/bge-reranker-base",
))
```


## 题库

题库文件位于 `data/questions.json`。员工端接口会自动移除 `answer` 与 `scoring_rubric`，避免答案泄露。

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
- `data/questions.json`（只读）：题库
- `exam-db` volume：SQLite `exam.db` 持久化，容器重建不丢数据

## 安全注意事项

- `admin.enable_auth: true` 时必须设置 `password`，否则管理员无法登录（启动时日志会告警）
- CORS 默认 `allow_credentials=false`（Token 走 Authorization header，无需 cookie），避免 CSRF 凭证泄漏
- 速率限制按 IP 维度，管理端独立配额（120/min）、考生端 60/min
- 管理员 Token 为进程内存储，进程重启需重新登录

