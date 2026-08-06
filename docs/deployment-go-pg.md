# 部署指南: Go + PostgreSQL 后端 (Task 12 Step 6)

适用: exam-server.exe (Go) + PostgreSQL 18 + scoring_worker (Python 评分) 双进程栈.
替代已移除的 Python+SQLite 旧栈, 支撑更高并发 (旧栈及回滚文档见 git 历史).

## 前置 (一次性)

1. PostgreSQL 18 已起, 库 exam_system 已建, 角色 exam_app (DML) / exam_migrator (DDL+DML).
2. migrations/0001_initial.sql 已由 exam_migrator 执行 (无 GRANT, 需手动 `GRANT` 全 DML 给 exam_app).
3. 产 dist 包: `pwsh scripts/windows/package.ps1` → `dist/windows/exam-system/` (dist 内含 packages/ 离线 wheel).
4. 离线安装 worker 依赖: `pwsh scripts/windows/install-worker.ps1` (需要本地语义模型加 `-WithLocalModel`; 见下文"worker 依赖离线安装").
5. 一次性创建 ScheduledTask (命令见下文"启动").

## 目录布局 (不携带 Data/Model)

- 部署根 `D:\exam-system\` (举例; 实际由 dist 解压)
  - `exam-server.exe`           Go API 二进制 (~22MB PE32+)
  - `config.production.yaml`    从 config.production.example.yaml 复制并填值
  - `frontend/`                  Go serve 静态资源
  - `scoring_worker/`            Python 评分 worker 包
  - `packages/`                  Python 离线 wheel (subjective-scoring + 依赖, cp312 win; 已 gitignore, 随 dist 携带)
  - `scripts/windows/`           install-worker.ps1 / start.ps1 / stop.ps1 / package.ps1
  - `docs/`                      本文档等
  - `manifest.sha256`            完整性核对清单
- `scripts/windows/`           install-worker.ps1 / start.ps1 / stop.ps1 / package.ps1
- `scripts/lint_papers.py`     题库静态检查 (L1-L6)
- `scripts/sync_run_snapshots.py` 试卷快照同步
- `docs/`                      本文档等

## 启动前快照同步

启动 worker 前必须做快照同步, 否则 worker 因 **snapshot_hash mismatch** 跳过所有评分:

```powershell
# 用 data/papers/*.json 最新内容覆盖 exam_runs 快照并更新 hash
python scripts/sync_run_snapshots.py
```

同步脚本的输出格式: `<run_id>  <paper_id>  <hash_prefix>`, 每卷一行.
输出 `Done. 0 failures.` 即全部成功.

> **何时必须再跑**: 凡修改 `data/papers/*.json` (修改 scoring_mode / scoring_points / synonyms
> / calculation 配置等) 后, 必须重新执行同步, 否则 worker 读到的快照与 hash 不匹配, 评分失败.

同步脚本限制: 需本地 PostgreSQL 可访问 (`DATABASE_URL` / 默认 DSN), 且 `data/exam_runs/`
目录有对应快照文件 (由 exam-open 时 Go 侧生成).
- DataRoot (独立持久目录, **不在包内**): `D:\exam-data\` (PG 数据 / uploads / backups)
- ModelRoot (独立持久目录, **不在包内**): `D:\exam-models\bge-reranker-v2-m3\`
  - 跨版本持久, 升级不覆盖 (升级只替换部署根的程序文件)

## worker 依赖离线安装 (packages/, 不访问 GitHub/PyPI)

部署机可能无法访问 GitHub, 因此 worker 依赖全部以 wheel 形式随 dist 携带在 `packages/`
(已 gitignore, 由 package.ps1 拷入 dist, 传输时随包一起拷贝):

```powershell
# 在部署根执行; 创建 C:\exam-venv 并完全离线安装 (--no-index 杜绝任何远程访问)
pwsh scripts/windows/install-worker.ps1
# 需要本地语义模型 (sentence-transformers + torch, ~2GB) 时:
pwsh scripts/windows/install-worker.ps1 -WithLocalModel
```

内容与约束:

- `packages/` 含 subjective-scoring **v0.1.11** (wheel + sdist) 及其全部依赖
  (pydantic / ftfy / sqlglot / tree-sitter* / httpx) + psycopg[binary], 均为 Windows cp312 wheel.
- 安装命令等价于 `pip install --no-index --find-links packages <依赖清单>`, 全程离线.
- Python 必须 3.12 (`>=3.12,<3.13`), 不要用 3.13.

升级 scoring 库版本的流程 (在联网开发机):

1. 修改 `../subjective-scoring`, 提交并打 tag (如 `v0.1.12`).
2. 同步 examSystem 引用: `scoring_worker/pyproject.toml` + 根 `pyproject.toml` 的 `[tool.uv.sources]` tag → 新版本, 然后 `uv lock` (两个工程都要).
3. 构建 wheel: `uv build --directory ../subjective-scoring` → 把 wheel/sdist 拷入 `packages/`, 删除旧版文件.
4. 验证离线解析闭环 (需 Python 3.12 兼容平台参数):
   ```bash
   pip install --dry-run --no-index --find-links packages --platform win_amd64 \
       --python-version 3.12 --only-binary=:all: --target /tmp/v \
       "psycopg[binary]>=3.2,<4" "pyyaml>=6.0,<7" "python-dotenv>=1.0,<2" \
       "subjective-scoring[text,sql,code,remote]" "sentence-transformers>=3.0" "torch>=2.2,<3"
   ```
5. 重打包 dist 并在部署机重新执行 install-worker.ps1.

## 升级 (无 install.ps1, 手动 zip + 哈希校验)

1. 产新 dist: `pwsh scripts/windows/package.ps1` → `dist/windows/exam-system/`.
2. 传输到部署点, 解压到临时目录 `D:\exam-new\`.
3. 核对完整性: `Get-FileHash` 重算 dist 内文件 → 与 `manifest.sha256` 比对, 任一不符中止.
4. stop.ps1 停服务 (见下).
5. 覆盖部署根程序文件 (白名单: exam-server.exe / config.production.example.yaml /
   frontend/ / scoring_worker/ / scripts/windows/*.ps1 / docs/*.md).
   - **禁止**覆盖 `config.production.yaml` (含生产密钥, 不在包内).
   - **禁止**触碰 DataRoot / ModelRoot.
6. start.ps1 起服务, 等 /api/health 绿.
7. 失败回滚: 恢复升级前全量备份 (数据库 + DataRoot), 回退到上一个 dist 包.

## 启动 (start.ps1)

前提: API / Worker 两个 ScheduledTask 需提前一次性创建. 一次性命令 (PowerShell 管理员):

```powershell
# API task (运行 exam-server.exe serve)
$api = New-ScheduledTaskAction -Execute "D:\exam-system\exam-server.exe" `
    -Argument "serve -bind 0.0.0.0:18080"
$trig = New-ScheduledTaskTrigger -AtStartup
Register-ScheduledTask -TaskName "ExamAPI" -Action $api -Trigger $trig `
    -RunLevel Highest -Force

# Worker task (运行 scoring_worker 轮询主循环)
$py = "C:\exam-venv\Scripts\python.exe"   # 生产 Python 3.12 venv (pyproject 限定 >=3.12,<3.13)
$worker = New-ScheduledTaskAction -Execute $py `
    -Argument "-m scoring_worker" -WorkingDirectory "D:\exam-system"
Register-ScheduledTask -TaskName "ExamWorker" -Action $worker -Trigger $trig `
    -RunLevel Highest -Force
```

日常启停: `pwsh scripts/windows/start.ps1` / `pwsh scripts/windows/stop.ps1`.
start.ps1 会轮询 `/api/health` 最多 `$HealthTimeoutSec` 秒确认 API 就绪.

## 本地模型加载 (RERANKER_MODEL, 方案 C 双形态)

`scoring_worker/grader_bridge.py` 自动按值类型分流:

| `RERANKER_MODEL` 值形态 | 行为 | 适用场景 |
|---|---|---|
| 存在的目录路径 (绝对或相对部署根) | 按本地目录加载 CrossEncoder | 容器化 / 离线生产部署 |
| 不存在的字符串 (如 `BAAI/bge-reranker-v2-m3`) | 当 HF repo id, 先查 `HF_HOME` cache, 未命中走 HF Hub 下载 | 开发 / 复用已下载 cache |

⚠️ **易踩坑**: `RERANKER_MODEL` 指向 HF cache 顶层目录 (如 `models/models--BAAI--bge-reranker-v2-m3`)
**会失败** — 该目录无 config.json. 必须指向 `snapshots/<hash>/` 子目录, 或直接用 repo id 让
sentence-transformers 自己解析 cache.

生产推荐: 显式绝对路径指向解包好的模型目录, 配合 `HF_HUB_OFFLINE=1` 强制离线.

| 变量 | 示例值 | 说明 |
|---|---|---|
| `RERANKER_MODEL` | `D:\exam-models\bge-reranker-v2-m3` | 本地路径 / HF repo id |
| `RERANK_USE_REMOTE` | `false` | 本地模型; `true` 用远程 reranker API |
| `RERANK_MODEL` | `Pro/BAAI/bge-reranker-v2-m3` | 远程模式下的 model 名 (本地模式忽略) |
| `RERANK_API_URL` / `RERANK_API_KEY` | — | 远程模式用 |
| `HF_HOME` | `D:\exam-models` | 本地 repo id 模式的 cache 根 |
| `DATABASE_URL` | `postgres://exam_app:***@127.0.0.1:5432/exam_system` | worker 必须 |
| `WORKER_ID` | `prod-worker-1` | 多实例区分 claim/续租 |
| `LOG_LEVEL` | `INFO` | DEBUG/INFO/WARNING |

## 精确评分模式 (exact scoring)

题库支持 5 种本地精确评分模式, **不经过 reranker**:

| scoring_mode | 评分器 | 适用题型 |
|---|---|---|
| `enumeration` | enumeration_scorer | 列举题: 评分点子串/同义词匹配, 命中得该条满分 |
| `translation` | translation_scorer | 翻译题: 先校验目标语言, 再按短语条目匹配 |
| `table` | table_scorer | 表格补全: 单元格期望值精确匹配, 可选行标签邻域校验 |
| `ledger` | ledger_scorer | 会计分录: 金额(相对容差) + 科目关键词双条件 |
| `case_analysis` | case_analysis_scorer | 案例分析: phrase 结论点精确命中 + 理由点回调 reranker |

其他模式 (`text` / `calculation` / `code`) 走原有路径.

**标注字段** (写在 `data/papers/*.json` 的 question 上, Go 侧忽略未知键):

```jsonc
{
  "scoring_mode": "enumeration",
  "scoring_points": [
    {"id": "p1", "text": "人力控制", "score": 3, "synonyms": ["手动", "手动控制"]}
  ]
}
```

维护注意事项:
- 改试卷后必须 `python scripts/lint_papers.py` (L1-L6, 零 error 方可提交)
- 改试卷后必须 `python scripts/sync_run_snapshots.py` (否则 hash mismatch)
- `_grade_by_exact_mode` 在 `grader_bridge.py` 的 `grade_subjective()` 入口分派,
  命中直接返回, 不经过 text reranker

## 题库维护

修改 `data/papers/*.json` 后的标准操作流程:

```powershell
# 1. 静态检查 (L1-L6, 零 error 方可提交)
python scripts/lint_papers.py

# 2. 快照同步 (必须, 否则 worker hash mismatch)
python scripts/sync_run_snapshots.py

# 3. 触发重评 (可选, 更新已提交答卷的分数)
# 通过 API 对 submission ids 逐一调 /api/admin/regrade/{id}
```

lint 规则概览:
- **L1 (error)**: text/enumeration 评分点含"答出...之一"等元句式
- **L2 (error)**: text 模式评分点无 CJK 字符 (库的有界修正整体跳过)
- **L3 (error)**: 评分点含日期 (触发库的数字硬校验)
- **L4 (warning)**: 化学式/单字母风险
- **L5 (error)**: 模式配置完整性 (enumeration 需 scoring_points, table 需 cells 等)
- **L6 (warning)**: 分值和不足满分

## 部署后健康自检

### scoring_worker --preflight (评分链路健康)

不 claim job / 不写 DB, 用固定无敏感 fixture 跑一次真实评分, 三层断言拦:
1. lexical fallback (reranker/judge 完全不可用)
2. reranker 降级 warning (如 "语义模型不可用, 已回退到词法相似度")
3. score=0 退化 (fixture 有合理答案不应 0 分)

```powershell
$env:RERANKER_MODEL="D:\exam-models\bge-reranker-v2-m3"
$env:RERANK_USE_REMOTE="false"
$env:DATABASE_URL="postgres://exam_app:***@127.0.0.1:5432/exam_system"
$env:WORKER_ID="preflight"; $env:LOG_LEVEL="INFO"
C:\exam-venv\Scripts\python.exe -m scoring_worker --preflight
# exit 0 = 通过; exit 2 = 检测到降级/异常, 不应放行
```

### API health

```powershell
Invoke-RestMethod http://127.0.0.1:18080/api/health
```

## 日志轮转

- API: `exam-server` 内置 lumberjack, 20MB × 10 保留, 自动滚动 (见 cmd/exam-server main.go).
- Worker: ScheduledTask 输出重定向到日志文件, 运维侧按需配置轮转.

## 备份

- PG 逻辑备份: `pg_dump -U exam_migrator exam_system > backup_$(date).sql`.
- DataRoot: `D:\exam-data\uploads\` 直接归档.
- 升级前**强制**先全量备份 (rollback 回退点).
- 日志 ≠ 备份: 按 12-Factor 把日志当事件流, 不依赖日志做数据恢复.

## 常见故障

| 现象 | 根因 / 处置 |
|---|---|
| preflight exit 2 + warning "回退到词法相似度" | `RERANKER_MODEL` 指向 HF cache 顶层 (无 config.json); 改指 `snapshots/<hash>/` 或用 repo id |
| preflight exit 2 + score=0 | reranker 加载失败但 warnings 未显形; 检查模型路径 / venv 依赖 / 磁盘 |
| **worker 日志大量 snapshot hash mismatch** | 改过 `data/papers/*.json` 但未跑 `sync_run_snapshots.py`; 重跑同步即可 |
| **enumeration 题得 0 分 (matched=0)** | 评分点 text 与作答字面差异大，缺 synonyms; 在 scoring_points 加 `synonyms` 字段 |
| API 起不来 "ScheduledTask 不存在" | 未一次性 New-ScheduledTask; 见"启动"章节 |
| worker claim 拒绝 "exam_app 无 INSERT 权限" | migrations 无 GRANT; 手动 `GRANT ALL ON ALL ... TO exam_app` |
| Go serve 404 前端 | dist 缺 frontend/; package.ps1 时 frontend/ 不存在会 warning 但继续 |
