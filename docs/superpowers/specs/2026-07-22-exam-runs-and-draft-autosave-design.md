# 考试轮次、草稿自动保存与考试管理设计

**日期：** 2026-07-22
**状态：** 设计已确认，待实施

## 背景

当前系统以试卷 `open` / `closed` 状态控制考试访问。考试时长从每位考生点击“开始考试”时单独计算，考试会话只保存开始时间，答案仅在最终提交时发送到服务端。

本次改造需要同时满足：

1. “试卷 / 专业”页负责试卷管理以及快速发布、结束和批量操作。
2. 原“发布考试”页改为“考试管理”，集中展示考试运行信息、链接和二维码。
3. 每次发布同一试卷都创建一个新的考试轮次，而不是重新打开旧轮次。
4. 继续采用“每位考生独立计时”，不引入试卷级全局倒计时。
5. 考生答题过程中持续向服务端保存最新草稿。
6. 管理员结束考试时立即停止答题，经过 5 秒只读收卷缓冲后，将所有未提交会话按服务端最新草稿自动提交。

## 非目标

- 不增加试卷级“剩余考试时间”。
- 不保存每次草稿修改历史，只保存每个会话的最新版本。
- 不引入 WebSocket 或 SSE，状态同步继续使用 HTTP 轮询。
- 不增加考生账号体系；仍以姓名、工号和考试链接进入考试。
- 本次不把个人计时到期改造成服务端定时收卷；个人到期仍沿用考生端自动提交和服务端截止时间校验。
- 同一试卷不允许同时存在两个 `open` 或 `closing` 轮次。

## 核心决策

### 每次发布创建新轮次

旧轮次永不重新打开。点击“发布”或“发布新轮次”时创建新的：

- `run_id`
- 轮次编号
- 随机公开链接 token
- 考试时长快照
- 不可变试卷快照
- 发布时间

旧轮次链接只显示“本轮考试已结束”，不会自动进入新轮次。同一员工可在新轮次再次参加考试。

### 每位考生独立计时

考试时长从服务端首次创建考试会话时开始计算：

```text
deadline_at = started_at + run.duration_minutes
```

刷新页面或重复调用开始接口必须返回原来的 `started_at` 和 `deadline_at`，不得重置计时。

### 管理员结束考试自动收卷

轮次状态机为：

```text
open
  ↓ 管理员结束考试
closing（5 秒，只读并同步最终草稿）
  ↓ 服务端统一生成提交
closed
```

- `open`：允许开始、保存草稿和正常提交。
- `closing`：禁止新会话，考生端立即锁定输入；已有会话只允许在 `finalize_at` 前同步最终草稿。
- `closed`：禁止开始、保存草稿和提交。

进入 `closing` 后不允许继续答题。5 秒窗口仅用于把浏览器中已经填写但尚未同步的内容上传到服务端。

## 架构边界

### 试卷内容

`data/papers/index.json` 和 `data/papers/{slug}.json` 继续负责可编辑试卷及其静态元数据。`index.json` 不再作为考试运行状态的真实来源。

### 考试运行状态

轮次、会话、草稿和提交关系由 SQLite 管理。管理端列表中的考试状态通过当前或最近轮次派生。

### 轮次快照

发布时将当前试卷原子复制到：

```text
data/exam_runs/{run_id}.json
```

本轮的考生加载、答案校验和评分始终使用该快照。后续编辑基础试卷不会改变历史轮次。

新增 `backend/exam_run_service.py`，集中负责：

- 发布和关闭轮次
- 创建和恢复考试会话
- 保存最新草稿
- 手动提交和管理员自动收卷
- 查询管理端运行数据
- 恢复未完成的 `closing` 轮次

路由层不直接组合数据库和文件写入逻辑。

应用运行时启动一个轻量收卷循环，每秒领取一次 `finalize_at <= now` 的 `closing` 轮次。领取和收卷均通过数据库条件更新及唯一约束保证幂等；应用启动时先立即执行一次扫描，恢复进程退出期间到期的轮次。

