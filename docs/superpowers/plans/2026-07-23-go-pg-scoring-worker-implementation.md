# Go API + PostgreSQL + Python 评分 Worker 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (- [ ]) syntax for tracking.

**Goal:** 在不破坏现有题库、考生端和管理端契约的前提下，将运行态从 FastAPI + SQLite 迁移到 Go + PostgreSQL，并以可恢复、可重试、可防止旧 Worker 覆盖新结果的 Python Worker 异步处理主观题，最终在普通办公 Windows 主机上通过 500 人容量验收。

**Architecture:** Go 服务负责现有 HTTP API、静态资源、试卷文件、轮次/会话状态机、草稿 CAS、客观题判分、管理端、导出和收卷；PostgreSQL 是轮次、会话、提交、复核和评分任务的唯一运行态数据源；Python 3.12 Worker 只读取轮次快照中的主观题并回写该评分代次的结果。迁移期保留现有 Python 服务作为行为基线和可回退版本，不做双写。

**Tech Stack:** Go 1.23、chi/v5、pgx/v5、yaml.v3、excelize/v2、skip2/go-qrcode、PostgreSQL 16、Python 3.12、psycopg 3、subjective-scoring v0.1.7、pytest、k6、PowerShell Scheduled Tasks。

## 事实来源与冲突处理

实现时按以下优先级判断行为，后项不得覆盖前项：

1. **docs/superpowers/specs/2026-07-23-go-pg-scoring-worker-design.md**
2. **docs/superpowers/specs/2026-07-22-exam-runs-and-draft-autosave-design.md**
3. 当前前端请求与字段：**frontend/js/exam.js**、**frontend/js/admin.js**、**frontend/js/papers.js**、**frontend/js/detail.js**
4. 当前 Python 行为：**backend/main.py**、**backend/exam_run_service.py**、**backend/question_loader.py**、**backend/objective_grader.py**、**backend/review_service.py**、**backend/exporter.py**
5. 本计划

若实现中发现上述来源仍有冲突，先补契约测试并以现有前端可正常工作为准，不得自行发明第二套 API。

## Global Constraints

- 只支持平铺试卷根结构：exam_info + questions；不得实现 sections/content/single 之类的新模型。
- 题型固定为 single_choice、multiple_choice、true_false、short_answer、essay、composite。
- 正常轮次的加载、校验、判分和重评必须使用 exam_runs.snapshot_path 指向的不可变快照；run_id 不是 paper_id。
- 考生 API 保留 paper、run、run_token、session_id、session_token、revision、answers 等现有字段。
- 所有领域错误保持 FastAPI 兼容外壳：{"detail":{"code":"CODE","message":"中文说明"}}。
- GET /api/exam 只能返回脱敏快照；必须移除 answer、answers_by_language、scoring_rubric、scoring_points、scoring_points_by_language、calculation，包括复合题子题中的同名字段。
- 草稿、会话状态、交卷均同时校验 session_id 和 session_token；数据库只保存 SHA-256 token 哈希。
- 草稿版本由客户端发送“下一版本”；SQL 必须以 draft_revision = revision - 1 为条件更新，不能先查后无条件更新。
- 交卷事务不调用 Python、不访问远程评分服务；客观题在 Go 内完成，主观题任务与提交同事务入队。
- 纯客观卷不创建 grading_jobs，交卷即 grading_status=done。
- Worker 使用租约 token、租约续期和 grading_generation 三重 fencing；过期 Worker 不得覆盖新任务或人工复核。
- 任意收卷失败必须回滚整个轮次事务并保持 closing；不得无条件标记 closed。
- 迁移脚本任意一行失败即回滚；不得“记录错误后继续”。
- Python Worker 固定使用 Python 3.12，并由 scoring_worker/pyproject.toml 与 scoring_worker/uv.lock 独立锁定；根目录 pyproject.toml/uv.lock 只服务旧基线与迁移工具，不得用于生产 Worker 安装。
- 生产 scoring_worker 只能导入 subjective_scoring、psycopg、标准库及明确列入 scoring_worker/pyproject.toml 的通用依赖；不得 import backend.*。
- 生产必须显式设置 RERANK_USE_REMOTE=true 或 false：远程模式必须完整提供 RERANK_API_URL/RERANK_API_KEY/RERANK_MODEL；本地模式必须安装 Worker 的 local extra 并提供可读模型目录，禁止因缺少 sentence-transformers/torch 或模型文件而静默退化为词法评分。
- 迁移期不删除 backend/ 和 data/exam.db；完成切流和回滚演练后再单独决定清理。
- 新生产发布包不得包含 backend/ 或 data/exam.db；旧 Python 后端保存在独立 rollback-tools，SQLite/config/运行数据保存在带时间戳预切流备份，二者统称回滚资产且不得混入生产 InstallRoot。
- 新生产发布包只允许携带无密码、无数据库 URL、无 API key 的 config.production.example.yaml，不得打包工作区或生产 config.yaml；首次安装必须显式提供 ConfigSource 或确认从脱敏模板初始化。
- data/papers/ 与 data/exam_runs/（包括轮次 JSON 和 .token 侧车文件）是生产持久数据，不属于程序发布物或纯回滚数据；安装、升级、卸载均不得删除、清空或用包内文件覆盖。
- 程序发布包不得携带任何 data/ 内容；install.ps1 必须把独立 Go InstallRoot/data 以 directory junction 指向既有 DataRoot，并在启动前验证 papers/index.json、全部试卷、轮次快照和 token 侧车文件仍可访问。
- Go InstallRoot 必须与旧 Python 根目录、rollback-tools、DataRoot 和备份目录物理分离；不得用“覆盖解压到旧根目录”的方式切流，否则旧 backend/.venv 会残留在生产运行目录。
- 不把普通控制台进程直接交给 New-Service；Windows 一期使用原生 Scheduled Tasks，并明确工作目录、环境变量、日志和失败重启。
- 任何压测脚本都必须包含 thresholds；“500 人 60 秒交卷”必须是 60 秒内总计约 500 次，而不是 500 次/秒。

## 完成定义

以下条件全部满足才可宣称计划实施完成：

- Python 基线与 Go 实现均通过同一套“兼容核心字段”HTTP 契约测试；Go 新增字段另有增强契约测试。
- scoring_worker 的 AST 边界测试确认不存在 backend.* import，Windows 发布清单确认不包含旧 Python 后端。
- scoring_worker 使用独立冻结依赖完成测试与 --check；生产发布包不包含根目录 pyproject.toml/uv.lock、backend/、data/ 或 exam.db。
- Worker 的远程模式和本地模型模式均完成独立环境 --check；本地模式确认实际加载配置模型而非 fallback scorer。
- Windows 安装与升级演练证明 data/papers/、data/exam_runs/ 及其中 .token 文件在安装前后 SHA-256 清单完全一致。
- Go 单元测试、PG 集成测试、Python Worker 测试和前端静态测试全部通过。
- SQLite→PG 迁移演练与 PG→SQLite 回滚演练均通过行数、ID 集合和关键字段校验。
- Worker 的崩溃恢复、租约过期、续租、重试、死信、重评代次和幂等写回均有自动化测试。
- 收卷在进程重启、重复扫描、并发手动提交和单条失败时有自动化测试。
- Windows amd64 构建成功，安装/卸载/启动/停止脚本在 staging 主机验证。
- k6 三条曲线达到本计划阈值，并提交不可手工编辑的 JSON 汇总结果。
- 停考窗口完成一次完整切流和一次完整回滚演练。

---

## 发布闸门

| 闸门 | 可验收产物 | 未通过时禁止 |
|---|---|---|
| G0 契约冻结 | Python 基线契约测试、题库/判分 parity fixtures | 编写业务 handler |
| G1 数据底座 | PG migration runner、schema、双向迁移演练 | 接入真实数据 |
| G2 考生主路径 | GET exam、start、draft、status、submit 全链路 | Worker 与管理端联调 |
| G3 评分与收卷 | Worker fencing/retry、finalize 幂等 | 批量导入历史数据 |
| G4 管理端对齐 | 全路由、XLSX、复核、重评、静态页面 | staging 切流 |
| G5 运维与容量 | Windows 任务、监控、真实 k6 阈值、回滚 | 生产切流 |

每个闸门通过后提交一次可回退的 Git commit。不得把多个闸门压成一个不可审查的大提交。

## 固定契约

### 1. 试卷结构

示例必须使用与 data/papers/software-development.json 相同的平铺结构：

~~~json
{
  "name": "软件开发",
  "exam_info": {
    "title": "软件开发考试",
    "description": "内部考试",
    "total_score": 100,
    "passing_score": 60
  },
  "questions": [
    {
      "id": "q1",
      "type": "single_choice",
      "question": "示例单选题",
      "options": [
        {"key": "A", "text": "甲"},
        {"key": "B", "text": "乙"}
      ],
      "answer": "B",
      "score": 5
    },
    {
      "id": "q2",
      "type": "composite",
      "question": "示例复合题",
      "score": 10,
      "subquestions": [
        {
          "id": "1",
          "question": "说明原因",
          "answer": "参考答案",
          "score": 10,
          "scoring_mode": "text"
        }
      ]
    }
  ]
}
~~~

Go 端使用保留未知字段的 map[string]any 文档模型和 json.Decoder.UseNumber，不得用会在重新写文件时丢失 scoring 配置的窄 struct。

读取时兼容历史别名 sub_questions，但内存规范化和管理端写回统一使用 subquestions。

### 2. 考生端 API

| 方法 | 路径 | 请求关键字段 | 响应关键字段 |
|---|---|---|---|
| GET | /api/exam?paper=software-development&run=公开令牌 | paper、run | paper_id、paper_name、run_id、round_no、run_status、duration_minutes、finalize_at、server_time、auto_submit、config、closed、exam_info、questions |
| POST | /api/exam/start | paper_id、run_token、name、employee_id、department | session_id、session_token、started_at、deadline_at、draft_revision、answers、run_status、session_status、created |
| PUT | /api/exam/sessions/{session_id}/draft | session_token、revision、answers | success、saved、throttled、draft_revision、draft_saved_at、run_status、session_status、finalize_at、retry_after_ms |
| GET | /api/exam/sessions/{session_id}/status?session_token=会话令牌 | session_token | session_id、session_status、run_status、started_at、deadline_at、draft_revision、draft_saved_at、finalize_at、submission_id、server_time |
| POST | /api/submit | session_id、session_token、answers、可选 auto_submit_reason | success、submission_id、status、grading_status、paper_id、run_id、objective_score、message |
| GET | /api/submission/{submission_id}/status | submission_id | submission_id、status、grading_status、review_status |
| GET | /api/health | 无 | ok、database、queued_jobs、time |

兼容交卷响应固定为：

~~~json
{
  "success": true,
  "submission_id": 123,
  "status": "grading",
  "grading_status": "pending",
  "paper_id": "software-development",
  "run_id": "run-20260725-001",
  "objective_score": 35,
  "message": "提交成功，系统正在评分中"
}
~~~

纯客观卷将 status 设为 auto_scored，grading_status 设为 done。

其中 grading_status、objective_score、saved、throttled、retry_after_ms、database 和 queued_jobs 是 Go 目标架构的兼容增强字段。Python 基线测试只断言现有核心字段，Go 增强测试必须额外断言这些字段；不得要求旧 Python 为迁移尚未实现的字段造假。

公开 submission status 端点不得返回姓名、工号、答案、总分或 grading_error；完整成绩和错误只允许经 admin Bearer 访问。

### 3. 管理端路由

