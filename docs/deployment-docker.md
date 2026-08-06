# 生产部署: Docker 容器化 (Linux)

适用: 把 Go exam-server + PostgreSQL + scoring_worker 以三容器形式跑在一台 Linux
服务器 (或一组机器) 上。与 Windows 计划任务方案 ([deployment-go-pg.md](deployment-go-pg.md))
二选一。

## 0. 架构

| 服务 | 镜像 | 职责 | 伸缩 |
|---|---|---|---|
| postgres | postgres:18-alpine | 数据持久化, 仅内网可达 | 单实例/托管库 |
| exam | examsystem:latest (仓库根 Dockerfile) | Go API + 静态前端 | 考试并发 |
| worker | examsystem-worker:latest (Dockerfile.worker) | 主观题评分轮询 (三模式可热切换) | 评分积压 (并行=实例数) |

关键约定:
- 密钥只经 `.env` 注入 (PG_PASSWORD / PG_APP_PASSWORD / PG_MIGRATOR_PASSWORD /
  EXAM_DATABASE_URL / EXAM_MIGRATOR_DATABASE_URL / EXAM_ADMIN_PASSWORD), 不进镜像。
- 最小权限角色 (initdb 由 `scripts/pg-bootstrap-prod.sh` 自动创建): `exam_app`
  运行态 (仅 DML, 应用+worker 共用) / `exam_migrator` 一次性迁移 (DDL+DML)。
  `migrate` 子命令自动读 `EXAM_MIGRATOR_DATABASE_URL` 切到迁移角色 (serve 不受影响)。
- 三容器均以非 root (uid 10001) 运行; exam 挂载的 `data/` 与 `logs/` 需
  `sudo chown -R 10001:10001 data logs` 才能写入。
- PG18 镜像的卷要挂整个 `/var/lib/postgresql` (镜像内 PGDATA=
  `/var/lib/postgresql/18/docker`); 挂 `/var/lib/postgresql/data` 会让真实数据
  落匿名卷, 换机/升级时丢失。
- exam 与 worker 共享同一 `./data` 卷: DB 里 snapshot_path 是相对
  `data/exam_runs/...` 存的, worker 必须能按相同相对路径读到快照文件。
- worker 单进程单线程轮询 (`concurrency` 配置未实际生效), 并行度 = 实例数。
- 评分方式三选一 (local / remote_reranker / llm): 管理后台「设置」栏保存后写入 DB
  (`app_settings['scoring']`), worker 每轮询热加载, 无需重启; 环境变量只在未保存
  任何设置时作为启动默认 (详见第 2 节)。
- `EXAM_ADMIN_PASSWORD` 是**初始密码**: 登录后可在「设置」栏修改 (bcrypt 存 DB,
  优先级高于 env/config 明文; 改密后旧 token 全部失效)。
- 迁移用 exam 镜像跑一次性任务, 不常驻。

## 1. 前置条件

- Linux 服务器, Docker Engine 24+ 且带 Compose v2 (`docker compose version`)。
- 磁盘: 镜像约 1GB; 本地模型另需约 2GB (放 `./models`)。
- 内存建议: PG 512MB+ / exam 512MB / worker 4GB (本地模型加载)。
- 构建需联网 (Docker Hub / GitHub / PyPI); 构建机与部署机一致用 Linux (镜像内是
  Linux wheel, 与仓库 `packages/` 里的 Windows wheel 无关)。

## 2. 一次性准备

```bash
git clone <repo-url> && cd examSystem

# 1) 生产配置 (database.url 留空, 由 EXAM_DATABASE_URL 注入)
cp config.production.example.yaml config.production.yaml
#    必改: admin.enable_auth: true; allow_origins 换成真实域名白名单
#    (模板默认已非 "*", 见文件内注释)

# 2) 环境变量 (强密码)
cp .env.production.example .env
#    必填: PG_PASSWORD / PG_APP_PASSWORD / PG_MIGRATOR_PASSWORD /
#          EXAM_DATABASE_URL / EXAM_MIGRATOR_DATABASE_URL / EXAM_ADMIN_PASSWORD

# 3) 数据/日志目录属主 (三容器均以 uid 10001 运行; exam 要写 data/ 与 logs/)
mkdir -p data logs && sudo chown -R 10001:10001 data logs

# 4) 模型目录 (本地模型模式才需要): 解包 bge-reranker-v2-m3 (需含 config.json)
mkdir -p models
#    把模型放到 models/bge-reranker-v2-m3/
```

