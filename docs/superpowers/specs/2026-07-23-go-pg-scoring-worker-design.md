# 方案 B：Go API + PostgreSQL + Python 评分 Worker

**日期：** 2026-07-23
**状态：** 设计已批准，待实施
**前置 spec：** [2026-07-22 考试轮次、草稿自动保存与考试管理设计](2026-07-22-exam-runs-and-draft-autosave-design.md)

## 背景

当前系统基于 Python (FastAPI + Uvicorn) 单进程 + SQLite，在默认配置下同时在线答题约 50–100 人；齐交卷峰值更低。目标是支持 **最高 500 人同时在线考试**，部署在 **普通办公 Windows 主机 + 本机 PostgreSQL Windows 服务**。

选择 **方案 B：Go 重写 API + PostgreSQL + Python 评分 Worker**，兼顾性能、Windows 友好部署、以及不重写 `subjective-scoring` 的投入产出比。

## 第 1 节：目标、边界与成功标准

### 目标

在 **一台普通办公 Windows 主机** 上，支持：

| 指标 | 目标 |
|---|---|
| 同时在线答题 | **最高 500** |
| 齐开考 | **500 人在 1–2 分钟窗口内陆续开考** 可完成 |
| 齐交卷 | **500 人先成功交卷（落库）**；主观题后台评分可排队 |
| 部署形态 | 本机 **PostgreSQL Windows 服务** + Go 主服务 + Python 评分 Worker |
| 前端 | **不重构**；尽量保持现有 API 契约，少改 `frontend/` |

### 非目标

- 不重写 `subjective-scoring`（文本/SQL/代码/语义评分引擎）
- 不引入 Redis / 消息中间件（减少 Windows 办公机依赖）
- 不做多机集群 / K8s
- 不做 Rust 重写
- 不追求「交卷瞬间主观题分同步返回」
- 不重构前端（API 契约兼容）
- 不在第一期迁移试卷格式（继续 `index.json` + `{slug}.json`）

### 架构原则

1. **热路径（开考 / 草稿 / 交卷）必须快且不阻塞在评分上**
2. **写放大可控**：草稿合并写，交卷先落库
3. **评分可堆积、可重试、可观察**
4. **Windows 可运维**：服务少、可装成服务、日志清晰
5. **API 兼容优先**：现有 `exam.js` / `admin.js` 尽量零改或小改

### 成功标准

- 压测：**500 在线稳态答题** 时草稿保存成功率 ≥ 99%，P95 延迟可接受（例如 < 500ms 本机局域网）
- 压测：**500 交卷峰值** 时「提交成功」≥ 99%（允许评分排队）
- 单机依赖清单固定为：**PostgreSQL + exam-server.exe + scoring-worker**
- 管理端复核、导出、试卷管理功能行为与现网一致（评分从 sync 变为 async 后的状态展示差异需明确）

### 关键行为变化（必须接受）

当前默认 `sync_grading: true`：交卷可能卡住等判分。
目标架构下：

- 交卷 API **始终先落库并返回** `submission_id` + `grading_status=pending|grading|done|failed`
- 前端已有 `/api/submission/{id}/status`，继续用轮询看评分进度（可小改文案）
- 客观题可在 Go 内即时算；主观题由 Worker 异步回写（一期统一在 Worker 内算整卷）

---

## 第 2 节：Windows 进程拓扑与模块边界

### 2.1 部署拓扑（单机）

```text
┌─────────────────────────────────────────────────────────────┐
│  普通办公 Windows 主机                                        │
│                                                             │
│  [PostgreSQL Windows 服务]  :5432  常驻                       │
│           ▲                                                 │
│           │ SQL                                             │
│  ┌────────┴────────┐      ┌─────────────────────────────┐  │
│  │ exam-server.exe │      │ scoring-worker (Python)     │  │
│  │ (Go API)        │      │ 1~N 进程                    │  │
│  │ :8000           │◄─DB──┤ 抢占 grading_jobs           │  │
│  │ 静态前端        │      │ 调 subjective-scoring       │  │
│  └────────┬────────┘      └─────────────────────────────┘  │
│           │ HTTP                                           │
└───────────┼─────────────────────────────────────────────────┘
            │
     浏览器 / 扫码手机（局域网）
```

