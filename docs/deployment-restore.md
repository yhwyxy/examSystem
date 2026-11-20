# 备份恢复与本地演练

数据库 dump、`data/` 和配置必须来自同一稳定时点。先进入维护状态，暂停 API 写入、worker 与检测进程，再执行 `scripts/backup-prod.sh`；PostgreSQL 保持运行。`.env` 和凭据另行安全保存，不能把它们加入公开演练记录。

## 在独立部署恢复

以下步骤供隔离测试主机使用，尚未在 Docker 中现场验收。先准备独立部署目录、已固定摘要的镜像、部署脚本及 Compose 文件，确认该目录没有原部署的数据库卷或 `data/`。每次恢复使用新的 project 名；失败后保留日志，从另一个空环境重新开始。

1. 设置 `RESTORE_BACKUP` 为备份目录的绝对路径；检查 `BACKUP_COMPLETE` 恰为 `examSystem-backup-v1`，清单包含且仅包含 dump、数据归档、配置三个文件，再执行 `sha256sum -c SHA256SUMS`（macOS 用 `shasum -a 256 -c SHA256SUMS`）。检查归档只有 `data/` 下的普通文件/目录，无绝对路径、`..`、符号链接或设备文件。仓库 `scripts.rehearse_restore.verify_backup` 提供该校验，导入函数不执行数据库操作。
2. 在恢复目录准备独立 `.env`，指向恢复项目内部的 `postgres` 服务，使用匹配备份角色的独立凭据。设置 `EXAM_BIND_ADDR` 为未占用的 loopback 地址；不要复用生产 URL、反向代理或 sandbox/provider 凭据。复制备份配置并审阅外部路径和服务地址。
3. 在以下命令中用新的 `RESTORE_PROJECT`，先用 `docker compose ... config` 在本地审阅目标卷及挂载；输出含凭据，勿写入共享日志。确认该 project 无既有卷，才创建空数据库。首次启动脚本须建立 dump 引用的角色。

```bash
# 在独立恢复目录执行；变量必须事先设置。
set -euo pipefail
: "${RESTORE_PROJECT:?必须设置新的恢复 project 名}"
: "${RESTORE_BACKUP:?必须设置已校验备份的绝对路径}"
test ! -e data
tar xzf "$RESTORE_BACKUP/data.tar.gz"
cp "$RESTORE_BACKUP/config.production.yaml" config.production.yaml
mkdir -p logs
# 部署镜像以 uid 10001 运行；按部署要求设置 data/logs 的属主。
docker compose -p "$RESTORE_PROJECT" -f docker-compose.prod.yml up -d --wait postgres
gzip -dc "$RESTORE_BACKUP/exam_system.sql.gz" | \
  docker compose -p "$RESTORE_PROJECT" -f docker-compose.prod.yml exec -T postgres \
  sh -c 'exec psql -X --no-password -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" "$POSTGRES_DB"'
```

4. 数据库导入必须无错误。与备份记录核对提交数、历史分数、复核审计、迁移版本，并逐一校验数据库 `snapshot_path` 所指文件及 `snapshot_hash`。不要修改备份或历史快照来消除不一致。
5. 使用已固定候选镜像执行 `docker compose -p "$RESTORE_PROJECT" -f docker-compose.prod.yml run --rm --no-deps exam migrate --config /app/config.yaml`，再 `up -d --no-deps exam`。检查健康接口、历史成绩读取及历史标识、未完成新流程成绩的发布拒绝。核准队列、权限、provider、sandbox 与 preflight 后才启动 worker；读取历史记录时保持任务处理暂停。
6. 若要回退程序，优先在独立恢复环境使用与备份 schema 匹配的已批准旧镜像及前端。直接在升级后的 schema 运行旧程序需另行兼容验证。切换服务前核对备份之后的新增答卷、复核和发布，制定保留或重放方案，再由发布负责人执行维护窗口切换；此手册不授权覆盖原部署。

## 合成数据的原生 PostgreSQL 演练

需要 Python 3.12、项目 Python 依赖（`httpx`、`psycopg`）、Go、Git、`pg_dump`/`psql` 和 SHA-256 工具。准备独立本地 PostgreSQL 实例：管理员数据库必须叫 `exam_goal_test` 或 `exam_rehearsal_admin`，URL 使用 loopback 与显式端口；连接用户需创建/删除数据库权限，实例预先有迁移要求的 `exam_app` 角色。凭据通过环境注入。

```bash
# 先设置 REHEARSAL_ADMIN_DATABASE_URL；不要指向业务数据库。
python scripts/rehearse_restore.py --baseline-ref 8783a88a3add974f554dd79a62f887ad533d45e2 --output-root /private/tmp/exam-restore-rehearsals
```

脚本编译当前候选程序和 显式指定的基线提交程序（此夹具要求基线恰有前两份 migration），使用基线的前两份 migration 建立两个历史成绩及复核审计，然后执行真实 dump、候选增量迁移、历史 API 检查、空库恢复、文件/快照哈希校验、旧程序读取旧 schema、候选再次升级及迁移幂等检查。临时数据库仅以唯一 `exam_rehearsal_` 前缀创建，在结束时清理。输出目录权限受限，包含版本摘要、API 响应、迁移日志和 `report.json`；命令退出 0 且报告 `passed: true` 才表示通过。异常时检查保护目录中的日志，确认原进程已结束再重试。

2026-10-13 本地演练通过：schema 2 → 11，两个历史成绩及两条审计保持一致；基线 `8783a88a3add974f554dd79a62f887ad533d45e2` 能读取与其匹配的恢复旧库。该基线不是已批准的生产回退版本。此证据不包含 Docker、真实模型、生产数据恢复或灰度上线验收。

2026-10-14 增加 `0012_review_audit` 后再次通过 schema 2 → 12 的成组备份、恢复、基线读取和再次升级。旧调分记录的 actor、子题和代次保持 NULL，API明确返回未知历史归属，没有自动填成当前管理员；迁移也没有补造重评事件。

2026-10-14 当前题库审计候选再次通过 schema 2 → 13 真实备份恢复与基线读取演练，最终报告 `/private/tmp/exam-goal-restore-rehearsals/restore-rehearsal-c571d9299d5e4cda8eec7fe1d49f9a4c/report.json`（`passed:true`）。报告记录最终候选和基线二进制 SHA-256、匹配旧库读取、再升级、迁移幂等与临时数据库清理。题库审计不回填历史操作者，迁移后没有历史操作记录的版本在页面显示“未记录操作者”。仍不代表生产回退版本已获批准或真实数据恢复验收。

检测任务租约围栏修复后的当前候选也通过 schema 2 → 13 原生演练，报告 `/private/tmp/exam-goal-restore-rehearsals/restore-rehearsal-4247be6783de4e7e95d63a1598adb108/report.json`（`passed:true`）。迁移文件未变；此次重新记录实际当前 API 二进制摘要，验证历史读取、成组恢复、再次升级和临时数据库清理。
