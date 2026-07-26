# Go + PostgreSQL 栈迁移收尾文档 (Task 14)

plan 行 1819-1901 原设想的 Task 14 范围: 停考 + 备份 + 数据迁移 + 切流演练 + 放行. 但本仓库
Python+SQLite 旧栈未进生产环境, 无生产数据和真实流量需迁移 -> **整七步切流演练在本次开发期不适用**.

本文档诚实记录: (a) 本开发期实际落地的 Task 14 工作; (b) 真实工程遗留清单 (待后续真上生产时补);
(c) 核心目的 (Go+PG 支撑更高并发 / 可运维) 的达成情况.

## 本次实施 (Task 14 本开发期)

### Task 9 admin 路径 4 stub 真实现 + 路由 mismatch 修复

核查发现 Task 9 admin.go 留存 5 个 stub handler 中有 4 处 UI 真调用 + 1 处路由方法不匹配:

| Stub | 真调用? | 修复内容 |
|---|---|---|
| openRunHandler | 不直接调 (UI 走 batch/open) | **真实现**: 调 RunService.Open 建运行, 返 url |
| closeRunHandler | 不直接调 (UI 走 batch/close) | **真实现**: 调 RunService.BeginClose |
| #batchOpenHandler (新) | UI 调 POST /papers/batch/open | **新加**: 循环调 Open |
| #batchCloseHandler (新) | UI 调 POST /papers/batch/close | **新加**: 循环调 BeginnerClose |
| #examLinkHandlerGET (新) | UI 用 GET, 旧后端只 POST | **新加 GET**: 从 token 侧车重建 url |
| examLinkHandler (POST) | 保留向后兼容 | 真实现: 调 Open 创建新 run |
| listExamsHandler | UI 不调 | stub 保留, 返 papers.List, 真 |
| resetRoundsHandler | UI 不调 | stub 保留 (note 标记) |
| batchReorderHandler | UI 不调 | 真 3 段纸卷 (Task 9 已真) |

### 配套基础设施改动

| 文件 | 改动 | 用途 |
|---|---|---|
| `internal/runs/service.go` | 加 token 侧车文件 (0600) Save/Load/Remove | 与 Python 旧栈 exam_run_service.py parity: 明文 token 不入 DB |
| `internal/runs/service.go` | 加 FindOpenBySlug | examLink GET 查最近 open run |
| `internal/runs/repository.go` | 加 FindOpenByPaper | 上一项的底层 |
| `internal/httpapi/router.go` | Dependencies 加 RunService *runs.Service | 注入 admin 4 真 handler |
| `internal/config/config.go` | LoggingConfig 加 RunTokenDir | 配置 token 侧车目录 |
| `cmd/exam-server/main.go` | 装配 runService + WithTokenDir + 注入 | 链路通 |
| `internal/httpapi/admin.go` | 5 stub 替换为 4 真 + 2 新 batch routes + 1 GET | 修复路由 mismatch + 真业务 |
| `internal/httpapi/admin.go` | 新 withTx helper | pgxpool.Pool 不带 WithTx, 用 Begin/Commit/Rollback |

### 验证证据 (loadtest schema 隔离下跑通)

```bash
# 1. POST /api/admin/papers/batch/open
{"ok":true,"runs":[{"slug":"a-tail","run_id":"run-e68c...","public_token":"fKrFjj...",
                    "url":"http://127.0.0.1:18080/?run_token=fKrF...&paper_slug=a-tail"}]}

# 2. GET /api/admin/exam-link?paper_slug=a-tail (从 token 侧车重建 URL)
{"ok":true,"paper_slug":"a-tail","qr_base64":"",
 "url":"http://127.0.0.1:18080/?run_token=fKrF...&paper_slug=a-tail"}

# 3. 文件系统侧车 <token_dir>/<run_id>.token (0600) 真生成
ls /tmp/exam-server-loadtest-tokens/  ->  run-e68c....token

# 4. POST /api/admin/papers/batch/close (BeginClose: open -> closing)
{"ok":true}     # 再次调幂等成功 (NO_OP)
```

go vet ./... exit 0; runs / papers package 单元测试通过.

## 已知遗留 (2026-07-26 fix/admin-tail-blocking 分支勘误版 + 修正先前判断错)

**重要勘误**: 原 Task 14 文档 (合并 commit cce54c2) 把多项状态判断错了, 本节按真实代码
证据 + UI 调用证据 (frontend/js/papers.js, admin.js grep 核查) 重写. 现状已不戳同.

### 1. ~多机 token 持久化改造~ (本部署架构不适用, 不补)