| 进程 | 职责 | 崩溃影响 |
|---|---|---|
| **PostgreSQL** | 真源：轮次/会话/草稿/提交/任务 | 全站不可用（预期） |
| **exam-server.exe** | HTTP API + 静态资源 + 收卷循环 + 客观题 | 考生断连；数据在 PG 不丢；重启可恢复 |
| **scoring-worker** | 主观题评分、写回成绩 | 交卷仍成功，成绩排队；重启继续捞任务 |

### 2.2 Go 模块边界（建议包结构）

```text
cmd/exam-server/          # main：配置、HTTP、生命周期
internal/
  config/                 # YAML + 环境变量
  db/                     # pgx 连接池、迁移
  auth/                   # admin cookie/session、token hash
  papers/                 # 试卷 CRUD、快照读写（文件）
  runs/                   # exam_runs 状态机、发布/关闭
  sessions/               # 开考、草稿、会话鉴权
  submit/                 # 交卷、幂等、pending 落库
  objective/              # 客观题判分（从 Python 移植）
  review/                 # 人工复核、regrade 入队
  export/                 # Excel 导出
  ratelimit/              # 内存限流（可后续外置）
  finalize/               # closing 收卷循环
  httpapi/                # 路由与 DTO（兼容现有 JSON）
  static/                 # 挂载 frontend/
```

**原则：** 路由层不直接拼 SQL；业务在 `runs/sessions/submit`；评分调度只写 `grading_jobs`，不内嵌 Python。

### 2.3 Python Worker 边界

```text
scoring_worker/
  main.py                 # 抢任务循环
  grader_bridge.py        # 复用/包装现有 backend.grader 逻辑
  claim.py                # FOR UPDATE SKIP LOCKED
```

第一期允许 Worker **复用现有 `backend/grader.py` + `subjective-scoring`**，通过读 PG 任务、写回结果对接；不必先把评分逻辑迁到独立包。长期可抽成独立 Python 包，与 Go 解耦。

### 2.4 队列选型（无 Redis）

用 **PostgreSQL 表 `grading_jobs` + `FOR UPDATE SKIP LOCKED`**：

- 零新中间件，符合「办公 Windows 少依赖」
- 崩溃可恢复、可重试
- 500 人齐交卷：任务堆积在表里，不堵交卷 API

---

## 第 3 节：数据模型（PostgreSQL）

### 3.1 迁移原则

- 语义对齐现有 SQLite 表：`exam_runs` / `exam_sessions` / `submissions` / `review_logs`
- 类型升级：`TEXT` 时间 → `TIMESTAMPTZ`；JSON → `JSONB`；布尔用 `BOOLEAN`
- ID：`exam_runs.id` / `exam_sessions.id` 仍可用 UUID 字符串；`submissions.id` 用 `BIGSERIAL`
- 同一试卷最多一个 `open|closing` 轮次：用 **partial unique index**

### 3.2 核心表

#### `exam_runs`

| 列 | 类型 | 说明 |
|---|---|---|
| id | TEXT PK | run_id |
| paper_id | TEXT NOT NULL | |
| round_no | INT NOT NULL | UNIQUE(paper_id, round_no) |
| public_token_hash | TEXT UNIQUE | |
| status | TEXT NOT NULL | open / closing / closed |
| duration_minutes | INT NOT NULL | |
| snapshot_path | TEXT | `data/exam_runs/{id}.json` |
| snapshot_hash | TEXT | |
| is_legacy | BOOLEAN DEFAULT false | |
| opened_at, closing_started_at, finalize_at, closed_at, created_at | TIMESTAMPTZ | |

```sql
CREATE UNIQUE INDEX uq_exam_runs_active
  ON exam_runs (paper_id)
  WHERE status IN ('open', 'closing');
```

#### `exam_sessions`

| 列 | 类型 | 说明 |
|---|---|---|
| id | TEXT PK | |
| run_id | TEXT NOT NULL FK | |
| employee_id, name | TEXT | |
| department | TEXT | |
| session_token_hash | TEXT NOT NULL | |
| started_at, deadline_at | TIMESTAMPTZ | |
| draft_json | JSONB NOT NULL DEFAULT '{}' | |
| draft_revision | INT NOT NULL DEFAULT 0 | CAS |
| draft_saved_at | TIMESTAMPTZ | |
| status | TEXT | active / submitted |
| client_ip, user_agent | TEXT | |
| created_at, updated_at | TIMESTAMPTZ | |
| UNIQUE(run_id, employee_id) | | |

