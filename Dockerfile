FROM golang:1.25-alpine AS build

WORKDIR /src

# 1. 依赖层（独立利用 Docker 缓存）
COPY go.mod go.sum ./
RUN go mod download

# 2. 编译层
COPY . .
RUN CGO_ENABLED=0 go build -o /out/exam-server ./cmd/exam-server

FROM alpine:3.20

WORKDIR /app

# 运行时目录属主自愈: 入口脚本 (docker-entrypoint.sh) 以 root 启动, chown 挂载
# 目录后经 su-exec 降权到 10001 再运行 exam-server, 无需部署时手动 chown
# (见 docs/deployment-docker.md §0/§2 步骤 3)
RUN apk add --no-cache ca-certificates tzdata wget su-exec \
 && addgroup -g 10001 -S exam \
 && adduser -S -D -H -u 10001 -G exam exam

COPY --from=build /out/exam-server /usr/local/bin/exam-server
COPY frontend ./frontend
COPY config.yaml ./config.yaml
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh

# 运行时数据目录（试卷 JSON、run 快照等）
# 容器内 /app/data 与 /app/config.yaml 建议通过 volume 持久化
RUN mkdir -p /app/data /app/logs && chown -R 10001:10001 /app

EXPOSE 8000

# 健康检查：调用 /api/health，30s 启动宽限，10s 间隔
HEALTHCHECK --interval=10s --timeout=3s --start-period=30s --retries=3 \
  CMD wget -q -T 2 -O /dev/null http://127.0.0.1:8000/api/health || exit 1

# 主观题评分依赖独立部署的 scoring_worker（Python，见 scoring_worker/），
# 通过共享 PostgreSQL 的 grading_jobs 队列衔接，不在本镜像内。
# 入口固定 docker-entrypoint.sh (root 启动 → chown 挂载目录 → 降权到 10001),
# 子命令由命令行决定:
#   docker run examsystem:latest                       = serve
#   docker run examsystem:latest migrate --config ...  = migrate (一次性)
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["serve", "--config", "config.yaml", "--bind", "0.0.0.0:8000", "--static", "frontend"]

# USER 10001 已移除: 降权在入口脚本内用 su-exec 完成, 镜像默认用户保持 root
# 以便 chown 挂载目录 (运行态仍是 10001)。
