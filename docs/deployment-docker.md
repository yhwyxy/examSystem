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
- 三容器运行态均为非 root (uid 10001)。exam 的入口脚本 `docker-entrypoint.sh`
  以 root 启动, 对挂载目录 `data/` `logs/` chown 到 10001 后经 su-exec 降权再运行
  exam-server (postgres 镜像同款模式), 部署时**无需**手动 chown (手动 chown 仅
  旧镜像需要, 见 §2 步骤 3 与 §13)。
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
- 本地语义模型是**部署输入物**, 运行时**不自动下载**: worker 容器以
  `HF_HUB_OFFLINE=1` + `HF_HOME=/models/hf-cache` + `models/` 只读挂载三层禁网。
  模型必须由 fetch-models.sh 或手动放入宿主 `models/` (扁平布局, 见 §2 步骤 4);
  路径对不上时评分**静默回退词法**不报错 (见 §13), 部署后务必跑 §6 的
  `--preflight` 自检。

## 1. 前置条件

- Linux 服务器, Docker Engine 24+ 且带 Compose v2 (`docker compose version`)。
- 磁盘: 镜像约 1GB; 本地模型另需约 2GB (放 `./models`)。
- 内存建议: PG 512MB+ / exam 512MB / worker 4GB (本地模型加载)。
- 构建需联网 (Docker Hub / PyPI 清华源; 模型下载走 hf-mirror); `subjective-scoring`
  用仓库内置 wheel, 默认**不访问 GitHub**。构建机与部署机一致用 Linux (镜像内是
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

# 3) 数据/日志目录 (exam 入口脚本会自动 chown 属主并降权, 见 §0, 无需手动)
mkdir -p data logs
#    仅旧镜像 (无 docker-entrypoint.sh) 需要手动属主:
#    sudo chown -R 10001:10001 data logs

# 4) 模型目录 (本地模型模式才需要): 下载 bge-reranker-v2-m3 (需含 config.json)
#    一键同步 (走 hf-mirror.com 国内镜像 + 一次性容器, 不要求宿主机装 Python):
mkdir -p models
./scripts/fetch-models.sh        # -> ./models/bge-reranker-v2-m3/
#    也可手动解包: 把 BAAI/bge-reranker-v2-m3 放到 models/bge-reranker-v2-m3/
#    布局要求: RERANKER_MODEL=/models/bge-reranker-v2-m3 期望**扁平布局** —— 该目录
#    顶部就是 config.json。若目录里只有 hub/models--BAAI--*/snapshots/<hash>/ (默认
#    huggingface-cli download 的 cache 布局), 路径对不上 → 评分静默回退词法
#    (见 §13 常见问题)。模型已在宿主时**无需重下**, 软链修复:
#       SNAP=$(cat models/models--BAAI--bge-reranker-v2-m3/refs/main 2>/dev/null \
#              || ls models/models--BAAI--bge-reranker-v2-m3/snapshots/ | head -1)
#       ln -sfn "models--BAAI--bge-reranker-v2-m3/snapshots/$SNAP" models/bge-reranker-v2-m3
#       docker compose -f docker-compose.prod.yml restart worker
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
- worker 基础依赖与 `scoring_worker/pyproject.toml` 对齐; `subjective-scoring`
  默认安装仓库内置 wheel（当前工作区为 `packages/subjective_scoring-0.1.14-py3-none-any.whl` 发布候选，尚未提交/发布），构建**不需要访问 GitHub**。开发 Git 依赖仍固定 v0.1.13，待评分库 PR 与真实 tag 完成后同步，详见 [候选发布记录](deployment-scoring-release.md)。
- worker 构建默认走国内镜像: pip 用清华源 (`PIP_INDEX_URL`, 可通过
  `--build-arg PIP_INDEX_URL=https://pypi.org/simple` 换回官方源)。
- 需要从 GitHub tag 拉 `subjective-scoring` 时 (构建机能访问 GitHub):
  `--build-arg SUBJECTIVE_SCORING_SOURCE=git` (直连慢可用
  `--build-arg SUBJECTIVE_SCORING_URL=<git 镜像地址>` 指到镜像)。
- 使用 git 来源前须确认目标 tag 已实际发布；当前 v0.1.14 候选尚不满足此条件。
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
./scripts/backup-prod.sh                # 输出到 backups/YYYYMMDD-HHMMSS-<唯一后缀>/
# 计划任务 (cron 每日 02:00):
#   0 2 * * * cd /path/to/examSystem && ./scripts/backup-prod.sh >> backups/backup.log 2>&1
```

`.env` 里的密钥不随备份走, 单独用密码管理器/离线介质保存 (`.env` 是部署机唯一凭据源)。

备份前暂停答卷写入、人工复核、发布、worker 和检测任务，确保数据库与文件来自同一稳定时点；脚本不负责暂停业务。成功备份包含 `BACKUP_COMPLETE`（`examSystem-backup-v1`）和三个文件的 `SHA256SUMS`，全部成功后才原子移入最终目录。失败备份不参与保留清理；旧式未标记目录也不会自动删除。默认保留 14 份，可用 `BACKUP_KEEP` 调整。若 `.backup.lock` 遗留，先检查其中 pid 对应的实际进程与启动时间，确认没有备份仍运行后才移除锁；不能仅根据 pid 文件判断锁已过期。

**恢复流程**：在另一台隔离主机的独立部署目录与全新 Compose project/数据库卷中恢复，保留原部署。`docker compose down` 不会清空数据库卷，不能据此把原卷当作空库。完整步骤见 [备份恢复与本地演练](deployment-restore.md)。恢复前校验完成标记、清单及归档路径；数据库导入使用 `psql -v ON_ERROR_STOP=1`，数据目录必须为空。数据库、`data/` 与配置成组恢复后，先核对历史分数、审计与快照哈希，再应用候选迁移并启动 API；核准队列、凭据和评分配置后才启动 worker。

**恢复演练**：每季度在隔离部署中验证 dump、文件、迁移、历史 API 和已批准的版本回退路径。仓库另提供只使用合成数据的原生 PostgreSQL 演练，它验证代码和备份链路，不代替 Docker 部署或生产恢复验收。

## 10. 升级流程

```bash
git pull                                   # 或部署新 tag
docker compose -f docker-compose.prod.yml build
docker compose -f docker-compose.prod.yml run --rm exam migrate --config /app/config.yaml
docker compose -f docker-compose.prod.yml up -d
```

升级前先完成一致性备份与隔离演练（见第 9 节）。迁移失败时保持维护状态；只有已验证旧程序兼容当前 schema 才能直接切换旧镜像，否则用成组备份恢复到独立环境及匹配版本。不能假设 `--no-build` 会选择旧镜像或撤销迁移。灰度准入与回退决策见 [发布手册](release-rollout-runbook.md)。

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
- 语义模型不可用 (评分照跑但产废分): preflight 退出码 2, 每题 warning
  「语义模型不可用，已回退到词法相似度」。**这是最危险的故障**: worker 的
  healthcheck 只探 DB 不加载模型, 容器显示 healthy, 直到提交试卷才暴露。
  根因按序排查:
  1. **模型目录布局不对 (最常见)**: `RERANKER_MODEL=/models/bge-reranker-v2-m3`
     期望扁平布局 (目录顶部就是 `config.json`); 用默认 `huggingface-cli download`
     拉出的是 cache 布局 (`hub/models--BAAI--bge-reranker-v2-m3/snapshots/<hash>/`),
     `/models/bge-reranker-v2-m3` 不存在 → CrossEncoder 把它当 repo id 离线解析
     → 报 not found。模型已在宿主时**无需重下**, 软链修复 (compose 目录下执行):
     ```bash
     SNAP=$(cat models/models--BAAI--bge-reranker-v2-m3/refs/main 2>/dev/null \
            || ls models/models--BAAI--bge-reranker-v2-m3/snapshots/ | head -1)
     ln -sfn "models--BAAI--bge-reranker-v2-m3/snapshots/$SNAP" models/bge-reranker-v2-m3
     docker compose -f docker-compose.prod.yml restart worker
     ```
     验证: `docker compose -f docker-compose.prod.yml run --rm --no-deps worker \
       python -m scoring_worker --preflight` 退出码应 0 (日志 `非 lexical`)。
  2. **镜像没装 torch/sentence-transformers**: `WORKER_EXTRAS` 未生效, 需带
     `WORKER_EXTRAS` 重建 worker (见 §3)。验:
     `docker compose -f docker-compose.prod.yml exec worker python -c \
       "import sentence_transformers, torch; print(torch.__version__)"`。
  3. **模型文件不完整 / `models/` 挂载源不存在**: 缺 `config.json` 或
     `pytorch_model.bin` (fetch-models.sh 只验 config.json, 会漏判半成品);
     或宿主 `models/` 目录为空 (bind mount 会自动建 root 属主空目录挂上,
     内容不完整照常回退)。验: `exec worker ls -la /models/bge-reranker-v2-m3/`。
  4. 模型路径对不上时 worker **不会自动下载**: 生产部署以 `HF_HUB_OFFLINE=1` +
     `HF_HOME=/models/hf-cache` + `models/` 只读挂载三层禁网, 见 §0 关键约定。
- 本地模式镜像没装 torch: `WORKER_EXTRAS` 没生效, 重新 `build worker`。
- `migrate` 命令报 `exec: "migrate": executable file not found`: 新镜像已加
  `ENTRYPOINT ["exam-server"]`, 直接用 `run --rm ... exam migrate ...`; 若在
  旧镜像/裸 `docker run` 上执行需写成 `exam-server migrate`。
- exam 容器写不了 `data/` / `logs/` (报 `papers.AtomicWriteJSON tmp open:
  .../.json.tmp.tmp: permission denied` 等 EACCES): 宿主目录属主是 root,
  uid 10001 无写权限。新镜像入口 (`docker-entrypoint.sh`, root 启动 → chown →
  su-exec 降权) 会自动修复, **重建镜像后自愈**:
  `docker compose -f docker-compose.prod.yml build exam && up -d --force-recreate exam`。
  旧镜像临时处理: `sudo chown -R 10001:10001 data logs` (见 §2 步骤 3)。
- PG 数据"看起来丢了"/卷是空的: 旧配置挂的是 `/var/lib/postgresql/data` (PG18
  真实数据在 `/var/lib/postgresql/18/docker`, 落匿名卷), 见 §4.1 提示。
- 迁移报 `permission denied for table ...`: 迁移要用 `exam_migrator` 连接串
  (`EXAM_MIGRATOR_DATABASE_URL`, migrate 子命令自动使用, 见 §4);
  运行态 `exam_app` 只 DML, 不要用它建表。