原决断: 单机文件侧车仅单进程, 多机生产需 redesign.
**修正**: 本仓库真实部署架构 (用户 2026-07-26 拍板) = **主控机单台 + 被控机 N 台**
(被控机仅浏览器客户端, 不跑 Go 进程). Go serve 单进程 + 单 PG 连接 + 文件侧车 = 完全适用.
不补, 无需 redesign. 仅真未来上多机集群才需考虑.

### 2. QR 库已引入 (已补 ✅)

原判断: qr_base64 字段永远空, Go 当前未引 QR 库, UI fallback 不渲染.
**修正**: 用户新架构 "主控机显示 QR -> 被控机扫码入场" 是核心入场流程, 必补.
- 引入 `github.com/skip2/go-qrcode` v0.0.0-20200617195104 (go.mod direct dep)
- internal/httpapi/admin.go `makeQRDataURL(url)`: PNG 256 + base64 data URI
- examLinkHandlerGET: `qr_base64` 真返 `data:image/png;base64,iVBORw0K...` (curl 实测 ~1034 字节)

### 3. resetRoundsHandler 真实现 + 路由修 (已补 ✅)

原判断: UI 不调, 暂不补, 真到需求再补.
**修正**: UI 真 调用 `POST /api/admin/exams/reset-rounds` (papers.js:1027), 但后端原
注册 `POST /api/admin/exam-link/{run}/reset-rounds` -> 完全 404 mismatch. 这条不是 "可后补",
是真功能坏.
- 改路由: 新挂 `POST /exams/reset-rounds` 配 UI. 旧路径 ExamLink/{run}/reset-rounds 保留
  向后兼容, 指向同 handler.
- handler 真软重置: active run -> 409 skip (防误删); closed run -> 删 active session +
  删 token 侧车 + 保留 exam_runs 行+submissions 历史 (软重置语义,FK RESTRICT 防硬删)
- 返回 `{ok, deleted, skipped, note}`, UI papers.js 真消费

### 4. listExamsHandler 真 schema 对齐 (已补 ✅)

原判断: UI 不调 + "暂未用".
**修正**: UI 真 调用 `loadExams` (papers.js:738), 而且轮询 (设 15s/1s poll). 之前 handler
返 `{"exams":[slug1, slug2, string-only]}` -> UI 期望裸数组 + 每项含完整 ExamOverview 对象
(paper_id/name/status/opened_at/duration_minutes/started_count/active_count/...).
**两处双 mismatch**: 包装形态错 + 元素错.
- internal/runs/repository.go: 新 `ListExamsOverview(slugs)` (单 SQL: LATERAL +
  DISTINCT ON + LEFT JOIN exam_sessions 三 counter, 避免 N+1)
- internal/httpapi/admin.go listExamsHandler: 真返裸数组 + 每 paper 状态 (含 "unpublished"
  fallback for paper 无 run 的)
- curl 实测: `[{paper_id, name, status:'open', started_count:0, active_count:0, opened_at:'...',
  ...}]` 与 UI renderExamCard 真对齐

### 5. admin_runs_test.go 单元测试已加 (已补 ✅)

原判断: "curl 已验证, 真上生产前需补".
**修正**: 新加 `internal/httpapi/admin_runs_test.go` (6 case):
- TestRoutes_NewBatchAndExamLinkRegistered: 新加 5 路由真挂载 (防 Task 14 后改坏)
- TestBatchOpen_NoDeps_503 / TestBatchOpen_InvalidJSON_503 (deps nil fail-fast)
- TestExamLinkGET_NoDeps_503 / TestResetRounds_NoDeps_503 / TestListExams_NoDeps_503
注: 真 PG fixture 路径 (open/close 真事务) 主控机单机靠 curl 验证; 真上 CI 时可补真 PG
fixture 扩展 (本文档不阻塞).

### 6. ~迁移脚本未建~ (不是遗留, 已废弃)

原判断: 三个 SQLite<->PG 迁移脚本本次未建.
**修正**: 核查发现 Task 2/3 (commit ce9b0a4/34ea12a) 早就建了:
  scripts/sqlite_to_postgres.py ✅ (17KB, 服务 SQLite→PG)
  scripts/postgres_to_sqlite.py ✅ (反向 PG→SQLite 回滚用过)
  scripts/verify_migration.py ✅ (双向数据计数对账)
  tests/test_sqlite_to_pg_to_sqlite_roundtrip.py ✅ (roundtrip 全程跑通)
**这条不是遗留**, 是 Task 14 文档判断错, 废弃.

### 7. 单元测试 / 调试日记: 旧 fail 历史路径 (本节不变)

本仓库 Python 旧栈 27-30 黑盒 fail / fixture portability / go_enhancements 字段已按
`project-abandon-python-contract-tests` 决断 (2026-07-26) 抛弃, 不修.


## 不适用 (本仓库无生产切换)

