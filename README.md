# 企业在线考试兼批改系统

基于 FastAPI + SQLite 的企业内部在线考试 MVP，支持员工扫码答题、客观题自动判分、主观题 LLM 判分、Embedding/关键词回退、管理员人工复核与 Excel 导出。

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

## Ollama 判分

默认配置使用：

```yaml
grading:
  llm:
    endpoint: "http://localhost:11434"
    model: "qwen2.5:7b"
```

如本地未启动 Ollama 或模型不可用，系统会自动回退到 Embedding/关键词相似度判分，保证提交流程不中断。

## 题库

题库文件位于 `data/questions.json`。员工端接口会自动移除 `answer` 与 `scoring_rubric`，避免答案泄露。

## 测试

```bash
pytest -q
python -m compileall backend main.py
```