`.env` 里的评分方式 (三选一; 生产上推荐在管理后台「设置」栏切换, 保存后以 DB 为准,
env 仅作未保存前的启动默认):
- `local` (默认): 本地语义模型, `RERANKER_MODEL=/models/bge-reranker-v2-m3`; 构建时带
  `WORKER_EXTRAS` (torch 等, 镜像约 2GB)。
- `remote_reranker`: 远程 reranker API (Cohere 兼容), 填 `RERANK_API_URL/KEY/MODEL`;
  `WORKER_EXTRAS` 留空, 镜像保持轻量。
- `llm`: 大模型判分 API (OpenAI 兼容 chat/completions), 填 `LLM_API_URL/KEY/MODEL`;
  `WORKER_EXTRAS` 留空。env 里可显式写 `SCORING_METHOD=llm` 或 `remote_reranker`
  (优先于 `RERANK_USE_REMOTE=true`)。

管理后台「设置」栏切换时只需填对应 url/api_key/model, 保存即生效 (worker 热加载,
无需重建镜像或重启); 只有 local 模式要求镜像里预装 torch (`WORKER_EXTRAS`),
所以镜像构建时就要决定是否带本地模型依赖。

## 3. 构建镜像

```bash
docker compose -f docker-compose.prod.yml build
```

- 产物: `examsystem:latest` (Go, 多阶段) + `examsystem-worker:latest` (Python 3.12)。
- worker 基础依赖与 `scoring_worker/pyproject.toml` 对齐, `subjective-scoring` 从
  GitHub tag `v0.1.13` 拉取; 升级该库时同步改 `Dockerfile.worker`。
- 改 `.env` 里的 `WORKER_EXTRAS` 后需重新 `build worker` 才生效。
- 后台切到 remote_reranker/llm 不需要重建; 切回 local 前确认镜像带 `WORKER_EXTRAS`
  (否则 worker 会记录切换失败并沿用旧服务)。

## 4. 启动数据库并迁移

```bash
docker compose -f docker-compose.prod.yml up -d postgres
docker compose -f docker-compose.prod.yml ps          # postgres 应为 healthy

# 一次性迁移 (复用 exam 镜像; 会等 postgres healthy 再跑)
# migrate 子命令自动切到 exam_migrator 角色 (EXAM_MIGRATOR_DATABASE_URL, DDL+DML):
docker compose -f docker-compose.prod.yml run --rm exam migrate --config /app/config.yaml
```

> 注意: `migrate` 只应用 DDL (**0001 建表 + 0002 app_settings**), **不搬任何已有
> 数据**。迁移是增量的: 已应用版本 (schema_migrations 记录) 直接跳过, 未应用的
> 自动补上。全新部署的 `pg_data` 卷是空库, 首次初始化会自动建
> `exam_app`/`exam_migrator` 角色; 若要从现有部署迁入, 先做第 4.1 节。

## 4.1 从现有 PG 迁入数据 (首次切换时)

```bash
# 1) 旧库 dump (旧库是本地 EDB 5432 时; -n public 只导业务 schema,
#    跳过 task3_*/test_*/loadtest 等测试遗留 schema):
pg_dump -h 127.0.0.1 -p 5432 -U exam_app -d exam_system -n public > exam_system.sql

# 2) 起新库后灌入, 再补跑一次 migrate (增量: 只会补 0002 等未应用版本,
#    已应用且 checksum 一致的自动跳过, 不会因表已存在而报错):
docker compose -f docker-compose.prod.yml up -d postgres
docker compose -f docker-compose.prod.yml exec -T postgres \
  psql -U "${PG_USER:-exam_super}" "${PG_DB:-exam_system}" < exam_system.sql
docker compose -f docker-compose.prod.yml run --rm exam migrate --config /app/config.yaml
```

> 若旧部署跑的是「PG18 + 挂 `/var/lib/postgresql/data`」的错误卷配置, 真实数据
> 在**匿名卷**里 (命名卷 `pg_data` 是空的): 务必先在本机旧容器内
> `docker compose exec -T postgres pg_dump ...` 导出, 再停旧栈换新配置, 否则旧卷
> 数据无处找回。