POST /api/admin/login 不经过 Bearer 中间件。以下其余 /api/admin 路由在 admin.enable_auth=true 时都要求 Authorization: Bearer：

- GET /api/admin/exam-link?paper={slug}
- GET /api/admin/stats?paper_id={slug}&run_id={run_id}
- GET /api/admin/submissions
- GET /api/admin/submissions/{submission_id}
- DELETE /api/admin/submissions
- POST /api/admin/review
- POST /api/admin/regrade/{submission_id}
- GET /api/admin/export
- GET、POST /api/admin/papers
- GET、PUT、DELETE /api/admin/papers/{slug}
- PATCH /api/admin/papers/{slug}/meta
- POST /api/admin/papers/{slug}/questions
- PUT、DELETE /api/admin/papers/{slug}/questions/{question_id}
- PUT /api/admin/papers/{slug}/questions/reorder
- POST /api/admin/papers/batch/open
- POST /api/admin/papers/batch/close
- POST /api/admin/papers/{slug}/open
- POST /api/admin/papers/{slug}/close
- GET /api/admin/exams
- POST /api/admin/exams/reset-rounds
- GET /api/admin/papers/{slug}/exam-link
- GET /api/admin/papers/{slug}/preview
- POST /api/admin/reload-questions
- POST /api/admin/reload-config

### 4. 状态机

~~~text
exam_runs:     open -> closing -> closed
exam_sessions: active -> submitted
grading_jobs:  queued -> leased -> done
                       -> queued    重试
                       -> dead      达到 max_attempts
                       -> superseded 评分代次已失效
grading_status: pending -> grading -> done
                            |       -> failed
~~~

禁止 closed 回到 open；每次发布创建新 run 和递增 round_no。同一 paper 只允许一个 open 或 closing。

### 5. 复核状态

review_status 与 grading_status 分开：

- grading_status=pending 或 grading 时，管理端显示“评分中”。
- grading_status=failed 时显示“评分失败/待复核”。
- 无主观题时 review_status=auto_scored。
- 主观题全部高置信且无人复核时 review_status=high_confidence。
- 任一题需要复核时 review_status=need_review；低置信时为 low_confidence。
- 所有主观题均人工确认后 review_status=reviewed。

### 6. 关键内部类型

Go 侧跨包类型固定如下，字段名不得在后续 Task 改写：

~~~go
type Document map[string]any
type Detail map[string]any

type SubmitResult struct {
    SubmissionID  int64
    Status        string
    GradingStatus string
    PaperID       string
    RunID         string
    ObjectiveScore float64
}

type EnqueueParams struct {
    SubmissionID int64
    PaperID      string
    RunID        string
    Generation   int64
    MaxAttempts  int
}
~~~

Python Worker 类型固定如下：

~~~python
@dataclass(frozen=True)
class Job:
    id: int
    submission_id: int
    paper_id: str
    run_id: str
    generation: int
    attempts: int
    max_attempts: int
    lease_owner: str
    lease_token: str

@dataclass(frozen=True)
class WorkerResult:
    subjective_score_machine: float
    subjective_score_final: float
    review_status: str
    grading_detail: list[dict[str, Any]]
~~~

total_score 不由 WorkerResult 传入；complete 事务始终用数据库当前 objective_score + WorkerResult.subjective_score_final 计算。

## 文件布局

~~~text
config.production.example.yaml
cmd/
  exam-server/main.go
internal/
  auth/store.go middleware.go
  config/config.go
  db/pool.go migrations.go
  papers/store.go validate.go sanitize.go snapshot.go
  objective/grader.go
  runs/repository.go service.go
  sessions/repository.go service.go
  submissions/repository.go service.go
  jobs/repository.go
  finalize/service.go
  ratelimit/limiter.go
  review/service.go
  export/xlsx.go
  httpapi/router.go errors.go student.go admin.go static.go
  testutil/postgres.go fixtures.go
migrations/
  embed.go
  0001_initial.sql
scoring_worker/
  __init__.py
  main.py
  claim.py
  grader_bridge.py
  repository.py
  pyproject.toml
  uv.lock
scripts/
  migrate_sqlite_to_postgres.py
  export_postgres_to_sqlite.py
  verify_migration.py
  loadtest/prepare.py
  windows/package.ps1
  windows/install.ps1
  windows/uninstall.ps1
  windows/start.ps1
  windows/stop.ps1
loadtest/
  start_peak.js
  draft_steady.js
  submit_peak.js
tests/
  contract/
  migration/
  worker/
  fixtures/contract/
docs/
  deployment-go-pg.md
  rollback-go-pg.md
  cutover-checklist-go-pg.md
~~~

---

### Task 0: 冻结现网契约与 parity fixtures

**Files:**

- Create: tests/contract/conftest.py
- Create: tests/contract/test_student_api.py
- Create: tests/contract/test_admin_api.py
- Create: tests/contract/test_security.py
- Create: tests/contract/test_go_enhancements.py
- Create: tests/contract/fake_worker_entry.py
- Create: tests/fixtures/contract/paper.json
- Create: tests/fixtures/contract/objective_cases.json
- Create: tests/fixtures/contract/sanitized_exam.json
- Modify: .gitignore

**Interfaces:**

- Produces: 一套可同时指向 FastAPI TestClient 和 EXAM_CONTRACT_BASE_URL 的兼容核心黑盒测试。
- Produces: 仅对 Go 目标运行的增强字段和节流行为测试。
- Produces: 客观题和试卷脱敏的固定输入/输出 JSON，Go 与 Python 共同读取。

- [ ] **Step 1: 建立固定题库**

paper.json 必须同时包含：单选、多选、判断、文本题、代码语言题、复合题；复合题必须使用 subquestions。答案中同时放入 answer、answers_by_language、scoring_points、scoring_points_by_language、calculation，用于验证深层脱敏。

- [ ] **Step 2: 写 Python 基线契约测试**

测试必须覆盖：

1. paper + run 查询参数和返回平铺 questions。
2. 开考请求使用 paper_id + run_token。
3. 草稿请求使用 session_token + revision + answers。
4. 状态查询缺少或伪造 session_token 返回 401 INVALID_SESSION_TOKEN。
5. 交卷请求使用 session_id + session_token + answers。
6. GET /api/exam 不出现任何敏感字段。
7. admin login 无 Bearer 可访问，错误密码返回 WRONG_PASSWORD。
8. 每个受保护 admin 路由无 Bearer 返回 401。
9. 管理端 papers、open、close、exams、preview、submissions、detail、review、regrade、export 的字段和 Content-Type。
10. 错误外壳固定为 detail.code + detail.message。

动态 token、时间和 ID 只做类型/格式断言；业务字段做精确断言。

test_go_enhancements.py 在未设置 EXAM_CONTRACT_EXPECT_GO=1 时整文件 skip；启用时要求 draft saved/throttled、submit grading_status/objective_score、submission status grading_status、health database/queued_jobs。

兼容测试对 regrade 只要求 HTTP 成功、success=true 和 submission_id：Python 基线允许同步返回 total_score，Go 增强测试要求异步返回 grading_status=pending 和 generation。该差异是设计批准的行为变化。

所有基线测试必须把 DB_PATH、PAPERS_DIR、INDEX_PATH、EXAM_RUNS_DIR 和配置指向 pytest 临时目录；不得读写工作区 data/exam.db、data/papers/index.json 或现有未提交文件。

契约测试通过 backend.grader.set_subjective_service 注入确定性 fake scorer，不加载本地模型、不调用远程 rerank；XLSX 只检查工作簿内容。

指向 Go 时，测试 harness 使用独立 TEST_DATABASE_URL schema，并启动一个注入 fake scorer 的 Worker 测试进程完成主观题任务；不得增加只为测试存在的生产 HTTP 路由。

- [ ] **Step 3: 固定 objective_cases.json**

至少包含：

- 单选正确/错误/空答案。
- 判断 bool、"true"、"false"、空答案。
- 多选全对、少选按比例、包含错项得 0、partial=false 少选得 0。
- 分数保留到 6 位。

- [ ] **Step 4: 运行基线**

Run:

~~~bash
env PYTHONPATH=. uv run pytest tests/contract -q
env PYTHONPATH=. uv run pytest tests/test_exam_run_service.py tests/test_papers.py tests/test_review_service.py tests/test_frontend_static.py -q
~~~

Expected: 全部 PASS。若基线测试失败，先修正 fixture 或记录现网缺陷，不得开始 Go handler。

- [ ] **Step 5: Commit**

~~~bash
git add tests/contract tests/fixtures/contract .gitignore
git commit -m "test: freeze exam HTTP and scoring contracts"
~~~

---

### Task 1: Go 骨架、配置和可测试启动入口

**Files:**

- Create: go.mod
- Create: cmd/exam-server/main.go
- Create: internal/config/config.go
- Create: internal/httpapi/router.go
- Create: internal/httpapi/errors.go
- Create: internal/httpapi/static.go
- Create: internal/config/config_test.go
- Modify: config.yaml

**Interfaces:**

- Produces: config.Load(path string) (Config, error)
- Produces: httpapi.NewRouter(Dependencies) http.Handler
- Produces: 子命令 serve、migrate、preflight

- [ ] **Step 1: 初始化纯 Go 依赖**

go.mod module 固定为 github.com/yhwyxy/examSystem，Go 版本 1.23。依赖固定为 github.com/go-chi/chi/v5、github.com/jackc/pgx/v5、gopkg.in/yaml.v3、github.com/xuri/excelize/v2、github.com/skip2/go-qrcode、github.com/google/uuid、golang.org/x/time。不得引入 CGO 依赖，以保证 macOS/Linux 可交叉编译 Windows amd64。

- [ ] **Step 2: 写配置失败测试**

覆盖：

- 读取现有 server、exam、scoring、review、grading、model、admin、export。
- 新增 database、draft、worker、logging。
- EXAM_DATABASE_URL 覆盖 database.url。
- EXAM_ADMIN_PASSWORD 覆盖 admin.password。
- database.url 为空时 serve/preflight 失败。
- grading.sync_grading=true 只记录警告，仍采用异步评分。

- [ ] **Step 3: 扩展 config.yaml**

保留用户现有配置，追加：

~~~yaml
database:
  url: ""
  max_conns: 32
  min_conns: 4
  connect_timeout_seconds: 5
  statement_timeout_seconds: 10

draft:
  min_server_interval_ms: 2000
  max_json_bytes: 512000

worker:
  poll_interval_ms: 200
  concurrency: 2
  lease_seconds: 300
  heartbeat_seconds: 60
  max_attempts: 5

logging:
  directory: "logs"
~~~

生产环境通过机器环境变量 EXAM_DATABASE_URL 注入连接串，不在日志中打印密码。

- [ ] **Step 4: 实现路由骨架**

