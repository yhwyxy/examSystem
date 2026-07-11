# 企业在线考试兼批改系统设计文档

## 1. 背景与目标

企业内部需要一套轻量、可本地部署的在线考试与批改系统，用于组织固定试卷考试，支持客观题自动判分、主观题语义相似度判分，以及管理员人工复核与成绩导出。

### 1.1 核心目标

1. 部署简单：单机可运行，依赖尽量少。
2. 员工端轻量：手机扫码进入，填写姓名工号后答题。
3. 判分可靠：客观题程序判分，主观题 Embedding 语义相似度评分。
4. 可复核：管理员可查看完整答卷并人工改分。
5. 可导出：支持成绩导出为 Excel/CSV。

### 1.2 核心流程

1. 管理员启动本地服务。
2. 员工手机扫码进入答题页面。
3. 员工手动填写姓名和工号后答题。
4. 客观题由后端程序自动判分。
5. 主观题使用 Embedding 模型进行语义相似度评分，Ollama 不可用时回退到本地模型或关键词相似度。
6. 员工提交后只看到提交成功，不展示成绩和参考答案。
7. 管理员后台可以实时查看提交记录、成绩和复核状态。
8. 管理员可以查看每位员工的完整试卷答案，并对主观题人工复核改分。
9. 管理员可导出成绩。

---

## 2. 需求规格

### 2.1 功能需求

| 编号 | 需求 | 说明 | 优先级 |
|---|---|---|---|
| FR-001 | 固定试卷 | 试卷通过 questions.json 配置，后端只读加载 | P0 |
| FR-002 | 扫码入口 | 管理员启动服务后生成考试二维码 | P0 |
| FR-003 | 身份填写 | 员工进入答题页后填写姓名、工号 | P0 |
| FR-004 | 客观题判分 | 单选、判断严格匹配，多选按比例给分 | P0 |
| FR-005 | 主观题判分 | Embedding 语义相似度判分，失败回退关键词 | P0 |
| FR-006 | 员工不显示成绩 | 提交后仅显示提交成功 | P0 |
| FR-007 | 管理员成绩列表 | 管理员查看所有提交记录与成绩 | P0 |
| FR-008 | 试卷详情复核 | 管理员查看每位员工完整答案与判分详情 | P0 |
| FR-009 | 人工改分 | 管理员可以修改主观题最终分数 | P0 |
| FR-010 | 成绩导出 | 导出所有员工姓名、工号、成绩、复核状态 | P0 |
| FR-011 | 考试时长配置 | 考试时长通过配置文件灵活调整 | P1 |
| FR-012 | 防重复提交 | 默认按工号限制单次提交 | P0 |

### 2.2 非功能需求

| 编号 | 需求 | 说明 |
|---|---|---|
| NFR-001 | 易部署 | 本地电脑可直接运行，优先减少依赖复杂度 |
| NFR-002 | 数据安全 | 参考答案不得下发到员工端 |
| NFR-003 | 并发安全 | SQLite 开启 WAL，支持多人近似同时提交 |
| NFR-004 | 可维护 | 题目、分数、考试时长、模型配置可通过文件调整 |
| NFR-005 | 可复核 | 主观题机器评分必须保存评分详情，支持人工复核 |
| NFR-006 | 可降级 | Embedding 不可用时必须自动回退到关键词相似度 |
| NFR-007 | 可追溯 | 人工改分需要记录改分日志 |

---

## 3. 总体架构设计

### 3.1 架构选择

系统采用轻量 Web 单体架构：

```text
员工手机浏览器
    |
    | HTTP
    v
FastAPI 本地服务
    |-- 静态页面托管：exam.html / admin.html / detail.html
    |-- 题目下发：过滤参考答案
    |-- 判分服务：客观题 + 主观题
    |-- 管理员 API：列表、详情、复核、导出
    |
    |-- questions.json：固定试卷与参考答案
    |-- exam.db：提交记录、判分结果、复核日志
    |
    |-- subjective-scoring：文本/SQL/代码多引擎主观题评分
```

### 3.2 技术栈

