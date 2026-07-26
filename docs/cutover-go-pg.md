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

## 已知遗留 (真上生产前需补)

### 1. Runs API 设计: token 侧车 + exam_run_dir 单实例约束

- **`RunTokenDir` 本机为绝对路径**, 生产部署需配 `D:\exam-tokens\` 等持久目录, 且**进程外不能
  多机共享** (单实例文件写不跨节点). 真上多机生产时需改 redesign:
  - 方案 A: 用 PG 加密表列 (PG row + aes-gcm 加密 token, key 在 secrets)
  - 方案 B: 用 Redis / Vault 存加密 token
  - 方案 C: 重回 Python 设计 (单实例 + exam_run_dir)
  **决断未做, 待真上多机生产时拍**.
- **进程重启**: token 侧车文件仍在, 但 admin 内存 inflight 状态丢失, 处理中 run 可能需人工状态对账.

### 2. QR endpoint 字段空驶

- `qr_base64` 字段当前永远返 "", Go 当前未引 QR 库. UI 收到空字段需 fallback 自行生成 QR
  (UI admin.js 已有 base64 qr 处理逻辑). 后续如需后端生成 QR 加 `go-qrcode`/`skip2/go-qrcode` 库.

### 3. resetRoundsHandler 仍是 stub

- plan 行 1879 要求 resetRounds 真实现 (删 run + finalize 桥接). UI 不调, **暂不补**, 真到需求
  再补.

### 4. listExamsHandler 仅返 papers slug, 不返 run 状态

- UI 需要每个 paper 的当前 open run 状态. 当前只返 slug. 真上可补 `RunService.FindOpenBySlug`
  循环联查 (但 N+1 性能考量, 应写 `findOpenByManySlugs` 批量查).

### 5. Admin stub 测试覆盖

- 新真 handler 暂用 curl 真实跑通验证, 未补 Go 单元测试. **真上生产前需补**
  `admin_test.go` batchOpen / batchClose / examLinkGET 三场景.

### 6. 单元测试 / 调试日记: 旧 fail 历史路径

- 本仓库 Python 旧栈 27-30 黑盒 fail / fixture portability / go_enhancements 字段已按
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