## 数据模型

### `exam_runs`

```text
id                   TEXT PRIMARY KEY
paper_id             TEXT NOT NULL
round_no             INTEGER NOT NULL
public_token_hash    TEXT UNIQUE
status               TEXT NOT NULL             -- open / closing / closed
duration_minutes     INTEGER NOT NULL
snapshot_path        TEXT
snapshot_hash        TEXT
is_legacy            INTEGER NOT NULL DEFAULT 0
opened_at            TEXT NOT NULL
closing_started_at   TEXT
finalize_at          TEXT
closed_at            TEXT
created_at           TEXT NOT NULL
UNIQUE(paper_id, round_no)
```

对 `open` / `closing` 状态增加约束，确保同一 `paper_id` 最多只有一个活动轮次。正常发布的轮次必须具有 token 和快照；仅迁移生成的 `is_legacy=1` 历史轮次允许这些字段为空。

### `exam_sessions`

```text
id                    TEXT PRIMARY KEY
run_id                TEXT NOT NULL
employee_id           TEXT NOT NULL
name                  TEXT NOT NULL
department            TEXT
session_token_hash    TEXT NOT NULL
started_at            TEXT NOT NULL
deadline_at           TEXT NOT NULL
draft_json            TEXT NOT NULL DEFAULT '{}'
draft_revision        INTEGER NOT NULL DEFAULT 0
draft_saved_at        TEXT
status                TEXT NOT NULL             -- active / submitted
client_ip             TEXT
user_agent            TEXT
created_at            TEXT NOT NULL
updated_at            TEXT NOT NULL
UNIQUE(run_id, employee_id)
```

考试身份信息在开始考试时写入会话，使管理员自动收卷可以直接创建完整提交记录。

### `submissions`

现有表增加：

```text
run_id TEXT NOT NULL
```

唯一约束由：

```text
UNIQUE(employee_id, paper_id)
```

调整为：

```text
UNIQUE(employee_id, run_id)
```

`paper_id` 和 `paper_name` 继续保留，供现有筛选、统计和导出使用。管理员自动收卷记录：

```text
auto_submit_reason = 'admin_closed'
```

## 数据迁移

1. 迁移前备份 SQLite 数据库。
2. 对每个已有提交记录的专业创建一个只读历史轮次。
3. 将已有提交绑定到相应历史轮次。
4. 重建提交表唯一约束为 `(employee_id, run_id)`。
5. 旧 `exam_sessions` 没有轮次身份，不做迁移并在升级时清理。
6. 部署前必须确认没有正在进行的考试。
7. 旧 `index.json.status` 保留兼容读取，但运行接口不再以其为准。

历史提交无法证明对应当前试卷内容，因此迁移轮次标记为 `is_legacy=1`，不伪造试卷快照、链接或二维码。成绩页面将其显示为“历史数据”。

## API 设计

### 加载轮次试卷

```http
GET /api/exam?paper={slug}&run={public_token}
```

返回指定轮次的脱敏试卷快照、轮次状态和每人考试时长。无效 token 返回 `RUN_NOT_FOUND`；关闭轮次返回用于展示结束页的状态信息，不返回新轮次内容。

### 开始或恢复考试

```http
POST /api/exam/start
```

请求：

```json
{
  "paper_id": "software-development",
  "run_token": "...",
  "name": "张三",
  "employee_id": "E1001",
  "department": "研发部"
}
```

响应：

```json
{
  "session_id": "...",
  "session_token": "...",
  "started_at": "...",
  "deadline_at": "...",
  "draft_revision": 0,
  "answers": {}
}
```

`UNIQUE(run_id, employee_id)` 保证首次开始时间不被覆盖。正常刷新使用浏览器保存的会话 token 恢复；恢复流程始终返回原截止时间和最后保存的草稿。

### 保存草稿

```http
PUT /api/exam/sessions/{session_id}/draft
```