| 层级 | 技术 | 说明 |
|---|---|---|
| 后端 | FastAPI | API、页面托管、判分入口 |
| 数据库 | SQLite | 单文件数据库，开启 WAL 模式 |
| 题库 | JSON 文件 | 固定试卷，只读加载 |
| 前端 | HTML + CSS + Vanilla JS | 无需 Vue/React 构建链路 |
| 主观题判分 | subjective-scoring | 评分点 / SQL AST / 代码混合 |
| 主观题语义 | sentence-transformers CrossEncoder（可选） | 无模型时词法回退 |
| 导出 | openpyxl 或 csv | 优先 xlsx，失败可回退 csv |
| 二维码 | qrcode | 生成考试入口二维码 |

### 3.3 目录结构

```text
examSystem/
├── main.py                  # 启动入口
├── config.yaml              # 运行配置
├── requirements.txt
├── pyproject.toml
├── data/
│   ├── questions.json       # 题库
│   └── exam.db              # SQLite（运行时生成）
├── backend/
│   ├── main.py              # FastAPI 应用与路由
│   ├── config.py            # 配置加载与校验
│   ├── database.py          # SQLite 访问层
│   ├── question_loader.py   # questions.json 加载、校验、脱敏
│   ├── grader.py            # 判分总入口，编排客观题和主观题
│   ├── objective_grader.py  # 单选、多选、判断题判分
│   ├── grader.py           # 判分入口（主观题调用 subjective-scoring）
│   ├── review_service.py    # 人工复核、改分、总分重算
│   ├── exporter.py          # xlsx / csv 导出
│   └── utils.py             # 局域网 IP、二维码、时间工具
├── frontend/
│   ├── exam.html
│   ├── admin.html
│   ├── detail.html
│   ├── css/style.css
│   └── js/
│       ├── exam.js
│       ├── admin.js
│       └── detail.js
├── docs/
│   └── design.md
└── tests/
    ├── test_core.py
    └── test_frontend_static.py
```

---

## 4. 模块职责

| 模块 | 职责 |
|---|---|
| config.py | 加载 config.yaml，提供类型化配置对象 |
| question_loader.py | 加载、校验题库，员工端脱敏 |
| database.py | 提交记录 CRUD、复核日志、统计查询 |
| objective_grader.py | 单选、多选、判断题判分 |
| grader.py | 客观题 + 主观题（subjective-scoring） |
| grader.py | 编排整卷判分，汇总分数与复核状态 |
| review_service.py | 人工改分、重算总分、写复核日志 |
| exporter.py | 成绩导出 |
| main.py | HTTP API、静态资源、鉴权与限流 |

---

## 5. 配置设计

`config.yaml` 示例：

```yaml
server:
  host: "0.0.0.0"
  port: 8000
  allow_origins:
    - "http://127.0.0.1:8000"
    - "http://localhost:8000"

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
  sync_grading: true

admin:
  enable_auth: false
  password: null

export:
  format: "xlsx"
```

### 5.1 配置说明

| 配置项 | 说明 |
|---|---|
| server.host | 服务监听地址，本地局域网访问使用 0.0.0.0 |
| server.port | 服务端口 |
| server.allow_origins | CORS 允许源，生产环境勿使用 `*` |
| exam.duration_minutes | 单次考试时长 |
| exam.auto_submit | 前端超时后是否自动提交 |
| exam.grace_period_seconds | 提交时长校验容忍秒数 |
| exam.duplicate_key | 默认按 employee_id 限制重复提交 |
| exam.enable_global_time_window | 是否启用全局考试开始和结束时间 |
| grading.sync_grading | 是否同步判分 |
| （主观题模型） | 由 subjective-scoring 的 SubjectiveScoringService / 环境变量配置 |
| admin.enable_auth | MVP 默认 false，仅适用于可信局域网 |

---

## 6. 试卷 JSON 设计

### 6.1 示例结构

文件路径：`data/questions.json`