业务文件数据 (`data/papers/*.json` 题库 + `data/exam_runs/` 快照/token) 走的是
`./data` bind mount, 与旧部署共用同一目录即保留; 换机部署时整个 `data/` 目录一起拷贝。

> dump 文件含真实作答/员工信息, 仓库 `.gitignore` 已禁止提交 `exam_system.sql`,
> 仅作本地备份。

## 5. 启动全栈

```bash
docker compose -f docker-compose.prod.yml up -d
docker compose -f docker-compose.prod.yml ps
```

## 6. 验证

```bash
curl http://127.0.0.1:8000/api/health      # 期望 ok + version + time

docker compose -f docker-compose.prod.yml ps   # postgres/exam/worker 应均 healthy
# (worker 的 healthcheck 是轻量 DB 探活, 见服务定义; 本地模型首启加载慢, start_period 60s)

# worker 链路自检 (真实评分一次, 不写 DB; 本地模型模式下会加载模型, 稍慢)
docker compose -f docker-compose.prod.yml run --rm --no-deps worker \
  python -m scoring_worker --preflight

docker compose -f docker-compose.prod.yml logs -f exam worker
```

管理后台 `http://127.0.0.1:8000/admin` (未配反代前可用 ssh 隧道访问)。

> preflight 用 env 配置的评分方式跑固定 fixture: local 模式加载本地模型, 稍慢;
> remote_reranker / llm 模式会真实调用对应 API (需容器能访问外网)。

## 7. 反向代理 + TLS (推荐 Caddy)

```bash
docker compose -f docker-compose.prod.yml exec exam ...   # 无; 反代在宿主跑
```

`/etc/caddy/Caddyfile`:

```caddyfile
exam.example.com {
    reverse_proxy 127.0.0.1:8000
}
```

同时:
- `config.production.yaml` 的 `allow_origins` 加上 `https://exam.example.com`。
- 防火墙只放行 80/443; `EXAM_BIND_ADDR` 保持 `127.0.0.1`。

## 8. worker 扩容

worker 单实例单线程, 评分积压时加实例:

```bash
docker compose -f docker-compose.prod.yml up -d --scale worker=3
```

- 同项目内 `--scale` 的实例共享 `WORKER_ID`, 不影响正确性 (租约按 job 归属,
  一个 job 只会被一个实例 claim), 仅日志区分度低。
- 需要区分实例/跨机器扩展: 每机器用自己的 `.env` (不同 `WORKER_ID`) + 同一个外部
  PostgreSQL (把 `docker-compose.prod.yml` 里的 postgres 服务替换为外部连接串)。

## 9. 备份与恢复

推荐用仓库自带脚本 (PG dump + `data/` + 生产配置 + SHA-256 清单, 保留最近 14 份):

```bash
./scripts/backup-prod.sh                # 默认输出到 backups/YYYYMMDD-HHMMSS/
# 计划任务 (cron 每日 02:00):
#   0 2 * * * cd /path/to/examSystem && ./scripts/backup-prod.sh >> backups/backup.log 2>&1
```

`.env` 里的密钥不随备份走, 单独用密码管理器/离线介质保存 (`.env` 是部署机唯一凭据源)。

**恢复流程** (先演练一次, 见下):

```bash
# 1) 停服务
docker compose -f docker-compose.prod.yml down

# 2) 还原 PG: 清掉旧卷数据再起空库, 灌入 dump, 补迁移 (增量跳过已应用)
docker compose -f docker-compose.prod.yml up -d postgres
gzip -dc backups/<时间戳>/exam_system.sql.gz | \
  docker compose -f docker-compose.prod.yml exec -T postgres \
  psql -U "${PG_USER:-exam_super}" "${PG_DB:-exam_system}"

# 3) 还原业务数据 (data/papers + data/exam_runs + token) 与配置
rm -rf data && tar xzf backups/<时间戳>/data.tar.gz
cp backups/<时间戳>/config.production.yaml config.production.yaml

# 4) 目录属主 + 起全栈
sudo chown -R 10001:10001 data logs
docker compose -f docker-compose.prod.yml up -d

# 5) 校验: /api/health + docker compose ps 全 healthy + 抽查一条 run
```

