FROM python:3.12-slim

WORKDIR /app

# 1. 依赖层（独立利用 Docker 缓存）
COPY requirements.txt ./
RUN pip install --no-cache-dir -r requirements.txt

# 2. 应用代码层
COPY . .

# 3. 运行时数据目录（SQLite db 文件、题库等）
# 容器内 /app/data 与 /app/config.yaml 建议通过 volume 持久化
RUN mkdir -p /app/data

EXPOSE 8000

# 生产环境关闭 reload，可通过环境变量覆盖配置文件路径
ENV CONFIG_PATH=config.yaml

# 健康检查：调用 /api/health，30s 启动宽限，10s 间隔
HEALTHCHECK --interval=10s --timeout=3s --start-period=30s --retries=3 \
  CMD python -c "import urllib.request,sys; urllib.request.urlopen('http://127.0.0.1:8000/api/health', timeout=2); sys.exit(0)" || exit 1

CMD ["uvicorn", "backend.main:app", "--host", "0.0.0.0", "--port", "8000"]