```json
{
  "exam_info": {
    "title": "企业内部考试",
    "description": "请在规定时间内完成答题。",
    "total_score": 100,
    "passing_score": 60
  },
  "questions": [
    {
      "id": "q1",
      "type": "single_choice",
      "question": "以下哪个是 Python 的 Web 框架？",
      "options": [
        { "key": "A", "text": "React" },
        { "key": "B", "text": "Django" },
        { "key": "C", "text": "Vue" },
        { "key": "D", "text": "Angular" }
      ],
      "answer": "B",
      "score": 5
    },
    {
      "id": "q2",
      "type": "multiple_choice",
      "question": "以下哪些 HTTP 状态码表示成功？",
      "options": [
        { "key": "A", "text": "200" },
        { "key": "B", "text": "404" },
        { "key": "C", "text": "201" },
        { "key": "D", "text": "204" }
      ],
      "answer": ["A", "C", "D"],
      "score": 5
    },
    {
      "id": "q3",
      "type": "true_false",
      "question": "TCP 是无连接协议。",
      "answer": false,
      "score": 2
    },
    {
      "id": "q4",
      "type": "short_answer",
      "question": "简述 RESTful API 的设计原则。",
      "answer": "RESTful API 设计原则包括：资源导向，使用名词表示资源；使用 HTTP 方法映射 CRUD 操作；无状态通信；统一接口；合理使用 HTTP 状态码；支持缓存。",
      "score": 10,
      "scoring_rubric": "资源导向 3 分；HTTP 方法 2 分；无状态 2 分；统一接口和状态码 2 分；缓存 1 分。"
    }
  ]
}
```

### 6.2 题型约束

| 题型 | type | answer 类型 | 说明 |
|---|---|---|---|
| 单选题 | single_choice | string | 例如 B |
| 多选题 | multiple_choice | string[] | 例如 [A, C, D] |
| 判断题 | true_false | boolean | true 或 false |
| 简答题 | short_answer | string | 参考答案文本 |
| 论述题 | essay | string | 参考答案文本 |

### 6.3 启动校验规则

系统启动时必须校验 `questions.json`：

1. `exam_info.title` 必须存在。
2. `questions` 必须是非空数组。
3. 每道题 `id` 必须唯一。
4. 每道题 `type` 必须属于允许题型。
5. 每道题 `score` 必须大于 0。
6. 客观题必须存在 `options` 和 `answer`。
7. 多选题 `answer` 必须是非空数组。
8. 主观题必须存在 `answer`，建议存在 `scoring_rubric`。
9. 所有题目分数之和应等于 `exam_info.total_score`，不一致时启动失败或给出明确错误。

### 6.4 员工端题目脱敏

员工端接口返回题目时必须移除：

1. `answer`
2. `scoring_rubric`
3. 任何参考答案、评分标准和机器判分相关字段

---

## 7. 数据库设计

数据库文件：`data/exam.db`

### 7.1 SQLite 初始化配置

```sql
PRAGMA journal_mode=WAL;
PRAGMA synchronous=NORMAL;
PRAGMA busy_timeout=5000;
PRAGMA foreign_keys=ON;
```

### 7.2 submissions 表

```sql
CREATE TABLE IF NOT EXISTS submissions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    employee_id TEXT NOT NULL,
    answers_json TEXT NOT NULL,
    grading_detail_json TEXT NOT NULL,
    objective_score REAL NOT NULL DEFAULT 0,
    subjective_score_machine REAL NOT NULL DEFAULT 0,
    subjective_score_final REAL NOT NULL DEFAULT 0,
    total_score REAL NOT NULL DEFAULT 0,
    review_status TEXT NOT NULL DEFAULT 'pending',
    started_at TEXT,
    submitted_at TEXT NOT NULL,
    reviewed_at TEXT,
    reviewer_note TEXT,
    client_ip TEXT,
    user_agent TEXT,
    UNIQUE(employee_id)
);
```

### 7.3 review_logs 表

```sql
CREATE TABLE IF NOT EXISTS review_logs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    submission_id INTEGER NOT NULL,
    question_id TEXT NOT NULL,
    old_score REAL,
    new_score REAL,
    note TEXT,
    created_at TEXT NOT NULL,
    FOREIGN KEY(submission_id) REFERENCES submissions(id)
);
```