索引：`session_token_hash`；`(run_id, status)`。

#### `submissions`

在现有字段上明确评分状态：

| 列 | 类型 | 说明 |
|---|---|---|
| id | BIGSERIAL PK | |
| name, employee_id | TEXT | |
| paper_id | TEXT | |
| paper_name | TEXT | |
| run_id | TEXT NOT NULL | |
| department | TEXT | |
| answers_json | JSONB | |
| grading_detail_json | JSONB NOT NULL DEFAULT '[]' | |
| objective_score / subjective_score_machine / subjective_score_final / total_score | DOUBLE PRECISION | |
| review_status | TEXT | pending / auto_passed / need_review / reviewed 等（保持现语义） |
| **grading_status** | TEXT NOT NULL | **pending / grading / done / failed**（新增，对外可映射） |
| **grading_error** | TEXT | 失败原因摘要 |
| **graded_at** | TIMESTAMPTZ | |
| started_at | TIMESTAMPTZ | |
| submitted_at | TIMESTAMPTZ | |
| reviewed_at | TIMESTAMPTZ | |
| reviewer_note | TEXT | |
| client_ip, user_agent | TEXT | |
| auto_submit_reason | TEXT | |
| UNIQUE(employee_id, run_id) | | |

说明：现网 `review_status=pending` 已表示「待评分/待处理」。为避免混淆，新增 **`grading_status`** 专管机器评分流水线；API 可继续返回兼容字段 `status: "grading"`。

#### `grading_jobs`（新）

| 列 | 类型 | 说明 |
|---|---|---|
| id | BIGSERIAL PK | |
| submission_id | BIGINT UNIQUE NOT NULL | 一提交一任务 |
| paper_id | TEXT NOT NULL | |
| run_id | TEXT NOT NULL | |
| status | TEXT NOT NULL | queued / leased / done / failed / dead |
| attempts | INT DEFAULT 0 | |
| max_attempts | INT DEFAULT 5 | |
| lease_owner | TEXT | worker 实例 ID |
| lease_until | TIMESTAMPTZ | 租约，防僵尸 |
| available_at | TIMESTAMPTZ | 延迟重试 |
| last_error | TEXT | |
| created_at, updated_at | TIMESTAMPTZ | |

```sql
CREATE INDEX idx_grading_jobs_claim
  ON grading_jobs (available_at)
  WHERE status IN ('queued', 'leased');
```

#### `review_logs`

与现网一致，FK → submissions。

### 3.3 文件仍在磁盘（不进 PG）

| 路径 | 内容 |
|---|---|
| `data/papers/` | 可编辑试卷 |
| `data/exam_runs/{run_id}.json` | 轮次不可变快照 |
| `data/run_tokens/`（若有） | 明文 token 存储策略与现网对齐或改为仅 hash+展示时一次性返回 |

试卷元数据继续文件驱动；**运行态只信 PG**。

### 3.4 从 SQLite 迁移

1. 停考窗口：无 open/closing 轮次
2. 备份 `exam.db`
3. 工具 `cmd/migrate-sqlite` 或 Python 一次性脚本：表对表导入
4. 校验：row count、抽样 submission、run 唯一约束
5. 历史 `is_legacy` 轮次照旧
6. 迁移后默认 **异步评分**；旧 pending 可生成 `grading_jobs` 补跑

---

## 第 4 节：API 兼容策略

### 4.1 原则

- **路径、方法、主字段名尽量不变**，保证 `frontend/js/exam.js` / `admin.js` 少改
- 交卷默认走「先成功后评分」（对应现网 `sync_grading=false` + `schedule_grading` 路径，但实现改为 PG 任务表）
- 错误码字符串尽量保留：`STALE_DRAFT_REVISION`、`DUPLICATE_SUBMISSION`、`RUN_CLOSING` 等

### 4.2 考生端（必须兼容）

