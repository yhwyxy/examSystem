# 企业在线考试兼批改系统

基于 FastAPI + SQLite 的企业内部在线考试 MVP，支持员工扫码答题、客观题自动判分、主观题 Embedding 语义相似度判分、关键词回退、管理员人工复核与 Excel 导出。

## 启动

```bash
cd /Users/yhw/Code/Github/examSystem
python -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt
python main.py
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

## Ollama Embedding 判分

默认配置使用：

```yaml
grading:
  embedding:
    model: "bge-m3"
    endpoint: "http://localhost:11434"
    timeout_seconds: 10
```

如本地未启动 Ollama 或模型不可用，系统会自动回退到 sentence-transformers 本地模型或关键词相似度判分，保证提交流程不中断。

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