请求：

```json
{
  "session_token": "...",
  "revision": 12,
  "answers": {}
}
```

规则：

- 每次上传完整答案映射，不使用增量 patch。
- 只接受高于服务器当前版本的 `revision`。
- 草稿允许答案不完整，但拒绝未知题目 ID 和非法数据结构。
- `open` 状态正常保存。
- `closing` 状态仅在 `finalize_at` 前允许已有会话进行最终同步。
- `closed` 状态拒绝保存。

响应包含服务器保存时间、已接受版本和当前轮次状态。

### 查询会话状态

```http
GET /api/exam/sessions/{session_id}/status
```

使用会话 token 鉴权，返回：

- 轮次状态
- 会话状态
- `deadline_at`
- 已保存草稿版本和时间
- `finalize_at`
- 自动提交后的 `submission_id`

### 手动提交

```http
POST /api/submit
```

提交改为使用 `session_id` 和 `session_token`，服务端从会话读取姓名、工号、专业、轮次和开始时间，不再信任客户端传入的 `started_at`。

手动提交事务完成：

1. 校验轮次、会话、截止时间和答案。
2. 创建 `grading` 状态的提交记录。
3. 标记会话 `submitted`。
4. 事务提交后进入评分队列。

轮次已进入 `closing` 后发起的新手动提交返回 `RUN_CLOSING`；已经先于关闭事务完成的提交保留，统一收卷会跳过已有提交。

### 发布新轮次

```http
POST /api/admin/papers/{slug}/open
```

行为：

1. 校验试卷存在、非空且结构有效。
2. 确认没有 `open` 或 `closing` 轮次。
3. 原子创建试卷快照。
4. 创建轮次、随机 token 和递增轮次编号。
5. 返回轮次信息、考试链接和二维码信息。

### 结束考试

```http
POST /api/admin/papers/{slug}/close
```

首次调用将活动轮次设为 `closing`：

```json
{
  "success": true,
  "status": "closing",
  "active_sessions": 12,
  "finalize_at": "2026-07-22T08:00:05+00:00"
}
```

到达 `finalize_at` 后，幂等收卷事务：

1. 锁定轮次并再次确认状态。
2. 查询所有尚未正式提交的会话。
3. 使用每个会话最后保存的草稿创建提交；无草稿时提交空答案。
4. 标记会话 `submitted`。
5. 将轮次更新为 `closed` 并写入 `closed_at`。
6. 事务提交后批量加入受限评分队列。

重复结束请求返回当前状态，不产生重复提交。

### 管理端考试数据

```http
GET /api/admin/exams
```

每张试卷返回当前或最近轮次：

- 试卷名称和编码
- 轮次编号及状态
- 发布时间、收卷时间和关闭时间
- 每位考生考试时长
- 已开始、已提交、当前答题人数
- 当前轮次考试 URL
- 二维码按需加载，避免列表响应携带大量 base64

从未发布过的试卷返回派生状态 `unpublished`，不创建空轮次。

### 批量操作

```http
POST /api/admin/papers/batch/open
POST /api/admin/papers/batch/close
```

请求：

```json
{
  "slugs": ["paper-a", "paper-b"]
}
```

批量操作采用“每张试卷独立事务”，允许部分成功：

```json
{
  "success": false,
  "requested": 3,
  "updated": 2,
  "skipped": 0,
  "papers": [],
  "errors": [
    {"slug": "paper-c", "code": "EMPTY_QUESTION_BANK", "message": "试卷无题目"}
  ]
}
```

同一请求中的重复 slug 去重，空列表由请求模型拒绝。单张试卷的发布或收卷必须完整成功或保持不变。

## 前端交互

### 试卷 / 专业页