### 7.4 grading_detail_json 结构

`grading_detail_json` 保存逐题判分详情，结构如下：

```json
[
  {
    "question_id": "q1",
    "type": "single_choice",
    "question": "以下哪个是 Python 的 Web 框架？",
    "student_answer": "B",
    "reference_answer": "B",
    "score": 5,
    "max_score": 5,
    "is_correct": true,
    "review_status": "auto_scored"
  },
  {
    "question_id": "q4",
    "type": "short_answer",
    "question": "简述 RESTful API 的设计原则。",
    "student_answer": "RESTful API 是一种面向资源的接口设计风格。",
    "reference_answer": "RESTful API 设计原则包括资源导向、HTTP 方法、无状态、统一接口和缓存。",
    "scoring_rubric": "资源导向 3 分；HTTP 方法 2 分；无状态 2 分；统一接口和状态码 2 分；缓存 1 分。",
    "machine_score": 7,
    "final_score": 7,
    "max_score": 10,
    "grading_method": "subjective_scoring:TextRerankerScorer",
    "similarity": 0.7,
    "confidence": null,
    "reason": null,
    "fallback_reason": null,
    "review_status": "need_review",
    "manually_reviewed": false
  }
]
```

### 7.5 review_status 取值

| 状态 | 说明 |
|---|---|
| pending | 尚未人工复核 |
| reviewed | 已人工复核 |
| need_review | 机器判断需要复核 |
| low_confidence | 低置信度，建议优先复核 |
| high_confidence | 高置信度，可抽查 |
| auto_scored | 客观题自动判分 |

---

## 8. 判分设计

### 8.1 客观题判分

#### 8.1.1 单选题

规则：

```text
学生答案 == 参考答案 => 满分
否则 => 0 分
```

#### 8.1.2 判断题

规则：

```text
学生答案 == 参考答案 => 满分
否则 => 0 分
```

#### 8.1.3 多选题

规则：

1. 参考答案集合为 R。
2. 学生答案集合为 S。
3. 如果 S 中存在不属于 R 的选项，表示错选，得 0 分。
4. 如果没有错选，按正确选中数量比例给分。

公式：

```text
score = max_score × |S ∩ R| / |R|
```

示例：

```text
参考答案：A, C, D
学生答案：A, C
得分：满分 × 2 / 3
```

### 8.2 主观题判分