**恢复演练** (每季度一次): 在**另一台测试机**上执行上述步骤, 验证 dump 可灌、
服务可起、能查出历史 run; 用 `sha256sum -c SHA256SUMS` 校验备份文件完整性。
演练同时验证 `scripts/backup-prod.sh` 的产物齐全 (dump / data / config / 清单)。

## 10. 升级流程

```bash
git pull                                   # 或部署新 tag
docker compose -f docker-compose.prod.yml build
docker compose -f docker-compose.prod.yml run --rm exam migrate --config /app/config.yaml
docker compose -f docker-compose.prod.yml up -d
```

升级前先备份 (见第 9 节); 迁移失败时用旧镜像回滚 (`docker compose up -d --no-build`)。

## 11. 题库维护 (改 data/papers/*.json 后)

```bash
# 1) 静态检查 (L1-L6, 零 error)
python3 scripts/lint_papers.py

# 2) 快照同步 (否则 worker 因 snapshot_hash mismatch 跳过评分)
#    需要宿主机有 psql + python3, 且 PG 端口可达:
#    临时取消 docker-compose.prod.yml 里 postgres 的 ports 注释后:
docker compose -f docker-compose.prod.yml up -d postgres
DATABASE_URL='postgres://exam_app:<密码>@127.0.0.1:5433/exam_system?sslmode=disable' \
  python3 scripts/sync_run_snapshots.py

# 3) 重新评分 (可选): /api/admin/regrade/{id}
```

## 12. 运维速查

| 操作 | 命令 |
|---|---|
| 看状态 | `docker compose -f docker-compose.prod.yml ps` |
| 看日志 | `docker compose -f docker-compose.prod.yml logs -f -n 200 exam worker` |
| 重启 exam | `docker compose -f docker-compose.prod.yml restart exam` |
| 停/起全部 | `docker compose -f docker-compose.prod.yml down` / `up -d` |
| 进 PG | `docker compose -f docker-compose.prod.yml exec postgres psql -U exam_app exam_system` |
| worker 自检 | 见第 6 节 `--preflight` |

## 13. 常见问题

- `EXAM_DATABASE_URL` 未设置: compose 直接报 `必填` 错误, 检查 `.env`。
- worker 报连不上库: worker 读 `DATABASE_URL` (不是 `EXAM_DATABASE_URL`),
  两者在 compose 里已绑定为同一值。
- 设置栏切评分方式后 worker 报「切换失败, 沿用旧服务」: 检查填写的
  url/api_key/model 是否完整; 切到 local 但镜像没装 torch 时也会失败,
  需带 `WORKER_EXTRAS` 重建 worker。
- `app_settings` 表不存在或没有评分设置行: worker 回退 env 默认 (local 或
  `RERANK_USE_REMOTE=true`), 不报错, 与旧部署兼容。
- `snapshot hash mismatch`: 改过试卷没跑同步 (见第 11 节)。
- worker 启动报模型加载失败: `RERANKER_MODEL` 指向的目录缺 `config.json`
  (要指向 snapshots/<hash>/ 或模型根), 或 `models/` 挂载路径不存在。
- 本地模式镜像没装 torch: `WORKER_EXTRAS` 没生效, 重新 `build worker`。
- `migrate` 命令报 `exec: "migrate": executable file not found`: 新镜像已加
  `ENTRYPOINT ["exam-server"]`, 直接用 `run --rm ... exam migrate ...`; 若在
  旧镜像/裸 `docker run` 上执行需写成 `exam-server migrate`。
- exam 容器写不了 `data/` / `logs/`: 容器以 uid 10001 运行, 宿主目录需
  `sudo chown -R 10001:10001 data logs` (见 §2 步骤 3)。
- PG 数据"看起来丢了"/卷是空的: 旧配置挂的是 `/var/lib/postgresql/data` (PG18
  真实数据在 `/var/lib/postgresql/18/docker`, 落匿名卷), 见 §4.1 提示。
- 迁移报 `permission denied for table ...`: 迁移要用 `exam_migrator` 连接串
  (`EXAM_MIGRATOR_DATABASE_URL`, migrate 子命令自动使用, 见 §4);
  运行态 `exam_app` 只 DML, 不要用它建表。