先只挂载 /api/health 和静态页 /、/exam、/admin、/detail、/js/*、/css/*。页面和脚本响应保留 no-store/no-cache 头。未知 /api 路径返回 JSON 404，静态未知路径不得回退为 exam.html。

按 server.allow_origins 实现 CORS：允许 GET、POST、PUT、PATCH、DELETE、OPTIONS，允许 Authorization、Content-Type、X-Requested-With，allow_credentials=false。reload-config 可以刷新业务配置，但 CORS 变更必须在响应 message 中提示重启生效。

- [ ] **Step 5: 验证**

Run:

~~~bash
go test ./internal/config ./internal/httpapi
go vet ./...
GOOS=windows GOARCH=amd64 go build -o /tmp/exam-server.exe ./cmd/exam-server
~~~

Expected: PASS，且 /tmp/exam-server.exe 生成。

- [ ] **Step 6: Commit**

~~~bash
git add go.mod go.sum cmd internal/config internal/httpapi config.yaml
git commit -m "feat: add Go server configuration and router skeleton"
~~~

---

### Task 2: PostgreSQL schema、版本迁移与测试隔离

**Files:**

- Create: migrations/embed.go
- Create: migrations/0001_initial.sql
- Create: internal/db/pool.go
- Create: internal/db/migrations.go
- Create: internal/db/migrations_test.go
- Create: internal/testutil/postgres.go
- Create: docker-compose.postgres.yml
- Modify: cmd/exam-server/main.go

**Interfaces:**

- Produces: db.Open(ctx, config) (*pgxpool.Pool, error)
- Produces: db.Migrate(ctx, pool) error
- Produces: testutil.NewPostgresSchema(t) 独立 schema

- [ ] **Step 1: 写 migration 幂等与 checksum 测试**

测试必须验证：

- 空库一次迁移成功。
- 再次迁移不重复执行。
- 已执行 migration 内容被修改后返回 checksum mismatch。
- 两个并发 migrator 通过 advisory lock 串行化。
- 每个测试使用随机 PostgreSQL schema，不能共享 TRUNCATE 状态。

- [ ] **Step 2: 创建 schema**

0001_initial.sql 必须包含以下数据约束：

~~~sql
CREATE TABLE exam_runs (
    id text PRIMARY KEY,
    paper_id text NOT NULL,
    round_no integer NOT NULL CHECK (round_no > 0),
    public_token_hash text UNIQUE,
    status text NOT NULL CHECK (status IN ('open','closing','closed')),
    duration_minutes integer NOT NULL CHECK (duration_minutes > 0),
    snapshot_path text,
    snapshot_hash text,
    is_legacy boolean NOT NULL DEFAULT false,
    opened_at timestamptz NOT NULL,
    closing_started_at timestamptz,
    finalize_at timestamptz,
    closed_at timestamptz,
    created_at timestamptz NOT NULL,
    UNIQUE (paper_id, round_no),
    CHECK (is_legacy OR (
        public_token_hash IS NOT NULL
        AND snapshot_path IS NOT NULL
        AND snapshot_hash IS NOT NULL
    ))
);

CREATE UNIQUE INDEX uq_exam_runs_active
ON exam_runs (paper_id)
WHERE status IN ('open','closing');

CREATE TABLE exam_sessions (
    id text PRIMARY KEY,
    run_id text NOT NULL REFERENCES exam_runs(id) ON DELETE RESTRICT,
    employee_id text NOT NULL,
    name text NOT NULL,
    department text,
    session_token_hash text NOT NULL,
    started_at timestamptz NOT NULL,
    deadline_at timestamptz NOT NULL,
    draft_json jsonb NOT NULL DEFAULT '{}'::jsonb,
    draft_revision integer NOT NULL DEFAULT 0 CHECK (draft_revision >= 0),
    draft_saved_at timestamptz,
    status text NOT NULL CHECK (status IN ('active','submitted')),
    client_ip text,
    user_agent text,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    UNIQUE (run_id, employee_id)
);

CREATE TABLE submissions (
    id bigserial PRIMARY KEY,
    name text NOT NULL,
    employee_id text NOT NULL,
    paper_id text NOT NULL,
    paper_name text,
    run_id text NOT NULL REFERENCES exam_runs(id) ON DELETE RESTRICT,
    department text,
    answers_json jsonb NOT NULL,
    grading_detail_json jsonb NOT NULL DEFAULT '[]'::jsonb,
    objective_score double precision NOT NULL DEFAULT 0,
    subjective_score_machine double precision NOT NULL DEFAULT 0,
    subjective_score_final double precision NOT NULL DEFAULT 0,
    total_score double precision NOT NULL DEFAULT 0,
    review_status text NOT NULL DEFAULT 'grading',
    grading_status text NOT NULL CHECK (grading_status IN ('pending','grading','done','failed')),
    grading_error text,
    grading_generation bigint NOT NULL DEFAULT 0 CHECK (grading_generation >= 0),
    graded_at timestamptz,
    started_at timestamptz,
    submitted_at timestamptz NOT NULL,
    reviewed_at timestamptz,
    reviewer_note text,
    client_ip text,
    user_agent text,
    auto_submit_reason text,
    UNIQUE (employee_id, run_id)
);

CREATE TABLE review_logs (
    id bigserial PRIMARY KEY,
    submission_id bigint NOT NULL REFERENCES submissions(id) ON DELETE CASCADE,
    question_id text NOT NULL,
    old_score double precision,
    new_score double precision,
    note text,
    created_at timestamptz NOT NULL
);

CREATE TABLE grading_jobs (
    id bigserial PRIMARY KEY,
    submission_id bigint NOT NULL REFERENCES submissions(id) ON DELETE CASCADE,
    paper_id text NOT NULL,
    run_id text NOT NULL REFERENCES exam_runs(id) ON DELETE RESTRICT,
    generation bigint NOT NULL CHECK (generation > 0),
    status text NOT NULL CHECK (status IN ('queued','leased','done','dead','superseded')),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    max_attempts integer NOT NULL DEFAULT 5 CHECK (max_attempts > 0),
    lease_owner text,
    lease_token text,
    lease_until timestamptz,
    available_at timestamptz NOT NULL DEFAULT now(),
    last_error text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (submission_id, generation)
);

CREATE TABLE migration_audit (
    source_sha256 text PRIMARY KEY,
    source_path text NOT NULL,
    table_counts jsonb NOT NULL,
    skipped_legacy_sessions integer NOT NULL DEFAULT 0,
    started_at timestamptz NOT NULL,
    completed_at timestamptz NOT NULL
);
~~~

同时创建：

- exam_sessions(session_token_hash)
- exam_sessions(run_id,status)
- submissions(paper_id,submitted_at DESC)
- submissions(run_id)
- submissions(review_status)
- submissions(grading_status)
- review_logs(submission_id,created_at)
- grading_jobs(available_at,id) partial index，条件为 status IN ('queued','leased')
- grading_jobs(lease_until,id) partial index，条件为 status='leased'

- [ ] **Step 3: 实现 checksum migrator**

runner 自己创建 schema_migrations(version text primary key, checksum text not null, applied_at timestamptz not null)。每个文件在单独事务中执行，使用 pg_advisory_lock；任何 SQL 失败则不记录版本。

- [ ] **Step 4: 接入 CLI**

~~~bash
go run ./cmd/exam-server migrate
~~~

Expected: 首次输出 applied 0001_initial，第二次输出 database schema is current。

- [ ] **Step 5: Commit**

~~~bash
git add migrations internal/db internal/testutil docker-compose.postgres.yml cmd/exam-server/main.go
git commit -m "feat: add PostgreSQL schema and checksum migrations"
~~~

---

### Task 3: SQLite→PG 导入、PG→SQLite 回滚导出与强校验

**Files:**

- Create: scripts/migrate_sqlite_to_postgres.py
- Create: scripts/export_postgres_to_sqlite.py
- Create: scripts/verify_migration.py
- Create: tests/migration/test_sqlite_postgres_roundtrip.py
- Modify: pyproject.toml
- Modify: uv.lock

**Interfaces:**

- Produces: migrate_sqlite_to_postgres.py --sqlite data/exam.db --database-url ...
- Produces: export_postgres_to_sqlite.py --database-url ... --sqlite-out ...
- Produces: verify_migration.py 返回非零表示不可切流

- [ ] **Step 1: 增加 psycopg**

将 psycopg[binary] 加入根项目依赖，保持 requires-python >=3.12，供迁移/回滚脚本使用。更新根目录 uv.lock 并验证 Windows cp312 wheel 可解析；生产 Worker 不读取这份依赖清单，其依赖在 Task 7 单独锁定。

- [ ] **Step 2: 写失败优先的往返测试**

测试源库必须包含：

- 两个 paper、正常和 legacy run。
- submitted session。
- done、grading 两种 submission。
- grading_detail_json、answers_json、auto_submit_reason。
- review_logs。
- 非连续 submission/review_log ID，用于验证 sequence。

测试顺序：SQLite→PG→新 SQLite；最终比较四张业务表的 ID 集合、关键字段和 JSON 语义。

- [ ] **Step 3: 实现导入规则**

导入脚本必须：

1. 只读打开源 SQLite，检查不存在 open/closing run；存在即退出 2。
2. 获取 PostgreSQL advisory lock。
3. 目标业务表非空时拒绝执行，除非 --resume 且 migration_audit 记录与源 SHA-256 相同。
4. 用 PRAGMA table_info/introspection 支持两类源库：当前 run_id schema，以及没有 run_id 的旧 submissions schema。
5. 旧 submissions schema 按 paper_id 创建一个 closed、is_legacy=true 的历史 run，并回填唯一 run_id；round_no 从该 paper 当前最大值加 1。
6. 旧 exam_sessions 若没有 run_id 或 session_token_hash，则在确认无活动考试后不导入，并在 migration_audit 记录 skipped_legacy_sessions；不得猜测归属。
7. 当前 schema 中若 closed run 仍有 status=active 且没有对应 submission 的 session，视为停考数据不一致并拒绝导入；有对应 submission 时规范化为 submitted。
8. 在单个 SERIALIZABLE 事务内按 exam_runs、exam_sessions、submissions、review_logs、grading_jobs 顺序写入。
9. 使用 json.loads 校验每个 JSON 字段；非法 JSON 立即回滚。
10. 导入前验证每个非 legacy run 的 snapshot_path 文件存在且 SHA-256 与 snapshot_hash 一致；不一致立即停止。
11. review_status=grading 的非 legacy 提交先用所属快照和 backend.objective_grader 计算客观 detail/objective_score，再映射 grading_status=pending、generation=1，并创建只补主观题的 queued job；不得把尚未判过的客观题留为 0。
12. review_status=grading 且 run 无快照时映射 grading_status=failed、review_status=need_review、grading_error="legacy run has no immutable snapshot"；不得用当前试卷代替。
13. 其他历史提交映射 grading_status=done。
14. review_logs 列按 submission_id、question_id、old_score、new_score、note、created_at 精确导入。
15. 用 setval 设置 submissions、review_logs、grading_jobs sequence 到 MAX(id)。
16. 写 migration_audit：源绝对路径、SHA-256、各表行数、跳过的旧 session 数、开始/完成时间。
17. 任意 INSERT 或校验失败回滚，不允许跳过坏行。

- [ ] **Step 4: 实现回滚导出**

回滚脚本从 PG 生成一份新的 SQLite 文件，不覆盖输入备份。映射规则：

- grading_status=pending 或 grading → SQLite review_status=grading。
- grading_status=failed → SQLite review_status=need_review。
- grading_status=done → 使用 PG review_status。
- grading_jobs 不写入 SQLite；旧 Python 启动时会按 review_status=grading 恢复评分。
- 保留 exam_runs、exam_sessions、submissions、review_logs 全部 ID。
- 写完后设置 sqlite_sequence。

- [ ] **Step 5: 强校验**

verify_migration.py 必须比较：

- 每张表行数。
- 每张表主键集合。
- submissions 的 employee_id、paper_id、run_id、answers_json、grading_detail_json、四个分数字段、review_status。
- review_logs 的 submission_id 与分数。
- 所有 FK 引用存在。
- active run 数为 0。
- PG sequence 下一值大于当前 MAX(id)。

- [ ] **Step 6: 验证**

Run:

~~~bash
env PYTHONPATH=. uv run pytest tests/migration -q
env PYTHONPATH=. uv run python scripts/migrate_sqlite_to_postgres.py --help
env PYTHONPATH=. uv run python scripts/export_postgres_to_sqlite.py --help
~~~

Expected: PASS；帮助命令不连接数据库。

- [ ] **Step 7: Commit**

~~~bash
git add scripts/migrate_sqlite_to_postgres.py scripts/export_postgres_to_sqlite.py scripts/verify_migration.py tests/migration pyproject.toml uv.lock
git commit -m "feat: add verified SQLite PostgreSQL migration and rollback"
~~~

---

### Task 4: 试卷读取、深层脱敏、快照和客观题 parity

**Files:**

- Create: internal/papers/store.go
- Create: internal/papers/validate.go
- Create: internal/papers/sanitize.go
- Create: internal/papers/snapshot.go
- Create: internal/papers/store_test.go
- Create: internal/papers/sanitize_test.go
- Create: internal/objective/grader.go
- Create: internal/objective/grader_test.go

**Interfaces:**

- Produces: papers.LoadEditable(slug) (Document, error)
- Produces: papers.LoadSnapshot(snapshotPath, expectedSHA256) (Document, error)
- Produces: papers.SanitizeForStudent(Document) Document
- Produces: papers.ValidateAnswers(questions, answers, strict) error
- Produces: objective.Grade(question, answer, partial) Detail

- [ ] **Step 1: 写 parity tests**

Go tests 直接读取 Task 0 fixtures，并与 sanitized_exam.json、objective_cases.json 做深度相等比较。另测：

- slug 正则为 ^[a-z0-9][a-z0-9_-]{0,31}$。
- 禁止 ..、/、反斜杠。
- composite 的答案键必须与全部 subquestions ID 精确相等。
- code 答案允许字符串或 {"answer":"...","language":"go"}，language 必须在 allowed_languages。
- 未知问题 ID 返回 UNKNOWN_QUESTION_ID。
- paper validator 对齐 question_loader.py：题目 ID 唯一、score>0、客观题 options/answer 合法、true_false answer 为 bool、composite 子题非空且分数和等于父题、scoring_mode 与 code_language/allowed_languages 合法、scoring_points 不超过满分、exam_info.total_score 与题目和一致。
- 管理端允许保存空 questions 的编辑中试卷，但发布时返回 EMPTY_QUESTION_BANK。

- [ ] **Step 2: 实现文件安全**

所有相对路径以可配置 data root 解析；读取后使用 filepath.Rel 再确认没有逃逸。写文件使用同目录临时文件、fsync、rename；每个 slug 使用进程内 mutex。

- [ ] **Step 3: 实现快照**

发布时流程固定为：

1. 加载并验证当前 paper。
2. 生成 run_id。
3. 将规范化后的完整 JSON 原子写入 data/exam_runs/{run_id}.json。
4. 对最终字节计算 SHA-256。
5. 后续 PG 事务失败时删除该快照。

LoadSnapshot 必须同时检查路径和 hash；hash 不一致返回 SNAPSHOT_INTEGRITY_ERROR，不能回退到当前 paper。

- [ ] **Step 4: 实现客观题判分**

输出字段与 Python objective_grader.py 对齐：question_id、type、question、student_answer、reference_answer、score、machine_score、final_score、max_score、is_correct、grading_method、confidence、reason、review_status、manually_reviewed、detail。

- [ ] **Step 5: 验证**

~~~bash
go test ./internal/papers ./internal/objective
~~~

Expected: PASS，且敏感字段递归扫描数量为 0。

- [ ] **Step 6: Commit**

~~~bash
git add internal/papers internal/objective
git commit -m "feat: add compatible paper snapshots and objective grading"
~~~

---

### Task 5: 轮次、会话、草稿 CAS 和考生 HTTP 契约

**Files:**

- Create: internal/runs/repository.go
- Create: internal/runs/service.go
- Create: internal/runs/service_test.go
- Create: internal/sessions/repository.go
- Create: internal/sessions/service.go
- Create: internal/sessions/service_test.go
- Create: internal/ratelimit/limiter.go
- Create: internal/ratelimit/limiter_test.go
- Create: internal/httpapi/student.go
- Create: internal/httpapi/student_contract_test.go
- Modify: internal/httpapi/router.go

**Interfaces:**

- Produces: runs.Open、runs.BeginClose、runs.GetPublicExam
- Produces: sessions.StartOrResume、SaveDraft、Status
- Consumes: papers.LoadSnapshot 和 SHA-256 token hash

- [ ] **Step 1: 写状态机和并发失败测试**

覆盖：

- 发布两轮 round_no 为 1、2，旧 token 永远定位旧 run。
- 同 paper 并发发布仅一个成功，另一个 ACTIVE_RUN_EXISTS。
- start 恢复不改变 started_at/deadline_at。
- start 的 run token 与 paper 不匹配返回 RUN_NOT_FOUND。
- 所有 session API 伪造 token 均失败。
- closing 禁止新 start；finalize_at 前只允许已有会话最后保存草稿。
- closed 禁止草稿。
- exam.enable_global_time_window 开启时，开始时间前返回 EXAM_NOT_STARTED，结束时间后返回 EXAM_ENDED。
- 两个请求同时发送 revision=1，只有一个 UPDATE 成功。
- 客户端依次发送 1、2 成功；发送 4 而服务器为 2 返回 STALE_DRAFT_REVISION。
- 超过 max_json_bytes 返回 413 PAYLOAD_TOO_LARGE。

session token 和 public run token 统一使用 crypto/rand 生成 32 字节 URL-safe 值、SHA-256 存储，并用 constant-time 比较哈希。

- [ ] **Step 2: 实现真正 CAS**

核心 SQL 固定为：

~~~sql
UPDATE exam_sessions
SET draft_json = $1,
    draft_revision = $2,
    draft_saved_at = now(),
    updated_at = now()
WHERE id = $3
  AND status = 'active'
  AND draft_revision = $2 - 1
RETURNING draft_revision, draft_saved_at, status;
~~~

UPDATE 返回 0 行时重读当前 revision：

- 请求 revision <= current 或 revision != current+1 → 409 STALE_DRAFT_REVISION，并返回 current_revision。
- session 已提交 → SESSION_SUBMITTED。
- 其他情况 → DRAFT_SAVE_FAILED。

- [ ] **Step 3: 实现安全节流**

若距 draft_saved_at 小于 min_server_interval_ms，且答案不是 closing 最终同步，则返回 HTTP 200：

~~~json
{
  "success": true,
  "saved": false,
  "throttled": true,
  "draft_revision": 7,
  "retry_after_ms": 800,
  "run_status": "open",
  "session_status": "active"
}
~~~

服务端不增加 revision。Task 10 会让前端在 saved=false 时保持 dirty。

- [ ] **Step 4: 实现公开试卷**

GET /api/exam 先按 public_token_hash 查 run，再校验 paper_id；open/closing 返回脱敏快照，closed 返回 questions=[]、exam_info={}、closed=true。本接口绝不直接序列化原始快照。

- [ ] **Step 5: 实现有界限流和请求体限制**

限流器必须按 key 存 token bucket，并以 10 分钟无访问 TTL 清理，最多保留 20000 个 key：

- draft/status：key=session_id，60 次/分钟，burst 10。
- submit：key=session_id，5 次/分钟，burst 2。
- start：key=run_token_hash + employee_id，10 次/分钟，burst 3；不能仅按公司 NAT IP 限制 500 人。
- 其他公开 API：key=IP，300 次/分钟。
- admin：key=IP，120 次/分钟。

429 保持 {"detail":{"code":"RATE_LIMITED","message":"请求过于频繁，请稍后再试"}}。draft 使用 draft.max_json_bytes；start/submit/admin JSON 使用 1 MiB 上限。

- [ ] **Step 6: 运行契约测试**

~~~bash
go test ./internal/runs ./internal/sessions ./internal/ratelimit ./internal/httpapi
~~~

Expected: PASS。

- [ ] **Step 7: Commit**

~~~bash
git add internal/runs internal/sessions internal/ratelimit internal/httpapi
git commit -m "feat: implement run session and draft APIs"
~~~

---

### Task 6: 交卷事务、客观题即时判分和任务入队

**Files:**

- Create: internal/submissions/repository.go
- Create: internal/submissions/service.go
- Create: internal/submissions/service_test.go
- Create: internal/jobs/repository.go
- Create: internal/jobs/repository_test.go
- Modify: internal/httpapi/student.go

**Interfaces:**

- Produces: submissions.Submit(ctx, sessionID, token, answers, reason) SubmitResult
- Produces: submissions.CreateFromSessionTx，供手动交卷与收卷共用
- Produces: jobs.EnqueueTx(submissionID, paperID, runID, generation)

- [ ] **Step 1: 写事务测试**

覆盖：

- 混合卷：submission、session submitted、generation=1、queued job 在同一事务提交。
- 纯客观卷：无 job，grading_status=done、review_status=auto_scored、total_score=objective_score。
- insert job 失败时 submission 和 session 状态全部回滚。
- 手动提交与 closing 竞争只产生一份 submission。
- 同一 employee_id + run_id 重复提交返回 409 DUPLICATE_SUBMISSION；已有 submission_id 可通过带 session_token 的 session status 查询。
- 客观 detail 顺序与快照 questions 顺序一致。
- 交卷始终以 answers 请求体为最终答案，不依赖最后草稿。
- 客户端 auto_submit_reason 只允许 third_blur、blur_timeout_30s 或 null；admin_closed 只能由 finalize 内部写入。

- [ ] **Step 2: 实现短事务交卷**

事务外先完成只读和 CPU 工作：

1. 读取 session + run，校验 session token。
2. 从 run.snapshot_path 加载且校验 hash。
3. 校验完整 answers。
4. Go 计算全部客观题 detail 和 objective_score。

随后开启短事务：

1. SELECT session FOR UPDATE，再次校验 token hash、status 和 deadline + grace_period_seconds。
2. SELECT run FOR UPDATE 并确认 status=open，且 snapshot_path/hash 与事务外读取值一致。
3. INSERT submission。
4. UPDATE session active→submitted，并断言 row count=1。
5. 有主观题时按 worker.max_attempts INSERT generation=1 queued job；无主观题时直接 done。
6. COMMIT 后返回。

不得吞掉任何步骤的错误。

- [ ] **Step 3: 固定初始 detail**

混合卷初始 grading_detail_json 只保存客观题 detail；Worker 完成时按快照顺序重建完整数组并覆盖，不使用 JSON append，从而保证重试幂等。

- [ ] **Step 4: 状态接口**

GET /api/submission/{id}/status 返回 grading_status；兼容 status 映射：

- pending/grading → grading
- done → review_status
- failed → need_review

该公开端点只返回 submission_id、status、grading_status、review_status，不返回分数、错误文本、身份或答案。

- [ ] **Step 5: 验证**

~~~bash
go test ./internal/submissions ./internal/jobs ./internal/httpapi
~~~

Expected: PASS。

- [ ] **Step 6: Commit**

~~~bash
git add internal/submissions internal/jobs internal/httpapi/student.go
git commit -m "feat: add transactional submit and grading enqueue"
~~~

---

### Task 7: Python Worker 的 claim、续租、重试和代次 fencing

**Files:**

- Create: scoring_worker/__init__.py
- Create: scoring_worker/main.py
- Create: scoring_worker/claim.py
- Create: scoring_worker/repository.py
- Create: scoring_worker/grader_bridge.py
- Create: scoring_worker/pyproject.toml
- Create: scoring_worker/uv.lock
- Create: tests/worker/test_claim.py
- Create: tests/worker/test_worker.py
- Create: tests/worker/test_grader_bridge.py
- Create: tests/worker/test_runtime_boundary.py

**Interfaces:**

- Produces: claim_job(conn, worker_id, lease_seconds) -> Job | None
- Produces: renew_lease(conn, job_id, worker_id, lease_token, lease_seconds) -> bool
- Produces: complete_job(conn, job, result) -> "done" | "superseded"
- Produces: fail_job(conn, job, error) -> "queued" | "dead" | "lost"
- Produces: grade_subjective(snapshot, answers, old_details) -> WorkerResult

- [ ] **Step 1: 建立独立 Worker 依赖项目**

scoring_worker/pyproject.toml 固定为独立、非安装型 uv 项目；不得复用根目录依赖：

~~~toml
[project]
name = "exam-system-scoring-worker"
version = "0.1.0"
requires-python = ">=3.12,<3.13"
dependencies = [
    "psycopg[binary]>=3.2,<4",
    "pyyaml>=6.0,<7",
    "python-dotenv>=1.0,<2",
    "subjective-scoring[text,sql,code,remote]",
]

[project.optional-dependencies]
local = [
    "sentence-transformers>=3.0,<4",
    "torch>=2.2,<3",
]

[dependency-groups]
dev = [
    "pytest>=8,<9",
]

[tool.uv]
package = false

[tool.uv.sources]
subjective-scoring = { git = "https://github.com/yhwyxy/subjective-scoring", tag = "v0.1.7" }
~~~

运行 `uv lock --project scoring_worker` 生成 scoring_worker/uv.lock。锁文件必须解析到 subjective-scoring 0.1.7 的固定 Git commit，并包含 Windows CPython 3.12 可用的 psycopg binary、text、SQL、code、remote extras 以及可选 local extra；不得手工复制或裁剪根目录 uv.lock。

- [ ] **Step 2: 写队列并发测试**

至少验证：

- 两个 Worker 不能 claim 同一 job。
- lease 过期后可被另一个 Worker 回收。
- 原 Worker 在 lease_token 失效后 complete 返回 superseded/lost，submission 不变。
- 心跳延长 lease_until。
- 第 1 到 4 次失败重新 queued，available_at 使用 5、10、20、40 秒退避；第 5 次 dead。
- regrade 产生 generation=2 后，generation=1 完成不能覆盖。
- 同一 complete 调用两次，第二次不重复 detail、不重复分数。
- 扫描 scoring_worker 全部 Python AST；出现 import backend 或 from backend 时测试失败。

- [ ] **Step 3: 实现原子 claim**

使用单条 CTE + UPDATE：

~~~sql
WITH candidate AS (
    SELECT id
    FROM grading_jobs
    WHERE (
        status = 'queued' AND available_at <= now()
    ) OR (
        status = 'leased' AND lease_until < now()
    )
    ORDER BY available_at, id
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
UPDATE grading_jobs j
SET status = 'leased',
    attempts = j.attempts + 1,
    lease_owner = %(worker_id)s,
    lease_token = %(lease_token)s,
    lease_until = now() + make_interval(secs => %(lease_seconds)s),
    updated_at = now()
FROM candidate
WHERE j.id = candidate.id
RETURNING j.*;
~~~

claim 后只在 submission.grading_generation=job.generation 时将 grading_status 改为 grading。

- [ ] **Step 4: 实现严格主观题桥接**

grader_bridge：

1. 只选择 short_answer、essay、composite。
2. 直接从第三方包导入 SubjectiveScoringService、ScoringRequest、ScoringResult、ScoringMode、ReviewLevel 和 CohereRerankerPairScorer；不得通过 backend.grader 或 backend.review_service 间接调用。
3. 在 scoring_worker/grader_bridge.py 内实现题库→ScoringRequest 的最小适配：
   - 显式 scoring_points 优先；仅有 scoring_rubric 时，用独立 parse_scoring_rubric 解析“说明文字 + 分值”并限制总分不超过 max_score。
   - scoring_mode 缺省为 text；code_language=sql 映射 sql，其余 code_language/allowed_languages 映射 code。
   - 代码题根据学生提交的 language 选择 answers_by_language 和 scoring_points_by_language，并校验语言白名单。
   - calculation 原样放入 scoring_config.calculation。
   - composite 逐个 subquestions 构造 ScoringRequest，再汇总父题。
4. 直接调用 service.score(request)。调用、模型、远程请求或结果校验异常必须向外抛出触发 job retry；不得把异常静默记 0 分并标记成功。
5. 将 ScoringResult 映射为现有 grading detail：question_id、type、question、student_answer、reference_answer、machine_score、final_score、max_score、grading_method、confidence、reason、review_status、need_manual_review、matched_points、missed_points、warnings；映射结果用 Task 0 fixture 与旧 Python 输出做 parity 测试。
6. Worker 启动时直接构造第三方服务：
   - RERANK_USE_REMOTE=true 时要求 RERANK_API_URL、RERANK_API_KEY、RERANK_MODEL 均非空，构造 CohereRerankerPairScorer，以 allow_model_load=false 注入 text_pair_scorer/code_pair_scorer。
   - RERANK_USE_REMOTE=false 时要求 local extra 已安装，并把相对 config.model.reranker 按进程工作目录解析成存在、可读的本地路径，再构造 allow_model_load=true 的 SubjectiveScoringService；若依赖或模型缺失必须失败，禁止 lexical fallback。
   - 环境变量未显式设置为 true/false 时，生产 --check 失败，避免因空值意外切到本地模式。
   - import、环境变量或服务构造失败时进程退出非零，不 claim job。
7. 从 exam_runs.snapshot_path 加载快照并校验 snapshot_hash；不得按 run_id 调旧 question_loader。
8. 将客观 detail 与新主观 detail 按快照问题顺序重建完整数组。
9. 在 grader_bridge.py 内实现 preserve_manual_reviews：machine_score 使用本次结果，已人工复核的 final_score、reviewed_by、review_note、manually_reviewed、reviewer_note 保留；不得 import backend/review_service.py。
10. review_status 只按主观题汇总：任一 low_confidence 优先，其次 need_review；全部 reviewed 为 reviewed；其余全部高置信为 high_confidence。客观题 auto_scored 不得把混合卷误判为 pending。

- [ ] **Step 5: 实现心跳**

评分期间启动单独数据库连接的心跳线程，每 heartbeat_seconds 执行 renew_lease。连续一次 renew 返回 false 即设置 lost 标记；评分结束后不得写回。

main.py 按 worker.concurrency 启动固定数量执行槽；每个槽、claim、心跳都使用自己的 psycopg 连接，不跨线程共享 connection。关闭信号停止新 claim，等待在途评分最多 30 秒后退出。`--check` 只验证配置分支、直接 import、依赖和服务构造，不 claim job、不写数据库、不发远程请求；远程连通性在 Task 12 preflight 验证。

- [ ] **Step 6: 实现 fenced complete**

同一事务内：

1. 确认 job 仍是 leased，owner、lease_token 匹配且 lease_until > now()。
2. UPDATE submissions，条件含 id 和 grading_generation。
3. 更新完整 grading_detail_json、subjective_score_machine、subjective_score_final、total_score=objective_score+subjective_final、review_status、grading_status=done、graded_at。
4. UPDATE job status=done 并清空租约。
5. 若第 2 步 0 行，将 job 标记 superseded，不改 submission。

- [ ] **Step 7: 实现失败**

错误摘要限制 2000 字符，不写答案或 token。attempts < max_attempts 时 queued，并清空 owner/token/until、设置 available_at=now()+backoff；否则 dead 并清空租约，且仅在 generation 仍匹配时把 submission 标记 failed + need_review。

- [ ] **Step 8: 验证**

~~~bash
uv lock --project scoring_worker --check
env PYTHONPATH=. uv run --project scoring_worker --frozen --group dev python -m pytest tests/worker -q
RERANK_USE_REMOTE=true RERANK_API_URL=https://example.invalid RERANK_API_KEY=test-only RERANK_MODEL=test-model env PYTHONPATH=. uv run --project scoring_worker --frozen python -m scoring_worker.main --check
! rg -n '(^|\\s)(from|import)\\s+backend' scoring_worker -g '*.py'
~~~

Expected: 独立锁文件未漂移；测试 PASS；--check 在选定的远程或本地模式验证配置后退出 0，不进入轮询；rg 无匹配。测试必须在 scoring_worker 自己的环境中运行，不能因根项目已安装 FastAPI、sentence-transformers 或 torch 而误通过；另用缺失 local extra/模型的测试证明本地模式会失败而不是降级。

- [ ] **Step 9: Commit**

~~~bash
git add scoring_worker tests/worker
git commit -m "feat: add fenced retryable subjective scoring worker"
~~~

---

### Task 8: closing 收卷循环和进程恢复

**Files:**

- Create: internal/finalize/service.go
- Create: internal/finalize/service_test.go
- Modify: internal/runs/service.go
- Modify: cmd/exam-server/main.go

**Interfaces:**

- Produces: finalize.ScanDue(ctx) (int, error)
- Produces: finalize.FinalizeRun(ctx, runID) ([]int64, error)

- [ ] **Step 1: 写故障测试**

覆盖：

- 最新草稿自动提交，auto_submit_reason=admin_closed。
- 空草稿也生成提交。
- 收卷和手动提交竞争只保留一条。
- 重复 FinalizeRun 不重复提交/job。
- 任意一个 session 答案校验或 DB 写入失败时整个事务回滚，run 仍 closing。
- 进程重启后扫描已到期 closing run。
- 两个 API 进程扫描同一 run 时只有一个持锁执行。

- [ ] **Step 2: 实现领取与收卷**

ScanDue 用 FOR UPDATE SKIP LOCKED 领取到期 run。FinalizeRun 在一个事务内：

1. SELECT run FOR UPDATE，确认 closing 且 finalize_at <= now。
2. 加载并校验一次快照。
3. SELECT active sessions FOR UPDATE。
4. 对每个 session 调 Task 6 的 CreateFromSessionTx。
5. 全部成功后 UPDATE run closing→closed。
6. COMMIT。

任何错误直接返回并回滚；下一秒扫描重试。不得对单个 session 捕获错误后继续关闭轮次。

- [ ] **Step 3: 生命周期**

serve 启动时先 ScanDue 一次，再每秒执行；shutdown 取消 context 并等待当前事务结束，最长 15 秒。

- [ ] **Step 4: 验证**

~~~bash
go test ./internal/finalize ./internal/runs
~~~

Expected: PASS。

- [ ] **Step 5: Commit**

~~~bash
git add internal/finalize internal/runs cmd/exam-server/main.go
git commit -m "feat: add atomic recoverable exam finalization"
~~~

---

### Task 9: 管理端认证、试卷/轮次 API 和文件事务

**Files:**

- Create: internal/auth/store.go
- Create: internal/auth/middleware.go
- Create: internal/auth/store_test.go
- Create: internal/httpapi/admin.go
- Create: internal/httpapi/admin_contract_test.go
- Modify: internal/papers/store.go
- Modify: internal/runs/service.go
- Modify: internal/httpapi/router.go

**Interfaces:**

- Produces: auth.Login(password) token
- Produces: auth.RequireAdmin middleware
- Produces: 固定契约中的 papers、open/close、batch、exams、exam-link、preview、reload 路由

- [ ] **Step 1: 认证测试**

验证：

- /api/admin/login 在中间件外。
- enable_auth=false 返回 token=auth_disabled，其余路由跳过验证。
- 明文密码和 SHA-256 配置均可登录。
- 错误密码 401 WRONG_PASSWORD。
- 未配置密码 503 ADMIN_PASSWORD_NOT_CONFIGURED。
- token 24 小时过期，比较使用 constant time。
- 日志不包含密码或 token。

- [ ] **Step 2: 路由分组**

chi 注册顺序固定为：

1. POST /api/admin/login
2. Route /api/admin，内部 Use RequireAdmin，再注册其余路由

从而避免首次登录被 Bearer 拦截。

- [ ] **Step 3: 实现 paper 文件写入**

管理端保存保持原 JSON 字段；写路径使用 per-slug mutex + 临时文件 + fsync + rename，并同步 index.json。open/closing 时禁止编辑、问题 CRUD、重排和删除。任一历史 run 或 submission 存在时禁止硬删除 paper，返回 PAPER_HAS_HISTORY。

- [ ] **Step 4: 实现发布链接**

明文 public token 只在发布时生成，数据库只存 hash。发布顺序固定为：生成并原子写 snapshot、生成并原子写 data/exam_runs/{run_id}.token、最后在 PG 事务插入 run；PG 失败时删除两个文件。URL 基址尊重 X-Forwarded-Proto、X-Forwarded-Host 和 Host；本机 localhost 管理访问按现网逻辑替换为 LAN IP。

- [ ] **Step 5: 实现安全 reset-rounds**

保留路由，但增加约束：

- open/closing 时 ACTIVE_RUN_EXISTS。
- 任一 run 有 submission 时 RUN_HAS_SUBMISSIONS；管理员必须先明确删除成绩。
- 任一 job 非 done/dead/superseded 时 GRADING_IN_PROGRESS。
- 只有无提交的 closed run 才可删除 run、session、snapshot、token，使下一次 round_no 回到 1。

此约束防止保留成绩却删除重评所需快照。

- [ ] **Step 6: 管理端契约**

逐一运行 Task 0 admin 测试，尤其验证：

- batch 路由不会被 {slug} 捕获。
- /questions/reorder 在 /questions/{question_id} 之前注册，不会把 reorder 当 question_id。
- batch 返回 requested、updated、skipped、papers、errors。
- preview 返回完整答案但必须有 admin auth。
- GET /api/admin/exams 不依赖 index.json.status。
- exam-link 无 token 时返回 url=null 和明确 message。
- reload-config 对业务阈值即时生效；若 server.allow_origins、host 或 port 变化，响应明确提示重启。

- [ ] **Step 7: 验证**

~~~bash
go test ./internal/auth ./internal/papers ./internal/runs ./internal/httpapi
~~~

Expected: PASS。

- [ ] **Step 8: Commit**

~~~bash
git add internal/auth internal/httpapi internal/papers internal/runs
git commit -m "feat: align admin authentication papers and run APIs"
~~~

---

### Task 10: 提交列表、复核、重评、删除、统计和 XLSX

**Files:**

- Create: internal/review/service.go
- Create: internal/review/service_test.go
- Create: internal/export/xlsx.go
- Create: internal/export/xlsx_test.go
- Modify: internal/submissions/repository.go
- Modify: internal/httpapi/admin.go

**Interfaces:**

- Produces: review.Apply、review.Regrade
- Produces: export.BuildXLSX
- Produces: submissions list/detail/stats/delete

- [ ] **Step 1: 写列表与导出契约测试**

验证：

- 列表支持 keyword、review_status、paper_id、run_id、sort_by、order、limit、offset。
- 列表不返回 answers_json、grading_detail_json。
- detail 返回 answers、grading_detail、review_logs，并保留其他兼容字段。
- stats 字段为 submitted_count、avg_score、max_score、min_score、pending_review、low_confidence_count、paper_id、run_id。
- XLSX Content-Type 和 Content-Disposition 正确。
- export 支持 paper_id 和 run_id；未传时最多导出 10000 条并按 submitted_at DESC。
- 表头包含 ID、姓名、工号、专业编码、专业名称、轮次、部门、客观题分、主观题机器分、主观题最终分、总分、复核状态、提交时间和动态题目列。
- composite 导出包含子题 ID、语言和分数说明。

- [ ] **Step 2: 实现人工复核事务**

规则：

- grading_status 为 pending 或 grading 时返回 GRADING_IN_PROGRESS；failed 允许人工兜底。
- failed 且缺少主观题 detail 时，先从所属 run 快照为每道缺失主观题构造 0 分、need_review、grading_method=manual_fallback 的 detail，再应用人工分。
- 只允许主观题或 composite 子题。
- new_score 必须在 0 到 max_score。
- 更新 final_score、subjective_score_final、total_score、review_status 和 reviewer_note。
- 同事务插入 review_logs；任一步失败全部回滚。

- [ ] **Step 3: 实现重评代次**

事务内锁定 submission：

- 纯客观卷由 Go 重新加载所属 run 快照并同步重算，generation 不变，不创建 job。
- 含主观题时先由 Go 按所属 run 快照重算客观 detail/objective_score，再执行 grading_generation + 1、grading_status=pending、grading_error=null，插入新 generation queued job。
- 旧 queued/leased job 标记 superseded。
- Worker 完成时保留已人工复核的 final_score。

HTTP 立即返回：

~~~json
{
  "success": true,
  "submission_id": 123,
  "grading_status": "pending",
  "generation": 2
}
~~~

- [ ] **Step 4: 实现删除**

DELETE /api/admin/submissions 接受 {"ids":[1,2]}。单事务删除；review_logs 和 jobs 由 FK cascade，返回 {"success":true,"deleted":2}。

- [ ] **Step 5: 验证**

~~~bash
go test ./internal/review ./internal/export ./internal/submissions ./internal/httpapi
~~~

Expected: PASS。

- [ ] **Step 6: Commit**

~~~bash
git add internal/review internal/export internal/submissions internal/httpapi/admin.go
git commit -m "feat: add compatible review regrade export and admin results"
~~~

---

### Task 11: 前端最小适配与完整契约回归

**Files:**

- Modify: frontend/js/exam.js
- Modify: frontend/js/admin.js
- Modify: frontend/js/detail.js
- Modify: tests/test_frontend_static.py

**Interfaces:**

- Consumes: draft saved/throttled、grading_status。
- 不改变页面路由、DOM 主结构或 UI 框架。

- [ ] **Step 1: 先写静态失败测试**

测试必须精确查找现有 DRAFT_LOOP_MS，不得引用不存在的 DRAFT_AUTOSAVE_INTERVAL_MS、showMessage 或未定义 resp。

- [ ] **Step 2: 草稿间隔与 throttle**

将：

~~~javascript
const DRAFT_LOOP_MS = 2000;
~~~

改为：

~~~javascript
const DRAFT_LOOP_MS = 5000;
~~~

处理成功响应时先判断：

~~~javascript
if (data.saved === false) {
  state.draftRevision = data.draft_revision ?? state.draftRevision;
  state.dirty = true;
  setDraftStatus('等待保存');
  return false;
}
~~~

不得在未落库时清除 dirty。

收到 STALE_DRAFT_REVISION 时读取 detail.current_revision，更新 state.draftRevision，保持 state.dirty=true，并在下一轮重发当前完整 answers；不得像旧代码一样把 stale 当成“服务器已保存本地答案”后清除 dirty。

- [ ] **Step 3: 修正页面离开保存**

sendBeacon 固定发送 POST，而后端契约是 PUT；替换为 fetch 的 PUT + keepalive:true。只有序列化请求体不超过 60000 字节时才发 keepalive 请求，超过时保留 dirty 并依赖正常保存/最终 submit 的完整 answers。离开页面的请求没有确认响应时，不提前增加 draftRevision、不把 dirty 设为 false。

同时把 saveDraftNow 签名改为 saveDraftNow({beacon=false, allowLocked=false}={})；仅在 state.locked && !allowLocked 时提前返回。handleClosingStatus 锁定控件后必须调用 saveDraftNow({allowLocked:true})，确保 5 秒 closing 窗口真的上传最后答案。

- [ ] **Step 4: 状态展示**

管理端列表取 effective status：

- pending/grading → grading
- failed → need_review
- done → review_status

detail 页 grading_status 为 pending/grading 时禁用复核按钮；failed 显示 grading_error 摘要并允许人工兜底复核或重新评分。

- [ ] **Step 5: 双后端契约回归**

先启动 Python 基线，再启动 Go 于 18080，分别执行：

~~~bash
env PYTHONPATH=. uv run pytest tests/contract tests/test_frontend_static.py -q
EXAM_CONTRACT_BASE_URL=http://127.0.0.1:18080 EXAM_CONTRACT_EXPECT_GO=1 env PYTHONPATH=. uv run pytest tests/contract -q
~~~

Expected: 两组全部 PASS。

- [ ] **Step 6: Commit**

~~~bash
git add frontend/js tests/test_frontend_static.py
git commit -m "feat: align frontend draft throttling and grading states"
~~~

---

### Task 12: Windows staging 安装、日志、健康检查与文档

**Files:**

- Create: scripts/windows/install.ps1
- Create: scripts/windows/package.ps1
- Create: scripts/windows/uninstall.ps1
- Create: scripts/windows/start.ps1
- Create: scripts/windows/stop.ps1
- Create: config.production.example.yaml
- Create: docs/deployment-go-pg.md
- Create: docs/rollback-go-pg.md
- Modify: DEPLOY.md
- Modify: README.md
- Modify: cmd/exam-server/main.go
- Modify: scoring_worker/main.py

**Interfaces:**

- Produces: Scheduled Task ExamSystemAPI
- Produces: Scheduled Task ExamSystemScoringWorker
- Produces: 文件日志 logs/exam-server.log、logs/scoring-worker.log
- Produces: dist/windows/exam-system/，只含程序与安装材料，不含旧 backend/、根项目 Python 依赖、SQLite 或任何运行数据。
- Produces: dist/windows/exam-system-rollback-tools/，与生产包物理分离，保存旧 Python 代码和双向迁移工具，但不含任何 data/ 或数据库备份。
- Produces: scoring_worker.main --preflight，只做依赖/模型或远程评分健康检查，不 claim job、不写成绩。

- [ ] **Step 1: 日志**

Go 与 Python 都写按大小轮转的 UTF-8 文件，单文件 20MB、保留 10 个。日志包含 request_id、run_id、submission_id、job_id、generation 和耗时；不得包含明文 token、密码、完整 answers 或草稿。

- [ ] **Step 2: 生成生产发布包**

package.ps1 创建全新 dist/windows/exam-system 目录，只复制：

- exam-server.exe
- config.production.example.yaml（password、database URL、API key 均为空的脱敏模板）
- frontend/
- scoring_worker/*.py、scoring_worker/pyproject.toml、scoring_worker/uv.lock
- scripts/windows/install.ps1、uninstall.ps1、start.ps1、stop.ps1
- docs/deployment-go-pg.md、docs/rollback-go-pg.md

config.production.example.yaml 固定使用以下脱敏结构，不从工作区 config.yaml 复制值：

~~~yaml
server:
  host: "0.0.0.0"
  port: 8000
  allow_origins: ["*"]

exam:
  title: "企业内部考试"
  duration_minutes: 60
  auto_submit: true
  grace_period_seconds: 30
  allow_duplicate_submit: false
  duplicate_key: "employee_id"
  enable_global_time_window: false
  start_time: null
  end_time: null

scoring:
  multiple_choice_partial: true
  wrong_choice_penalty: false
  score_precision: 1

review:
  high_confidence_threshold: 0.75
  need_review_threshold: 0.5
  low_confidence_threshold: 0.35

grading:
  sync_grading: false

model:
  reranker: "models/reranker"

admin:
  enable_auth: true
  password: ""

export:
  format: "xlsx"

database:
  url: ""
  max_conns: 32
  min_conns: 4
  connect_timeout_seconds: 5
  statement_timeout_seconds: 10

draft:
  min_server_interval_ms: 2000
  max_json_bytes: 512000

worker:
  poll_interval_ms: 200
  concurrency: 2
  lease_seconds: 300
  heartbeat_seconds: 60
  max_attempts: 5

logging:
  directory: "logs"
~~~

不得复制 config.yaml、根目录 pyproject.toml/uv.lock、backend/、main.py、data/、测试、scoring_worker/.venv、__pycache__、缓存、旧日志或回滚数据。脚本生成 manifest.sha256，并在发现 scoring_worker import backend.*、发布包出现 config.yaml、backend、exam.db、根依赖清单或任意 data/ 路径时失败。

DataRoot/papers 与 DataRoot/exam_runs 是独立于 Go InstallRoot 的生产持久数据，通过 InstallRoot/data junction 访问，必须由安装/备份流程原地保留；旧 data/exam.db 与旧 Python 服务属于独立回滚资产。三者都不混入新程序发布包，但不得把 exam_runs 描述为“仅回滚数据”。

同一 package.ps1 另创建 dist/windows/exam-system-rollback-tools，只复制 backend/*.py、main.py、README.md、根目录 pyproject.toml/uv.lock、scripts/migrate_sqlite_to_postgres.py、scripts/export_postgres_to_sqlite.py、scripts/verify_migration.py、frontend/ 和 docs/rollback-go-pg.md。该目录单独生成 manifest.sha256，不得包含 data/、exam.db、WAL/SHM、日志、缓存或生产密钥，也不得由 install.ps1 复制进新系统 InstallRoot。真正的 SQLite 数据、config.yaml 和轮次文件来自 Task 14 的带时间戳预切流备份。

- [ ] **Step 3: 安装脚本**

install.ps1 必须：

1. 要求管理员权限。
2. 接受显式 PackageRoot、InstallRoot、DataRoot，以及本地模式可选 ModelRoot。InstallRoot 必须是独立的新 Go 程序目录，不能等于旧 Python 根目录、DataRoot、rollback-tools 或任一备份目录；解析真实路径后发现重叠即失败。
3. InstallRoot 若已存在，只允许包含上一版 Go 发布物、config.yaml、logs、scoring_worker/.venv 和既有 junction；发现 backend/、main.py、根目录 pyproject.toml/uv.lock、exam.db 或真实 data 目录即失败。安装器不得替用户删除或搬迁这些文件。
4. 只按生产包白名单复制程序文件到 InstallRoot，不清空目录，不使用 robocopy /MIR，不删除未知文件或目录。
5. InstallRoot/config.yaml 已存在时必须逐字节保留；config.production.example.yaml 可随程序版本安全覆盖。首次迁移必须通过 ConfigSource 显式复制旧配置，或显式传入 InitializeFromTemplate 才能由脱敏模板创建 config.yaml，不能静默复制模板或覆盖旧配置。ConfigSource 内容不得写进 manifest 或日志。
6. 在 DataRoot/papers 和 DataRoot/exam_runs 上生成安装前 SHA-256 清单；两目录必须存在，exam_runs 不存在时不得静默创建空目录掩盖迁移遗漏。
7. 用 `New-Item -ItemType Junction` 创建或验证 InstallRoot/data -> DataRoot。若 InstallRoot/data 已是指向同一规范路径的 junction 则复用；若是普通目录、其他 target 或解析后逃逸则失败。安装器不得复制、移动或修改 DataRoot 内容。
8. 本地模式若 config.model.reranker 使用相对 models/ 路径，则同样创建或验证 InstallRoot/models -> ModelRoot，并记录模型目录哈希；绝对模型路径则直接验证。远程模式不要求 ModelRoot。
9. 复制和 junction 建立后重新生成 DataRoot/papers、DataRoot/exam_runs 及可选 ModelRoot 清单；任一既有文件缺失或哈希变化立即失败并停止启动。
10. 验证 PostgreSQL 服务运行；验证 EXAM_DATABASE_URL 机器环境变量存在但不打印值；admin.enable_auth=true 时要求 EXAM_ADMIN_PASSWORD 机器环境变量存在且不是示例值；验证 uv 可执行且能找到 Python 3.12。要求 RERANK_USE_REMOTE 明确为 true 或 false：true 时检查 RERANK_API_URL/RERANK_API_KEY/RERANK_MODEL 均存在但不打印密钥；false 时检查目标模型目录可读。
11. 远程模式执行 `uv sync --project "$InstallRoot/scoring_worker" --frozen --no-dev --python 3.12`；本地模式执行同一命令并追加 `--extra local`。两者都只按 scoring_worker/uv.lock 创建或更新 scoring_worker/.venv；不得读取根目录依赖、安装或复制旧 examSystem backend 包。本地模式安装后必须验证 sentence_transformers 与 torch 可导入。
12. 验证 exam-server.exe、config.yaml、frontend、data junction、data/papers/index.json、data/exam_runs、scoring_worker/*.py、scoring_worker/pyproject.toml、scoring_worker/uv.lock、scoring_worker/.venv/Scripts/python.exe；本地模式还验证 models junction/绝对路径。
13. 在 WorkingDirectory=InstallRoot 下执行 scoring_worker/.venv/Scripts/python.exe -c "import subjective_scoring, scoring_worker"；另用 AST 边界检查确认 scoring_worker 无 backend.* import。不要以“import backend 失败”作为唯一判据，因为系统环境可能碰巧安装同名包。
14. 执行 exam-server.exe migrate、exam-server.exe preflight 和 scoring_worker/.venv/Scripts/python.exe -m scoring_worker.main --preflight；Worker preflight 使用固定无敏感 fixture，不 claim job、不写数据库。
15. 用 New-ScheduledTaskAction 设置明确 WorkingDirectory=InstallRoot；API action 运行 exam-server.exe serve；Worker action 运行 scoring_worker/.venv/Scripts/python.exe -m scoring_worker.main。
16. principal 使用 SYSTEM、ServiceAccount、Highest；InstallRoot、DataRoot、logs 和可选 ModelRoot 必须授予该账号所需权限，其中模型目录只读。
17. 使用 AtStartup trigger、RestartCount=999、RestartInterval=1 分钟、ExecutionTimeLimit=0。创建任务后启动，并轮询 /api/health 最多 30 秒。

不得使用 New-Service 包装上述控制台程序。

- [ ] **Step 4: 卸载与控制**

stop.ps1 先停 Worker 再停 API；start.ps1 先启动 API 并等待健康，再启动 Worker；uninstall.ps1 只删除两个 Scheduled Tasks。默认保留 InstallRoot、config.yaml、logs、scoring_worker/.venv、data/models junction、DataRoot、ModelRoot、数据库和备份；即使提供“移除程序文件”选项，也只能删除已核对 manifest 的 Go 发布物和 junction 本身，绝不能跟随 junction 删除 target。

- [ ] **Step 5: preflight**

install.ps1 顺序执行 Go preflight 与 Worker preflight，检查：

- PG 连接和 migration 版本。
- admin.enable_auth=true 时 EXAM_ADMIN_PASSWORD 非空，且不等于 123456、password、changeme 等模板值；禁止仅依赖发布包中的默认口令。
- data/papers/index.json 与全部 paper 可读。
- data/exam_runs 可读；所有非 legacy run 的快照存在且 hash 正确，现有 .token 文件可读且不会被安装脚本改写。
- 同 paper 不存在多个活动 run。
- worker 配置 heartbeat_seconds < lease_seconds / 2。
- scoring_worker 不依赖 backend.*，subjective_scoring 可直接导入。scoring_worker.main --preflight 在远程模式用实际配置完成一次不含真实答案的健康评分请求；本地模式确认目标模型已实际加载并完成固定 fixture 评分。任一模式不得报告 lexical fallback 后仍通过。
- 监听端口可用。

- [ ] **Step 6: 部署和回滚文档**

deployment-go-pg.md 写出安装、升级、日志、备份、启动顺序和常见故障。

升级章节必须明确：程序包不携带 data/ 或本地模型；DataRoot/ModelRoot 位于独立持久目录，通过 InstallRoot 下 junction 挂载。升级前记录 DataRoot/papers、DataRoot/exam_runs 以及本地模式 ModelRoot 的哈希，再运行 install.ps1；安装器只覆盖白名单程序文件并保留 config.yaml/logs/venv/junction，升级后再次比对 target 哈希。禁止删除整个 InstallRoot 后重新解压，更禁止删除或递归复制 junction target。

rollback-go-pg.md 明确两类回滚：

- 尚未接收新提交：停止新进程，把 legacy-sqlite/ 中经 SHA-256 校验的预切流 SQLite 恢复到旧服务隔离目录，启动预切流旧 Python 发布包。
- 已接收新提交：停考并停止 Worker/API，从独立 exam-system-rollback-tools 运行 export_postgres_to_sqlite.py 生成新 SQLite，运行 verify_migration.py，原子替换回滚副本中的 data/exam.db，再启动旧 Python。

文档必须同时说明：rollback-tools 只含代码和依赖锁，预切流备份才含实际 data/config；恢复时在隔离目录组合二者，校验 SHA-256 后再切换，禁止把 backend/ 拷回新生产 InstallRoot。

文档不得引用未创建的脚本。

- [ ] **Step 7: 验证**

~~~bash
GOOS=windows GOARCH=amd64 go build -o /tmp/exam-server.exe ./cmd/exam-server
uv lock --project scoring_worker --check
RERANK_USE_REMOTE=true RERANK_API_URL=https://example.invalid RERANK_API_KEY=test-only RERANK_MODEL=test-model env PYTHONPATH=. uv run --project scoring_worker --frozen python -m scoring_worker.main --check
powershell -ExecutionPolicy Bypass -File scripts/windows/package.ps1
~~~

Expected: dist/windows/exam-system 中不存在 backend、exam.db、data/、config.yaml 和根目录 pyproject.toml/uv.lock，只含脱敏 config.production.example.yaml 与 Worker 独立依赖清单；dist/windows/exam-system-rollback-tools 含旧代码和三个迁移工具但不含任何 data/。在 Windows staging 记录 package.ps1、install.ps1、stop.ps1、start.ps1、uninstall.ps1 均成功，并保存安装前后 data/papers、data/exam_runs 的相同 SHA-256 清单。

- [ ] **Step 8: Commit**

~~~bash
git add scripts/windows config.production.example.yaml docs/deployment-go-pg.md docs/rollback-go-pg.md DEPLOY.md README.md cmd/exam-server/main.go scoring_worker/main.py
git commit -m "docs: add Windows operations deployment and rollback"
~~~

---

### Task 13: 真实 k6 容量测试与背压验收

**Files:**

- Create: scripts/loadtest/prepare.py
- Create: loadtest/start_peak.js
- Create: loadtest/draft_steady.js
- Create: loadtest/submit_peak.js
- Create: loadtest/README.md
- Create: scripts/loadtest/summarize.py
- Modify: .gitignore

**Interfaces:**

- Produces: loadtest/generated/start-users.json、draft-sessions.json、submit-sessions.json，全部加入 .gitignore。
- Produces: loadtest/results/summary.json，由脚本从 k6 JSON 生成。

.gitignore 固定忽略 loadtest/generated/ 和 loadtest/results/*，再用否定规则只允许提交 loadtest/results/summary.json；原始 k6 JSON 和任何 token 不进入 Git。

- [ ] **Step 1: 准备隔离数据**

prepare.py 使用三个专用 paper：loadtest-start、loadtest-draft、loadtest-submit，各自发布独立 run，并生成 500 个唯一 employee_id：

- start-users.json 只保存 paper_id、run_token 和员工资料，不预创建 session。
- draft-sessions.json 与 submit-sessions.json 预先调用 start，保存各自 session_id/session_token。

不得复用生产试卷、生产 run 或生产数据库。

- [ ] **Step 2: 开考峰值**

start_peak.js 使用 constant-arrival-rate：

~~~javascript
executor: 'constant-arrival-rate',
rate: 500,
timeUnit: '1m',
duration: '1m',
preAllocatedVUs: 100,
maxVUs: 500
~~~

每次迭代只 start 一个唯一员工。阈值：

~~~javascript
checks: ['rate>0.99'],
http_req_failed: ['rate<0.01'],
http_req_duration: ['p(95)<500', 'p(99)<1000']
~~~

- [ ] **Step 3: 草稿稳态**

draft_steady.js 使用 500 constant-vus、持续 30 分钟；每个 VU 每 5 秒仅在答案变化时 PUT 草稿，revision 单调递增。阈值：

- checks > 99%
- http_req_failed < 1%
- p95 < 500ms
- p99 < 1000ms

throttled=true 计为可接受响应，但最终每个 session 至少一次 saved=true。

- [ ] **Step 4: 交卷峰值**

submit_peak.js 使用 constant-arrival-rate，rate=500、timeUnit=1m、duration=1m；每个 session 只提交一次。阈值：

- checks > 99%
- http_req_failed < 1%
- p95 < 750ms
- p99 < 1500ms

脚本必须断言返回 submission_id 和 grading_status；不得用固定 token 或空实现。

- [ ] **Step 5: 队列最终一致性**

压测结束后 summarize.py 查询：

- submissions 数。
- queued/leased/done/dead job 数。
- 超时 lease 数必须为 0。
- 同 employee/run 重复 submission 数必须为 0。
- 15 分钟观察窗后 queued+leased 必须持续下降；是否归零取决于评分模型吞吐，但不得永久租赁。

- [ ] **Step 6: 运行**

~~~bash
k6 run --out json=loadtest/results/start.json loadtest/start_peak.js
k6 run --out json=loadtest/results/draft.json loadtest/draft_steady.js
k6 run --out json=loadtest/results/submit.json loadtest/submit_peak.js
env PYTHONPATH=. uv run python scripts/loadtest/summarize.py loadtest/results
~~~

Expected: k6 退出 0，summary.json 的 passed=true。

- [ ] **Step 7: Commit**

只提交脚本和脱敏汇总，不提交 session token 或原始生产数据。

~~~bash
git add scripts/loadtest loadtest .gitignore
git commit -m "test: add reproducible 500-user capacity validation"
~~~

---

### Task 14: 停考迁移、staging 切流与生产放行

**Files:**

- Modify: docs/deployment-go-pg.md
- Create: docs/cutover-checklist-go-pg.md

**Interfaces:**

- Produces: 一份逐项签字的切流记录和回滚记录。

- [ ] **Step 1: 全量自动化验证**

~~~bash
go test ./...
go vet ./...
env PYTHONPATH=. uv run pytest -q
uv lock --project scoring_worker --check
env PYTHONPATH=. uv run --project scoring_worker --frozen --group dev python -m pytest tests/worker -q
env PYTHONPATH=. uv run --project scoring_worker --frozen python -m scoring_worker.main --check
GOOS=windows GOARCH=amd64 go build -o /tmp/exam-server.exe ./cmd/exam-server
~~~

Expected: 全部退出 0。

- [ ] **Step 2: 预切流备份**

停考并确认无 open/closing run。停止旧 Python 服务后备份：

- data/exam.db 及 WAL/SHM。
- data/papers。
- data/exam_runs。
- config.yaml。
- 当前旧版本发布包。
- dist/windows/exam-system-rollback-tools 及其 manifest.sha256。

备份目录名使用实际 UTC 时间，例如 backup-20260725T120000Z，并生成 SHA-256 清单。

data/papers 与 data/exam_runs 仍是新系统切流后的生产持久数据；此步骤的备份用于防灾，不代表后续安装可以删除或替换原目录。data/exam.db 只供旧 Python 回滚使用，新 Go/Worker 进程不得打开它。

- [ ] **Step 3: 导入和校验**

在空 PG 运行 migrate，再从独立 exam-system-rollback-tools 读取带时间戳备份中的 SQLite 副本，运行 SQLite→PG 导入和 verify_migration。任一校验失败立即停止，不启动 Go 对外端口；不得用仍位于 live data/ 的数据库做唯一迁移源，rollback-tools 也不得复制进新生产 InstallRoot。

- [ ] **Step 4: staging smoke**

Go 暂监听 18080，执行：

- 以独立 Go InstallRoot、现有 DataRoot 和可选 ModelRoot 从 package.ps1 产物运行 install.ps1，再做一次原地 Go 版本升级；比较两次安装前后 DataRoot/papers、DataRoot/exam_runs（含 .token）SHA-256 清单，必须完全一致；若使用本地模式，ModelRoot 清单也必须一致。
- 检查 package.ps1 的程序包产物和 Go InstallRoot 均不存在 backend/、真实 data/、exam.db、根目录 pyproject.toml/uv.lock；InstallRoot/data 必须只是指向现有 DataRoot 的 junction。独立 rollback-tools 含旧代码和迁移工具但不含 data/；Worker 只使用 InstallRoot/scoring_worker/.venv。
- Task 0 全部契约。
- 发布测试卷、开考、保存、手动提交。
- closing 自动提交。
- Worker 完成主观评分。
- 选定 rerank 模式确实生效：远程模式记录脱敏后的健康请求成功证据；本地模式记录实际加载模型路径且没有 fallback。
- admin 列表/detail/review/regrade/export。
- 重启 API 和 Worker 后恢复。

- [ ] **Step 5: 回滚演练**

停止 staging 新进程，在隔离目录组合 rollback-tools 与预切流备份，执行 PG→SQLite 导出，verify 后用旧 Python 在 18081 启动，重复查询历史成绩和一次新测试考试。演练成功后删除测试数据，重新从原备份执行正式导入，避免使用被演练污染的库；不得把演练目录覆盖到新生产 InstallRoot。

- [ ] **Step 6: 正式切流**

导入与校验通过后，把 DataRoot/exam.db 及 WAL/SHM 原子移动到带时间戳回滚资产的 legacy-sqlite/ 子目录，并再次核对原 SHA-256；不得删除。使用与旧 Python 根目录物理分离的 Go InstallRoot，从已验证的 package.ps1 产物运行 install.ps1，显式传入旧配置副本作为 ConfigSource，并挂载现有 DataRoot/可选 ModelRoot；确认 Go InstallRoot 不含 backend 或真实 data 目录，安装前后 DataRoot/papers、DataRoot/exam_runs 哈希一致，config.yaml 来自已核对的 ConfigSource 且未被模板覆盖，本地模式 ModelRoot 未变化。随后启动 Go :8000，再启动 Worker。前 30 分钟每 5 分钟检查：

- /api/health。
- PG 连接数和锁等待。
- queued/leased/dead。
- 5xx、429、STALE_DRAFT_REVISION 比率。
- 日志中 snapshot integrity 和 worker lost lease。

出现数据完整性错误、重复提交、收卷无法完成或 5xx 持续超过 1% 时执行 rollback-go-pg.md。

- [ ] **Step 7: 放行**

连续完成一轮内部考试、所有提交可查询、任务无永久 lease、XLSX 可打开、回滚产物校验通过后，标记 G5 完成。生产进程和新发布目录不得加载/包含 backend/ 或 exam.db；仓库中的 backend/ 与 SQLite 回滚备份清理由后续独立计划处理，本计划不删除。

- [ ] **Step 8: Commit**

~~~bash
git add docs/deployment-go-pg.md docs/cutover-checklist-go-pg.md
git commit -m "docs: record Go PostgreSQL cutover gates"
~~~

---

## 最终自检清单

- [ ] 计划内没有把 run_id 当 paper_id 使用。
- [ ] 计划内没有 sections/content/single 题库模型。
- [ ] GET /api/exam 明确经过深层脱敏。
- [ ] 所有 session API 明确校验 token。
- [ ] 草稿 SQL 明确包含 revision 条件。
- [ ] login 明确在 Bearer middleware 外。
- [ ] Worker 没有引用不存在的 score_answers。
- [ ] Worker 具备 lease token、heartbeat、generation fencing、retry 和 dead。
- [ ] 收卷错误会回滚且保持 closing。
- [ ] migration 具备 schema_migrations、checksum、事务、行数/ID/sequence 校验。
- [ ] review_logs 源/目标字段一致。
- [ ] regrade 使用所属 run 快照并保留人工分。
- [ ] reset-rounds 不会删除仍被成绩引用的快照。
- [ ] XLSX、preview、batch、delete、exam-link、reload 等现有路由均有任务。
- [ ] 前端只引用现存 DRAFT_LOOP_MS 和实际函数。
- [ ] submit_peak.js 是真实请求，500 表示 60 秒总量。
- [ ] Windows 使用 Scheduled Tasks，不使用 New-Service 包装控制台进程。
- [ ] Python 明确为 3.12。
- [ ] Worker 具备独立 scoring_worker/pyproject.toml 与 scoring_worker/uv.lock，并固定 subjective-scoring v0.1.7 所需 text/sql/code/remote extras。
- [ ] Worker 测试、--check 和生产安装均使用 scoring_worker 独立环境，不读取根目录 pyproject.toml/uv.lock。
- [ ] RERANK_USE_REMOTE 必须显式选择；远程配置完整，本地模式安装 local extra、实际加载模型且禁止 lexical fallback。
- [ ] 新程序发布包不含 backend/、exam.db、根目录 Python 依赖清单或任何 data/ 内容。
- [ ] 新程序发布包不含运行 config.yaml，只含脱敏模板；安装显式复制 ConfigSource，管理密码由非示例值环境变量覆盖。
- [ ] 独立 rollback-tools 含旧 Python 与双向迁移工具但不含生产 data/，切流/回滚步骤明确从该隔离目录运行。
- [ ] Go InstallRoot 与旧 Python 根目录物理分离；InstallRoot/data 仅为指向现有 DataRoot 的 junction，安装器拒绝真实 data 目录或 legacy backend 残留。
- [ ] data/papers、data/exam_runs 和 .token 文件被定义为生产持久数据，安装/升级前后哈希一致，卸载 junction 时不跟随删除 target。
- [ ] 回滚文档只引用本计划实际创建的脚本。
- [ ] 最终完成声明以测试、迁移、压测和演练证据为准。

## 执行方式

计划保存于 docs/superpowers/plans/2026-07-23-go-pg-scoring-worker-implementation.md。

执行时从 Task 0 开始，按 G0→G5 顺序推进；推荐每个 Task 使用独立实现上下文，并在进入下一 Task 前审查当前 commit 和测试证据。禁止跳过契约冻结、迁移回滚演练或容量阈值。