| 方法 | 路径 | 行为要点 |
|---|---|---|
| GET | `/api/exam` | 快照 + run 状态 |
| POST | `/api/exam/start` | 创建/恢复会话，不重置计时 |
| PUT | `/api/exam/sessions/{id}/draft` | revision CAS；**服务端可节流** |
| GET | `/api/exam/sessions/{id}/status` | open/closing/closed、deadline |
| POST | `/api/submit` | 落库 + 入队；返回 grading |
| GET | `/api/submission/{id}/status` | 轮询评分进度 |
| GET | `/api/health` | liveness |

### 4.3 管理端

保持现有 admin 路由集合（login、papers CRUD、open/close、submissions、review、regrade、export、stats、exam-link…）。实现语言换 Go，**契约不变**。

`POST /api/admin/regrade/{id}`：重置 `grading_status` + 新/重置 job，不阻塞 HTTP。

### 4.4 交卷响应（兼容增强）

```json
{
  "success": true,
  "submission_id": 123,
  "status": "grading",
  "grading_status": "pending",
  "paper_id": "default",
  "run_id": "...",
  "message": "提交成功，系统正在评分中"
}
```

旧前端只认 `status` 仍可用。

### 4.5 静态页

Go 继续托管 `/` `/exam` `/admin` `/detail` 与 `frontend/**`，与现 FastAPI 静态行为一致。

---

## 第 5 节：草稿写路径（500 人关键）

### 5.1 问题

500 × 约 2s 一轮脏写 → 量级上 **每秒数十到上百次 UPDATE**，再叠加齐交卷会压垮磁盘与连接。

### 5.2 策略（组合）

| 层 | 措施 |
|---|---|
| **前端（小改）** | 建议间隔 **5s**（可配置）；仅脏数据；离线缓冲最后一次 |
| **服务端** | 接受 2s 客户端也不崩：同一 session **最小写入间隔**（如 2s），过密返回当前 revision + `saved:false` 或 200 但 `throttled:true`（需约定，避免前端当失败狂重试） |
| **CAS** | `WHERE id=? AND draft_revision=?` 成功则 +1；否则 `409 STALE_DRAFT_REVISION` |
| **Payload** | 整包 draft JSONB 覆盖（与现网一致，不存历史） |
| **连接** | pgx 池：建议 `max_conns ≈ CPU*2~4`，办公机如 8 核可 16–32 |

### 5.3 推荐默认配置

```yaml
draft:
  min_server_interval_ms: 2000
  max_json_bytes: 512000
frontend_recommended_interval_ms: 5000
```

目标：**稳态 500 在线时草稿成功率 ≥ 99%**，偶发 throttle 不丢最终交卷（交卷带全量 answers）。

---

## 第 6 节：开考 / 交卷 / 评分流水线

### 6.1 开考

1. 校验 run=`open`、token、防重复（同 run+employee 恢复原 session）
2. 插入/返回 session + deadline
3. 限流：同 IP 开考次数（继承现逻辑量级，如 10/分钟，可配置）

**齐开考：** 纯 PG 插入 + 唯一约束，Go 无全局锁；500 人 1–2 分钟散开为设计目标，**同一两秒硬齐开**仍建议业务错峰。

### 6.2 交卷（热路径，必须短）

事务内：

1. 鉴权 session，校验 run=`open`、未提交、deadline+grace
2. 校验 answers（对照快照题型）
3. `INSERT submissions`（scores 先 0，`grading_status=pending`）
4. `UPDATE exam_sessions SET status=submitted`
5. `INSERT grading_jobs (... status=queued)`
6. COMMIT

然后立即返回。**Worker 内统一算整卷**（实现简单，与现 `grade_submission` 一致）。

### 6.3 Worker 抢任务

```sql
BEGIN;
SELECT id FROM grading_jobs
 WHERE status IN ('queued','leased')
   AND available_at <= now()
   AND (status = 'queued' OR lease_until < now())
 ORDER BY id
 FOR UPDATE SKIP LOCKED
 LIMIT 1;

UPDATE grading_jobs SET
  status='leased', lease_owner=$worker, lease_until=now()+interval '5 minutes',
  attempts=attempts+1, updated_at=now()
 WHERE id=$id;
COMMIT;
```

评分成功：

- 写 `submissions` 分数 + `grading_detail_json` + `grading_status=done` + `review_status`
- job → `done`