- 增加行复选框、表头全选和半选状态。
- 增加“批量发布”和“批量结束”，未选择时禁用。
- 已关闭显示“发布”，直接创建新轮次，不再跳转页面。
- 考试中显示“结束”。
- 收卷中显示禁用的“正在自动收卷”。
- 编辑、预览和删除继续保留；`open` / `closing` 时禁止编辑和删除。
- 从未发布显示“未发布”；已存在轮次但当前关闭显示“已关闭”。
- 试卷存在任何历史轮次时禁止硬删除，避免轮次和成绩失去所属试卷；软删除不在本次范围内。
- 结束前展示当前答题人数，并明确提示 5 秒后按服务器草稿自动提交。
- 批量结束展示所选轮次的当前答题总人数。

### 考试管理页

侧边栏和页面可见文案由“发布考试”改为“考试管理”。内部 `data-view="publish"` 暂时保留，避免破坏已保存的视图状态。

卡片展示：

- 试卷名称、编码和轮次编号
- 未发布、考试中、收卷中或已关闭
- 发布时间和关闭时间
- 每位考生考试时长
- 已开始、已提交和当前答题人数
- 当前轮次链接、复制、打开和二维码
- 考试中显示“结束考试”
- 已关闭显示“发布新轮次”

排序为“收卷中、考试中、未发布、已关闭”，同状态按发布时间倒序。已关闭卡片弱化显示。收卷中每 2 秒刷新，其余状态每 15 秒刷新。

“试卷 / 专业”页的提交数继续表示该试卷所有轮次的累计提交数；“考试管理”卡片中的人数只统计卡片所示轮次。

### 成绩列表、统计与导出

- 提交查询结果增加 `run_id` 和轮次编号。
- 成绩列表增加“轮次”列，并允许按试卷后进一步筛选轮次。
- 未选择轮次时，现有试卷筛选和总览统计继续聚合该试卷的所有轮次。
- 导出结果增加轮次编号；迁移数据标记为“历史数据”。
- 重新评分必须使用提交所属轮次的快照，不能加载当前可编辑试卷。

### 考生端草稿同步

页面显示：

```text
未保存 → 保存中 → 已保存 14:32:10
```

使用统一的 2 秒同步循环：

- 有答案变化时上传完整草稿。
- 没有变化时查询会话状态。
- 每分钟最多约 30 次请求。
- 页面关闭前使用 `sendBeacon` 补存一次。

会话信息保存在 `sessionStorage`，刷新后恢复原截止时间及服务器草稿。

检测到 `closing` 后：

1. 立即锁定所有输入控件。
2. 停止正常计时和防切屏逻辑。
3. 立即上传最后一版完整草稿。
4. 显示“考试已结束，正在自动收卷”。
5. 自动提交完成后展示成功状态和提交编号。

网络断开时保留浏览器内答案，并明确显示草稿未保存。管理员收卷只能使用最后一次成功写入服务器的版本。

## 并发与故障恢复

- 发布：先原子写快照，再创建轮次；任一步失败都不开放考试，并清理孤立快照。
- 草稿：通过递增 `revision` 防止乱序覆盖，旧版本返回 `STALE_DRAFT_REVISION`。
- 手动提交与管理员关闭：通过数据库事务排序；提交事务先完成则保留手动提交，关闭先进入 `closing` 则由统一收卷处理。
- 收卷：收卷函数必须幂等，唯一约束防止重复提交。
- 进程重启：启动时扫描 `finalize_at <= now` 的 `closing` 轮次并继续收卷。
- 评分恢复：提交记录先持久化为 `grading`；启动时重新加入尚未完成的评分任务，评分结果更新保持幂等。
- 批量操作：每张试卷独立处理并返回逐项结果。
- 限流：草稿和状态接口使用基于会话的独立限流，不与普通 IP 限流共用计数。

## Token 与数据安全

- 轮次公开 token 和会话 token 使用密码学安全随机数。
- 数据库只保存 token 哈希；日志不得记录完整 token、草稿或考生答案。
- 轮次 token 仅用于定位公开考试轮次，不替代考生身份认证。
- 草稿、状态和提交接口必须同时校验会话 ID 与会话 token。
- 管理端轮次、草稿统计和批量操作继续使用现有管理员认证。

