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

RUN apk add --no-cache ca-certificates tzdata wget

COPY --from=build /out/exam-server /usr/local/bin/exam-server
COPY frontend ./frontend
COPY config.yaml ./config.yaml

# 运行时数据目录（试卷 JSON、run 快照等）
# 容器内 /app/data 与 /app/config.yaml 建议通过 volume 持久化
RUN mkdir -p /app/data

EXPOSE 8000

# 健康检查：调用 /api/health，30s 启动宽限，10s 间隔
HEALTHCHECK --interval=10s --timeout=3s --start-period=30s --retries=3 \
  CMD wget -q -T 2 -O /dev/null http://127.0.0.1:8000/api/health || exit 1

# 主观题评分依赖独立部署的 scoring_worker（Python，见 scoring_worker/），
# 通过共享 PostgreSQL 的 grading_jobs 队列衔接，不在本镜像内。
CMD ["exam-server", "serve", "--config", "config.yaml", "--bind", "0.0.0.0:8000", "--static", "frontend"]