| plan Task 14 步骤 | 本机不适用原因 |
|---|---|
| Step 1 停考窗口公告 | 无生产考生 |
| Step 2 预切流备份 SQLite | `data/exam.db` 仅开发期测试数据 (245KB) |
| Step 3 空PG 导入并 verify_migration.py | 无生产数据, 三个迁移脚本未建 (见文档收尾) |
| Step 5 回滚演练 | 同上 |
| Step 6 正式切流 | 无生产流量切换 |
| Step 7 放行 | 无运维签字, 仅开发期完成 |

### 迁移工具 (`rollback-tools`)

plan 行 799/1552/1638/1861 反复要求三个 Python 迁移脚本:

- `scripts/migrate_sqlite_to_postgres.py` (SQLite → PG)
- `scripts/export_postgres_to_sqlite.py` (PG → SQLite, 回滚用)
- `scripts/verify_migration.py` (双向数据计数对账)

**本文档记录: 本仓库无生产切换需求 -> 这些工具本次未建**. 真上生产时另立项建:
- 工程量 ~1-2 天
- 关键设计: exam_runsidecar.py token 文件需被迁移脚本辨识并转换 (Python 文件侧车 →
  Go 文件侧车, 路径方案不同 -> 中转需要核对)
- schema 不同 (Python SQLite 与 Go PostgreSQL 表名/列名不同) -> 需要写 mapping table

## 核心目的达成核查

按 `project-python-backend-sqlite` 仲裁原则核心目的 = **Go+PG 支撑更高并发 + 可运维**:

- ✅ 全栈 Go serve 真 build (PE32+ 22MB), `go test ./... -p 1` 通过
- ✅ k6 Task 13 smoke 三场景 (start/draft/submit) 全绿: 本机 p95 3.5/5.4/2.6ms << 阈值,
  submit 8904 RPS / <1% fail_rate
- ✅ admin UI 真 跑通开考/关考/重建 exam-link 3 条核心路径 (Task 14 本文档)
- ✅ scoring_worker `--preflight` 健康自检 (Task 12 Step 5)
- ✅ 部署文档 (Task 12 Step 6) + 回滚文档完整

真上生产 + 真实流量前需补: ①/②/③/④/⑤ 五项已知遗留, **不需要补** Task 14 七步演练.

## fix/admin-tail-blocking 分支补完 (2026-07-26, 新增在勘误版后)

用户拍板 P1 范围扩展 (1+2+3+4), fix/admin-tail-blocking 分支按真实主控机+被控机单机架构
补完前述 4 项 + 1 项(隐含 5):

- ✅ **listExams 真返裸数组 + ExamOverview 聚合** (Task 14 admin 主入口 bug 全修)
  - Repository 新加 `ListExamsOverview` 单 SQL (LATERAL + DISTINCT ON + 三 counter LEFT JOIN)
  - handler 返 `[{paper_id, name, status, opened_at, started_count, active_count, ...}]`
- ✅ **QR 库 引入 + qr_base64 真生成** 配主控机显示二维码给被控机扫码入场
  - `github.com/skip2/go-qrcode` direct dep
  - admin.go `makeQRDataURL` helper + examLinkHandlerGET 真用
- ✅ **resetRoundsHandler 真软重置 + 路由修** UI 真 调用路径
  - POST /exams/reset-rounds 路由真挂 (UI papers.js:1027 真匹配)
  - handler 真软重置: active run skip 409 (防误删未交卷) + closed run 删 active session + 删 token 侧车
- ✅ **admin_runs_test.go 单元测试** 6 case 防回归
  - Task 14 新加 5 路由真挂载 + deps nil 优先 503 + InvalidJSON fail-fast 全覆盖
- ✅ **本文档勘误** 修正先前的多项状态判断错 (1 项架构不适用 / 2-4 项真尾巴 vs 旧判断 "暂未用" / 6 项不是遗留)
- ✅ 关闭 + resetRounds 联 e2e curl 实测 (loadtest schema):
  - open: 状态返 open + counter=0; 关闭后 resetRounds deleted; disabled 多机 token 任务废弃

**真实主控机+被控机架构核查**:
- 单机 Go 进程: ✅ 文件 sidecar token 持久化适用 (本部署形态下设计原 status 适用)
- 单虚拟 PG 连接: ✅ 不涉多机 token 互访
- 被控机浏览器扫码入场: ✅ QR 真生成 + run_token url 真透传
- 单机回滚演练: scripts/postgres_to_sqlite.py 已建 (roundtrip 测试覆盖), 真上生产前需做一次演练
- 真上生产前需补: 一次完整 staging 切换演练 (PG→SQLite→PG, scripts/* 真跑) + admin_test.go 真 PG
  fixture 扩展 (主控机单机 curl 已真跑通, CI 流水线可补 fixture)