失败：

- attempts < max → `queued`，`available_at = now() + backoff`
- 否则 `dead`，submission `grading_status=failed`，`review_status=need_review`

### 6.4 管理员收卷（closing）

保持现语义：

```text
open → closing（5s 只读写最终草稿）→ 批量按 draft 生成 pending submission + jobs → closed
```

收卷循环在 **Go finalize 协程**（每秒扫描 `finalize_at <= now()`），幂等条件更新。

### 6.5 同步评分配置

`grading.sync_grading`：

- **目标架构默认 false**（忽略 true，或 true 仅 dev 单测）
- 文档明确：500 人场景禁止同步等主观题

---

## 第 7 节：限流、安全、会话

| 项 | 设计 |
|---|---|
| 限流 | Go 进程内 token bucket（按 IP+路由）；多实例时不共享——**单机单 exam-server 即可** |
| Admin | 密码 + HttpOnly cookie/session（行为对齐现网） |
| Session token | 只存 hash；客户端持有明文 |
| CORS | 配置 `allow_origins`，内网可 `*` 但不建议生产习惯 |
| 输入限制 | draft/answers 体积上限，防恶意大包 |
| 密码 | config 明文仅限内网 MVP；文档提示改密 |

---

## 第 8 节：Windows 部署与运维

### 8.1 安装清单

1. PostgreSQL 16+ Windows 安装器 → 服务自动启动
2. 创建库/用户：`exam` / 强密码
3. 解压发布包：
   ```text
   exam-system/
     exam-server.exe
     config.yaml
     frontend/
     data/papers/
     scoring-worker/   # 或 embedding 进 PATH 的 venv
     start.bat / install-services.ps1
   ```
4. Python 3.11+ + venv + `subjective-scoring` 依赖（办公机若用本地 rerank 需模型文件；**推荐远程 rerank 省内存**）
5. `exam-server.exe migrate` 执行 SQL 迁移
6. 注册两项 Windows 服务或计划任务：
   - `ExamSystemAPI`
   - `ExamSystemScoringWorker`（可 2 实例）

### 8.2 配置示例（片段）

```yaml
server:
  host: "0.0.0.0"
  port: 8000

database:
  url: "postgres://exam:***@127.0.0.1:5432/exam?sslmode=disable"
  max_conns: 32
  min_conns: 4

grading:
  sync_grading: false
  worker_lease_seconds: 300
  max_attempts: 5

draft:
  min_server_interval_ms: 2000

worker:
  poll_interval_ms: 200
  concurrency: 2
```

### 8.3 硬件建议（500 目标）

| 资源 | 建议 |
|---|---|
| CPU | 8 核 |
| RAM | 16GB（本地大模型则 32GB+ 或改远程） |
| 磁盘 | SSD |
| 网 | 千兆局域网 |

### 8.4 可观测

- 结构化日志：开考/草稿 throttle/交卷/claim/评分耗时
- `/api/health`：PG ping + 可选 `queued_jobs` 计数
- 管理端可加「评分队列深度」（第二期）

---

## 第 9 节：判分职责切分

| 题型 | 执行位置 | 说明 |
|---|---|---|
| single/multiple/true_false | Python Worker（一期）或 Go（二期） | 一期跟 `grade_submission` 走，避免分叉 |
| short_answer / essay / composite | Python `subjective-scoring` | **不迁移** |
| 人工复核 | Go API 写 PG | 与现 review 语义一致 |
| 重评 | Go 入队 | Worker 重跑 |

**二期可选：** 客观题挪到 Go 交卷路径，缩短「可见客观分」时间；非 500 硬前置。

---

## 第 10 节：前端改动范围（刻意最小）

| 项 | 是否必须 | 说明 |
|---|---|---|
| 草稿间隔 2s→5s | **强烈建议** | 配置或常量 |
| 交卷后轮询 status | 已有则保持 | 文案「评分中」 |
| throttle 响应 | 若服务端新增字段 | 勿当致命错误清空答案 |
| 管理端 | 基本不改 | 列表多「评分中」状态映射 `grading_status` |

**不重构 UI 框架、不改 Apple Light 方向。**

---

## 第 11 节：容量与背压