## 错误码

| 错误码 | 处理 |
|---|---|
| `RUN_NOT_FOUND` | 链接无效 |
| `RUN_CLOSED` | 显示本轮已结束 |
| `RUN_CLOSING` | 锁定答题并等待自动收卷 |
| `SESSION_NOT_FOUND` | 无有效考试会话 |
| `INVALID_SESSION_TOKEN` | 拒绝草稿或提交 |
| `STALE_DRAFT_REVISION` | 忽略旧版本，保留服务器新版本 |
| `EXAM_TIMEOUT` | 拒绝超时手动提交 |
| `DUPLICATE_SUBMISSION` | 返回已有提交信息 |
| `DRAFT_SAVE_FAILED` | 保留本地答案并重试 |
| `EMPTY_QUESTION_BANK` | 禁止发布空卷 |
| `ACTIVE_RUN_EXISTS` | 同一试卷已有活动轮次 |

## 测试设计

### 后端

- 发布新轮次生成递增编号、随机 token 和不可变快照。
- 旧链接不能进入新轮次。
- 重复开始不重置计时，刷新恢复草稿。
- 同一员工同轮次只能提交一次，新轮次可以再次参加。
- 草稿版本乱序、非法 token、未知题目和非法答案结构。
- 手动提交与自动收卷竞争仅产生一份提交。
- 最新草稿和空草稿均能在管理员关闭时自动提交。
- 自动提交原因写为 `admin_closed`。
- 重复关闭不重复提交。
- `closing` 期间进程重启后可恢复收卷。
- 评分任务可在进程重启后重新领取。
- 批量发布和结束返回部分失败结果。
- 旧数据库提交正确迁移到历史轮次。
- 成绩查询、统计、导出和重新评分使用正确轮次。

### 前端

- 行选择、全选、半选和批量按钮状态。
- 发布按钮直接调用接口，不再跳转页面。
- 卡片排序以及未发布、考试中、收卷中、已关闭状态展示。
- 历史轮次和迁移数据展示。
- 草稿保存状态、失败重试和版本递增。
- 刷新恢复答案及原截止时间。
- 收卷时锁定控件、最终同步并显示自动提交结果。
- 页面中不存在试卷级剩余考试时间。

### 端到端验收

1. 发布第一轮考试。
2. 考生 A 正常手动提交。
3. 考生 B 只保存部分草稿。
4. 考生 C 开始考试但不作答。
5. 管理员结束考试。
6. A 不重复提交，B 按最新草稿自动提交，C 以空答案提交。
7. 发布第二轮，同一员工可以再次参加。
8. 第一轮链接只显示“本轮考试已结束”。

## 预计修改范围

- `backend/main.py`
- `backend/database.py`
- `backend/paper_store.py`
- `backend/question_loader.py`
- `backend/grader.py`
- `backend/review_service.py`
- `backend/exporter.py`
- 新增 `backend/exam_run_service.py`
- `frontend/admin.html`
- `frontend/js/admin.js`
- `frontend/js/papers.js`
- `frontend/js/exam.js`
- `frontend/js/detail.js`
- `frontend/css/style.css`
- `tests/test_papers.py`
- `tests/test_frontend_static.py`
- 新增轮次、草稿和收卷相关测试文件
- `README.md` 与相关设计说明

## 验收结论

实现完成后应满足：

- 页面职责清晰，试卷页用于管理和快速操作，考试管理页用于运行态查看。
- 每次发布形成可追踪、不可变的新轮次。
- 每位考生仍按自己的开始时间独立计时。
- 草稿持续保存在服务端，刷新可恢复。
- 管理员结束考试后，所有未提交会话在 5 秒只读缓冲结束时按最新服务器草稿自动生成正式提交。
- 重复请求、并发提交、进程重启和批量部分失败不会产生重复提交或状态错乱。