主观题由独立库 [subjective-scoring](https://github.com/yhwyxy/subjective-scoring) 处理：

```text
Text 题：评分点 + CrossEncoder/词法 + 规则拦截
SQL 题：sqlglot AST 结构比较
Code 题：tree-sitter 结构分 + 语义分融合
```

题库字段 `scoring_rubric` 会解析为 `scoring_points`；也可直接配置 `scoring_points` / `scoring_mode` / `code_language`。

### 8.3 语义模型与复核

- 默认 CrossEncoder：`BAAI/bge-reranker-base`（sentence-transformers）
- 无模型时回退词法相似度
- 复核阈值仍由 `review.*` 配置（管理端展示用）；引擎侧另有 `ScoringOptions.manual_review_thresholds`

### 8.4 同步与异步策略

MVP 阶段采用同步判分：

1. 员工提交后，后端立即执行客观题判分。
2. 主观题通过 Embedding 模型计算相似度。
3. 后端保存完整判分结果。
4. 员工端只收到提交成功。

后续如果考试人数或主观题数量增加，可升级为异步队列判分。

---

## 9. 考试时间设计

### 9.1 时间控制策略

考试时间由配置项 `exam.duration_minutes` 控制。

MVP 阶段采用：

1. 前端显示倒计时。
2. 前端超时后自动提交。
3. 后端保存 `started_at` 和 `submitted_at`。
4. 后端在提交时校验是否超过允许时长。

### 9.2 刷新页面处理

员工开始答题后，前端应将开始时间缓存到浏览器本地存储中。页面刷新后继续使用原开始时间计算剩余时间。

### 9.3 服务端校验

提交时服务端校验：

```text
submitted_at <= started_at + duration_minutes + grace_period
```

`grace_period` 可默认为 30 秒，用于容忍网络延迟。

### 9.4 全局考试时间窗口

MVP 默认不启用全局时间窗口。如果后续启用，则通过以下配置控制：

```yaml
exam:
  enable_global_time_window: true
  start_time: "2026-07-09T09:00:00"
  end_time: "2026-07-09T10:00:00"
```

---

## 10. 员工端设计

### 10.1 访问方式

管理员启动服务后，系统生成考试地址：

```text
http://本机局域网IP:8000/exam
```

员工通过手机扫码访问。

### 10.2 页面流程

1. 打开考试页面。
2. 填写姓名和工号。
3. 点击开始考试。
4. 获取不含答案的试卷。
5. 显示题目和倒计时。
6. 员工手动提交或超时自动提交。
7. 后端判分并保存。
8. 页面显示提交成功。

### 10.3 员工端禁止展示内容

员工端不得展示：

1. 参考答案。
2. 评分 rubric。
3. 客观题对错。
4. 主观题得分。
5. 总分。
6. 机器评分理由。

---

## 11. 管理员后台设计

### 11.1 访问方式

```text
http://localhost:8000/admin
http://本机局域网IP:8000/admin
```

### 11.2 管理员鉴权说明

MVP 阶段根据当前需求不启用管理员登录和访问口令，仅适用于可信局域网环境。

由于管理员后台可以查看员工答案和导出成绩，正式考试或生产使用时建议至少增加简单管理员口令。

### 11.3 监控面板

展示：

1. 已提交人数。
2. 平均分。
3. 最高分。
4. 最低分。
5. 待复核人数。
6. 低置信度主观题数量。
7. 最近提交记录。

### 11.4 成绩列表

| 字段 | 说明 |
|---|---|
| 姓名 | 员工填写 |
| 工号 | 员工填写，唯一 |
| 客观题分数 | 自动计算 |
| 主观题机器分 | subjective-scoring 初判 |
| 主观题最终分 | 人工复核后可能变化 |
| 总分 | 客观题 + 主观题最终分 |
| 复核状态 | pending / reviewed / need_review / low_confidence |
| 提交时间 | 员工提交时间 |
| 操作 | 查看详情、复核、导出 |

### 11.5 试卷详情页

管理员点击某个员工后进入试卷详情页，展示完整试卷。

#### 11.5.1 员工信息

1. 姓名
2. 工号
3. 提交时间
4. 客观题分数
5. 主观题机器分
6. 主观题最终分
7. 总分
8. 复核状态

#### 11.5.2 客观题详情

每道客观题展示：

1. 题目
2. 选项
3. 学生答案
4. 参考答案
5. 是否正确
6. 得分 / 满分
7. 多选题选对、漏选、错选情况

#### 11.5.3 主观题详情

每道主观题展示：

1. 题目
2. 学生答案
3. 参考答案
4. 评分 rubric
5. 机器评分方式
6. 初判分数
7. 最终分数
8. 相似度或置信度
9. 评分理由
10. 回退原因
11. 复核状态
12. 人工修改入口

### 11.6 人工复核操作

管理员可以：

1. 修改某道主观题最终分数。
2. 填写复核备注。
3. 标记为已复核。
4. 标记为异常。
5. 重新判分。

改分后系统必须：

1. 更新该题 `final_score`。
2. 重新计算主观题最终总分。
3. 重新计算总分。
4. 写入 `review_logs`。
5. 更新 `review_status`。

---

## 12. API 设计

### 12.1 统一响应格式

成功响应：

```json
{
  "success": true,
  "code": "OK",
  "message": "success",
  "data": {}
}
```

失败响应：

```json
{
  "success": false,
  "code": "DUPLICATE_SUBMISSION",
  "message": "该工号已提交，不能重复提交",
  "data": null
}
```

### 12.2 错误码

| code | 说明 |
|---|---|
| OK | 成功 |
| INVALID_REQUEST | 请求参数错误 |
| INVALID_QUESTION_FILE | 试卷配置错误 |
| DUPLICATE_SUBMISSION | 重复提交 |
| EXAM_NOT_STARTED | 考试尚未开始 |
| EXAM_ENDED | 考试已结束 |
| EXAM_TIMEOUT | 提交超时 |
| SUBMISSION_NOT_FOUND | 提交记录不存在 |
| REVIEW_SCORE_INVALID | 复核分数非法 |
| INTERNAL_ERROR | 服务内部错误 |

### 12.3 员工端 API

#### 12.3.1 获取试卷

```http
GET /api/exam/questions
```

返回不包含答案的题目。

#### 12.3.2 提交答案

```http
POST /api/exam/submit
```

请求示例：

```json
{
  "name": "张三",
  "employee_id": "E001",
  "started_at": "2026-07-09T09:00:00",
  "answers": {
    "q1": "B",
    "q2": ["A", "C"],
    "q3": false,
    "q4": "RESTful API 是一种面向资源的接口设计风格。"
  }
}
```

响应示例：

```json
{
  "success": true,
  "code": "OK",
  "message": "提交成功",
  "data": null
}
```

### 12.4 管理员 API

#### 12.4.1 获取统计数据

```http
GET /api/admin/stats
```

#### 12.4.2 获取提交列表

```http
GET /api/admin/submissions?keyword=张三&review_status=need_review&sort_by=total_score&order=desc
```

#### 12.4.3 获取试卷详情

```http
GET /api/admin/submissions/{submission_id}
```

#### 12.4.4 人工复核改分

```http
POST /api/admin/submissions/{submission_id}/review
```

请求示例：

```json
{
  "question_id": "q4",
  "new_score": 8.5,
  "note": "答案覆盖主要知识点，人工调整为 8.5 分。"
}
```

#### 12.4.5 重新判分

```http
POST /api/admin/submissions/{submission_id}/regrade
```

#### 12.4.6 导出成绩

```http
GET /api/admin/export
```

---

## 13. 二维码生成设计

系统启动时执行：

1. 获取本机局域网 IP。
2. 拼接考试地址。
3. 生成二维码。
4. 在终端打印考试地址、管理员后台地址和二维码。
5. 管理员后台展示二维码图片。

示例：

```text
考试地址：http://192.168.1.20:8000/exam
管理员后台：http://192.168.1.20:8000/admin
```

注意事项：

1. 员工手机必须和部署电脑处于同一局域网。
2. 防火墙需要允许服务端口访问。
3. 如果电脑 IP 改变，需要刷新二维码或重启服务。

---

## 14. 安全设计

### 14.1 MVP 安全措施

1. 参考答案只保存在后端。
2. 员工获取试卷接口不返回答案和 rubric。
3. 提交接口后端重新读取题库判分，不信任前端分数。
4. 默认按工号限制重复提交。
5. SQLite 使用事务写入。
6. 管理员后台无鉴权，仅适用于可信局域网。
7. 可记录提交 IP 和 User-Agent。

### 14.2 后续增强建议

1. 管理员后台增加访问口令。
2. 考试入口增加一次性 token。
3. 增加全局考试时间窗口。
4. 增加员工名单导入和工号白名单。
5. 增加导出文件访问控制。
6. 增加操作审计日志。

---

## 15. 主观题判分边界

系统目标是：

```text
机器初判 + 置信度分级 + 管理员复核异常项
```

系统不是：

```text
全自动免人工判分系统
```

### 15.1 Embedding 优点

1. 部署简单。
2. CPU 可运行。
3. 推理速度快。
4. 支持 Ollama 远程调用和本地 sentence-transformers 模型。

### 15.2 Embedding 局限

1. 只能衡量语义相似度。
2. 难以判断逻辑错误。
3. 难以识别答非所问但文字相近的答案。
4. 不能自然生成评分理由。

---

## 16. 测试与验收标准

### 16.1 功能验收

| 编号 | 验收项 | 预期结果 |
|---|---|---|
| AC-001 | 启动服务 | 成功加载配置和试卷，生成二维码 |
| AC-002 | 员工扫码 | 手机可打开答题页 |
| AC-003 | 题目脱敏 | 员工端接口不包含 answer 和 scoring_rubric |
| AC-004 | 单选判分 | 答案一致满分，不一致 0 分 |
| AC-005 | 判断判分 | 布尔值一致满分，不一致 0 分 |
| AC-006 | 多选判分 | 无错选时按比例给分，有错选时 0 分 |
| AC-007 | 主观题 Embedding | Ollama 可用时使用 Embedding 相似度判分 |
| AC-008 | 主观题回退 | Ollama 不可用时自动回退本地模型或关键词 |
| AC-009 | 重复提交 | 同一工号不能重复提交 |
| AC-010 | 员工提交结果 | 员工只看到提交成功，不看到分数 |
| AC-011 | 管理员列表 | 管理员能看到提交记录和成绩 |
| AC-012 | 试卷详情 | 管理员能查看每位员工逐题答案和参考答案 |
| AC-013 | 人工改分 | 修改主观题分数后总分自动重算 |
| AC-014 | 改分日志 | 人工改分写入 review_logs |
| AC-015 | 成绩导出 | 可导出姓名、工号、成绩、复核状态 |

### 16.2 异常验收

| 编号 | 场景 | 预期结果 |
|---|---|---|
| EX-001 | questions.json 格式错误 | 启动失败并提示具体错误 |
| EX-002 | 题目 ID 重复 | 启动失败并提示重复 ID |
| EX-003 | 分数总和不一致 | 启动失败或明确警告 |
| EX-004 | Ollama 不可用 | 自动回退本地模型或关键词，不影响提交 |
| EX-005 | 非法复核分数 | 接口返回 REVIEW_SCORE_INVALID |
| EX-006 | 查询不存在提交 | 接口返回 SUBMISSION_NOT_FOUND |

---

## 17. 开发计划

### Phase 1：基础框架

1. 搭建 FastAPI 服务。
2. 实现配置加载。
3. 实现试卷 JSON 加载与校验。
4. 初始化 SQLite 和 WAL。
5. 挂载静态页面。
6. 实现局域网 IP 获取和二维码生成。

### Phase 2：员工答题流程

1. 实现员工答题页面。
2. 实现获取脱敏试卷 API。
3. 实现姓名和工号填写。
4. 实现倒计时和自动提交。
5. 实现提交接口。
6. 实现重复提交校验。
7. 实现提交成功页面。

### Phase 3：判分引擎

1. 实现单选题判分。
2. 实现判断题判分。
3. 实现多选题按比例判分。
4. 实现 Embedding 语义相似度判分。
5. 实现关键词回退判分。
6. 保存每题判分详情。

### Phase 4：管理员后台

1. 实现统计面板。
2. 实现成绩列表。
3. 实现搜索、筛选和排序。
4. 实现试卷详情页。
5. 实现人工复核改分。
6. 实现重新计算总分。
7. 实现复核日志记录。

### Phase 5：导出与部署

1. 实现 xlsx 导出。
2. 实现 csv 回退导出。
3. 编写 README。
4. 编写 requirements.txt。
5. 提供本地启动命令。
6. 可选提供 Docker Compose。

---

## 18. 后续扩展方向

1. 管理员登录。
2. 多套试卷。
3. 题库管理页面。
4. 考试批次管理。
5. 员工名单导入。
6. 工号白名单校验。
7. 成绩排名和统计分析。
8. 主观题批量复核。
9. Embedding 阅卷队列异步化。
10. 多模型交叉评分。
11. 局域网长期服务部署。
12. 操作审计日志。

---

## 19. 当前最终决策

当前版本采用：

```text
FastAPI + SQLite + questions.json + 原生 HTML/JS + Embedding 语义相似度判分 + 关键词回退
```

最终确认：

1. 前端不使用 Vue / React。
2. 不使用 PySide6 作为员工端界面。
3. 员工通过手机扫码访问 Web 页面。
4. 管理员通过浏览器访问后台。
5. 管理员可以逐人复核完整试卷答案。
6. 考试时长、模型、阈值、题目和分值都通过配置或 JSON 文件灵活调整。
7. MVP 阶段管理员后台不启用鉴权，仅适用于可信局域网。