| 场景 | 机制 |
|---|---|
| 草稿风暴 | 服务端 min interval + 前端 5s |
| 齐交卷 | 交卷 O(1) 事务；jobs 堆积 |
| 评分慢 | 增加 worker 进程数；远程 rerank；队列 deep 告警 |
| 连接打满 | 池上限 + 快速失败 503，避免雪崩 |
| IP 限流 | 公司 NAT 可能误伤 → 可按路径调大 draft 限额或按 session 限流（二期） |

**预估（架构级，非实测）：**

| 场景 | 目标 |
|---|---|
| 同时在线 | 500 |
| 齐交卷先成功 | 300–500 |
| 评分完成时间 | 视主观题与 worker，可能数分钟级排队（可接受） |

---

## 第 12 节：测试与压测计划

### 12.1 正确性

- 移植/重写：run 状态机、draft CAS、closing 收卷、重复提交、超时 grace
- 迁移工具：空库/有历史提交
- Worker：lease 过期回收、死信、regrade

### 12.2 压测三条曲线（必须做）

1. **稳态答题**：500 VU，5s 草稿，持续 30–60min
2. **开考峰值**：500 在 60s 内 start
3. **交卷峰值**：500 在 60s 内 submit（可 mock 评分或轻量 worker）

工具：k6 / vegeta / 自研 Go 压测客户端均可。

### 12.3 通过标准（与第 1 节对齐）

- 草稿成功率 ≥ 99%
- 交卷成功 ≥ 99%
- API 进程无崩溃；PG 无长时间锁等待爆炸
- 评分最终一致性：queued → done/failed，无永久租赁泄漏

---

## 第 13 节：实施阶段（建议）

| 阶段 | 内容 | 产出 |
|---|---|---|
| **P0** | PG schema + 迁移脚本 + 本地 docker/Windows PG 开发环境 | 可导入数据 |
| **P1** | Go：health、config、papers 只读、exam start/draft/status/submit | 考生主路径通 |
| **P2** | grading_jobs + Python worker 打通 | 异步评分 E2E |
| **P3** | admin API 全量 + 收卷循环 + export | 功能对齐 |
| **P4** | 前端草稿间隔 + 状态展示小改 | 体验 |
| **P5** | Windows 服务脚本 + 压测 + 调参 | 500 证据 |
| **P6** | 文档、回滚手册、切流 | 可上线 |

**回滚：** 保留旧 Python+SQLite 安装包；切流前全量备份 PG；问题则 DNS/端口切回旧进程（需停考窗口）。

---

## 第 14 节：风险与决策表

| 风险 | 缓解 |
|---|---|
| Go 重写 API 行为有偏差 | 契约测试对照现网响应；分阶段替换路由 |
| 本地 embedding 内存爆 | 默认远程 rerank；worker concurrency=1~2 |
| NAT 限流误伤 | draft 按 session 限流；管理端出口白名单 |
| 双写文件+PG 不一致 | 发布快照与 run 行同事务尽量「先写文件再提交 PG」，失败清理 |
| 办公机休眠/杀进程 | Windows 服务 + 开机自启；lease 回收 |
| 迁移丢数据 | 强制备份 + 校验脚本 |

| 决策 | 选择 |
|---|---|
| 语言 | Go API |
| DB | PostgreSQL |
| 队列 | PG job 表 |
| 评分 | Python worker + subjective-scoring |
| 前端 | 不重构 |
| 默认同步评分 | 关闭 |
| Redis | 不用（一期） |

---

## 第 15 节：目录与仓库形态（建议）

```text
examSystem/
  backend/                 # 现有 Python（过渡期保留；worker 复用）
  frontend/                # 不动结构
  cmd/exam-server/         # 新 Go
  internal/...             # 新 Go
  migrations/*.sql
  scoring_worker/          # 或 scripts/ 下
  docs/superpowers/specs/2026-07-23-go-pg-scoring-worker-design.md
  config.yaml
```

过渡期可 **Python 与 Go 并存**，用端口切换验证；稳定后 Python 仅作 worker 依赖。

---

## 一句话总览

> **Go 扛 500 人开考/草稿/交卷，PostgreSQL 做真源与任务队列，Python 只做主观题评分；前端基本不动；Windows 三件套服务化。**
