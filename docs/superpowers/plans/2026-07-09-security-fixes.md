# 安全漏洞修复实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 修复4项安全漏洞：管理认证缺失、XSS注入、CORS过度开放

**Architecture:** 
- 后端：新增 `/api/admin/login` 端点 + Bearer Token 机制，所有 admin 路由挂载 `require_admin` 依赖
- 前端：增加登录面板和 `authFetch` 封装，所有动态数据用 `esc()` 转义
- CORS：从 config.yaml 读取具体域名列表

**Tech Stack:** Python/FastAPI, JavaScript/HTML

## Global Constraints

- 保持向后兼容：`admin.enable_auth: false` 时行为不变
- 密码使用 SHA-256 哈希存储（与现有 utils.py 一致）
- Token 有效期 24 小时，存内存（单实例足够）

---

### Task 1: 后端 - 添加 CORS 配置支持

**Files:**
- Modify: `backend/config.py:18-23`
- Modify: `config.yaml:1-3`

**Interfaces:**
- Produces: `ServerConfig.allow_origins: list[str]` 字段

- [ ] **Step 1: 修改 config.py 添加 CORS 配置字段**

```python
class ServerConfig(BaseModel):
    host: str = "0.0.0.0"
    port: int = 8000
    allow_origins: list[str] = Field(default_factory=lambda: ["http://127.0.0.1:8000", "http://localhost:8000"])
```

- [ ] **Step 2: 修改 config.yaml 添加 CORS 配置**

```yaml
server:
  host: "0.0.0.0"
  port: 8000
  allow_origins:
    - "http://127.0.0.1:8000"
    - "http://localhost:8000"
```

- [ ] **Step 3: 修改 main.py 使用配置的 CORS 源**

```python
# 替换第30-36行的CORS配置
cfg = get_config().server
app.add_middleware(
    CORSMiddleware,
    allow_origins=cfg.allow_origins,
    allow_credentials=True,
    allow_methods=["*"],
    allow_headers=["*"],
)
```

- [ ] **Step 4: 测试验证**

```bash
curl -H "Origin: http://malicious.com" -I http://localhost:8000/api/health
# 预期：响应头中无 Access-Control-Allow-Origin
```

- [ ] **Step 5: 提交**

```bash
git add backend/config.py config.yaml backend/main.py
git commit -m "fix(security): restrict CORS to configured origins"
```

---

### Task 2: 后端 - 添加管理认证机制

**Files:**
- Modify: `backend/main.py:1-20`
- Modify: `backend/main.py:234-250` (admin routes)

**Interfaces:**
- Produces: `require_admin(request: Request)` 依赖函数
- Produces: `POST /api/admin/login` 端点
- Produces: `_admin_tokens: dict[str, float]` 内存 Token 存储

- [ ] **Step 1: 添加认证相关导入和常量**

在 main.py 顶部添加：

```python
import hashlib
import secrets
from fastapi import Depends, Header

# Token 存储 {token: expire_timestamp}
_admin_tokens: dict[str, float] = {}
_TOKEN_TTL = 86400  # 24小时
```

- [ ] **Step 2: 添加 require_admin 依赖函数**

```python
async def require_admin(request: Request):
    """管理员认证依赖。enable_auth=false 时跳过验证。"""
    cfg = get_config().admin
    if not cfg.enable_auth:
        return
    
    auth = request.headers.get("authorization", "")
    if not auth.startswith("Bearer "):
        raise HTTPException(status_code=401, detail={"code": "UNAUTHORIZED", "message": "请先登录"})
    
    token = auth[7:]
    expire = _admin_tokens.get(token)
    if expire is None:
        raise HTTPException(status_code=401, detail={"code": "INVALID_TOKEN", "message": "Token 无效或已过期"})
    
    if time.time() > expire:
        _admin_tokens.pop(token, None)
        raise HTTPException(status_code=401, detail={"code": "TOKEN_EXPIRED", "message": "Token 已过期，请重新登录"})
```

- [ ] **Step 3: 添加登录端点**

```python
class LoginRequest(BaseModel):
    password: str

@app.post("/api/admin/login")
def admin_login(req: LoginRequest) -> dict[str, Any]:
    cfg = get_config().admin
    if not cfg.enable_auth:
        return {"success": True, "token": "auth_disabled", "message": "认证未启用"}
    
    if not cfg.password:
        raise HTTPException(status_code=500, detail={"code": "NO_PASSWORD", "message": "管理员密码未配置"})
    
    # 验证密码（支持明文和 SHA-256 哈希）
    pwd_hash = hashlib.sha256(req.password.encode()).hexdigest()
    if req.password != cfg.password and pwd_hash != cfg.password:
        raise HTTPException(status_code=401, detail={"code": "WRONG_PASSWORD", "message": "密码错误"})
    
    # 生成 Token
    token = secrets.token_urlsafe(32)
    _admin_tokens[token] = time.time() + _TOKEN_TTL
    return {"success": True, "token": token}
```

- [ ] **Step 4: 为所有 admin 路由添加认证依赖**

修改以下路由，添加 `dependencies=[Depends(require_admin)]`：
- `GET /api/admin/exam-link`
- `GET /api/admin/stats`
- `GET /api/admin/submissions`
- `GET /api/admin/submissions/{submission_id}`
- `POST /api/admin/review`
- `POST /api/admin/regrade/{submission_id}`
- `GET /api/admin/export`
- `POST /api/admin/reload-questions`
- `POST /api/admin/reload-config`

示例：
```python
@app.get("/api/admin/stats", dependencies=[Depends(require_admin)])
def admin_stats() -> dict[str, Any]:
    return database.get_stats()
```

- [ ] **Step 5: 测试验证**

```bash
# 启用认证后访问
curl http://localhost:8000/api/admin/stats
# 预期：401 Unauthorized

# 登录获取 Token
curl -X POST http://localhost:8000/api/admin/login -H "Content-Type: application/json" -d '{"password":"your_password"}'
# 预期：{"success":true,"token":"..."}

# 使用 Token 访问
curl -H "Authorization: Bearer <token>" http://localhost:8000/api/admin/stats
# 预期：200 OK
```

- [ ] **Step 6: 提交**

```bash
git add backend/main.py
git commit -m "feat(security): add admin authentication with Bearer Token"
```

---

### Task 3: 前端 - 添加登录面板和 authFetch

**Files:**
- Modify: `frontend/admin.html:1-50`
- Modify: `frontend/js/admin.js:1-10`

**Interfaces:**
- Produces: `authFetch(url, options)` 封装函数
- Produces: 登录面板 HTML

- [ ] **Step 1: 修改 admin.html 添加登录面板**

在 `<body>` 开头添加：

```html
<div id="loginPanel" class="container" style="display:none;max-width:400px;margin-top:100px">
  <div class="card">
    <h2 style="text-align:center;margin-bottom:20px">🔐 管理后台登录</h2>
    <input type="password" id="loginPassword" placeholder="请输入管理员密码" style="width:100%;margin-bottom:15px">
    <button class="btn" onclick="doLogin()" style="width:100%">登录</button>
    <p id="loginError" style="color:#ef4444;text-align:center;margin-top:10px;display:none"></p>
  </div>
</div>
<div id="mainContent" style="display:none">
  <!-- 原有内容 -->
</div>
```

- [ ] **Step 2: 修改 admin.js 添加认证逻辑**

在文件顶部添加：

```javascript
let authToken = localStorage.getItem('admin_token') || '';

// 认证封装
async function authFetch(url, options = {}) {
  const headers = options.headers || {};
  if (authToken && authToken !== 'auth_disabled') {
    headers['Authorization'] = `Bearer ${authToken}`;
  }
  const resp = await fetch(url, { ...options, headers });
  
  if (resp.status === 401) {
    localStorage.removeItem('admin_token');
    authToken = '';
    showLoginPanel();
    throw new Error('认证已过期，请重新登录');
  }
  
  return resp;
}

function showLoginPanel() {
  $('loginPanel').style.display = 'block';
  $('mainContent').style.display = 'none';
}

function showMainContent() {
  $('loginPanel').style.display = 'none';
  $('mainContent').style.display = 'block';
  // 加载数据
  Promise.all([loadStats(), loadRows(), loadExamLink()]).catch(e => toast(e.message));
}

async function doLogin() {
  const password = $('loginPassword').value;
  const errorEl = $('loginError');
  
  try {
    const resp = await fetch('/api/admin/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ password })
    });
    
    const data = await resp.json();
    
    if (!resp.ok) {
      errorEl.textContent = data.detail?.message || '登录失败';
      errorEl.style.display = 'block';
      return;
    }
    
    authToken = data.token;
    localStorage.setItem('admin_token', authToken);
    errorEl.style.display = 'none';
    showMainContent();
  } catch (e) {
    errorEl.textContent = '网络错误';
    errorEl.style.display = 'block';
  }
}

// 页面加载时检查认证状态
async function checkAuth() {
  if (!authToken) {
    showLoginPanel();
    return;
  }
  
  try {
    const resp = await authFetch('/api/admin/stats');
    if (resp.ok) {
      showMainContent();
    } else {
      showLoginPanel();
    }
  } catch (e) {
    showLoginPanel();
  }
}

checkAuth();
```

- [ ] **Step 3: 将所有 fetch 调用改为 authFetch**

```javascript
// loadStats
const s = await authFetch('/api/admin/stats').then(r => r.json());

// loadRows
const rows = await authFetch('/api/admin/submissions?' + params.toString()).then(r => r.json());

// loadExamLink
const data = await authFetch('/api/admin/exam-link').then(r => r.json());
```

- [ ] **Step 4: 测试验证**

1. 启用认证：`config.yaml` 中设置 `admin.enable_auth: true` 和 `admin.password: "test123"`
2. 访问 `/admin`，应显示登录面板
3. 输入错误密码，应显示错误提示
4. 输入正确密码，应进入管理后台
5. 刷新页面，应保持登录状态

- [ ] **Step 5: 提交**

```bash
git add frontend/admin.html frontend/js/admin.js
git commit -m "feat(security): add admin login panel and authFetch"
```

---

### Task 4: 前端 - XSS 防护

**Files:**
- Modify: `frontend/js/admin.js:1-60`

**Interfaces:**
- Produces: `esc(str)` 转义函数

- [ ] **Step 1: 添加 HTML 转义函数**

在 admin.js 顶部添加：

```javascript
function esc(s) {
  if (s == null) return '';
  const div = document.createElement('div');
  div.textContent = String(s);
  return div.innerHTML;
}
```

- [ ] **Step 2: 转义所有动态数据**

修改 `loadStats` 函数：
```javascript
$('stats').innerHTML = [
  ['提交人数', s.submitted_count], ['平均分', s.avg_score], ['最高分', s.max_score],
  ['最低分', s.min_score], ['待复核', s.pending_review], ['低置信', s.low_confidence_count],
].map(([k, v]) => `<div class="stat"><div class="muted">${esc(k)}</div><h2>${esc(v)}</h2></div>`).join('');
```

修改 `loadRows` 函数：
```javascript
$('tbody').innerHTML = rows.map(r => `<tr>
  <td>${esc(r.id)}</td><td>${esc(r.name)}</td><td>${esc(r.employee_id)}</td><td>${esc(r.department)}</td>
  <td>${esc(r.objective_score)}</td><td>${esc(r.subjective_score_final)}</td><td><b>${esc(r.total_score)}</b></td>
  <td>${badge(esc(r.review_status))}</td><td>${esc(r.submitted_at)}</td>
  <td><a class="btn" href="/detail?id=${esc(r.id)}">复核</a></td>
</tr>`).join('');
```

- [ ] **Step 3: 测试验证**

1. 在数据库中插入测试数据，name 字段包含 `<script>alert('XSS')</script>`
2. 访问管理后台，查看是否弹出警告框
3. 预期：脚本被转义为文本显示

- [ ] **Step 4: 提交**

```bash
git add frontend/js/admin.js
git commit -m "fix(security): escape all dynamic data to prevent XSS"
```

---

### Task 5: 集成测试与文档更新

**Files:**
- Modify: `README.md` (可选)

- [ ] **Step 1: 完整流程测试**

```bash
# 1. 测试 CORS
curl -H "Origin: http://malicious.com" -I http://localhost:8000/api/health

# 2. 测试认证（启用状态）
curl http://localhost:8000/api/admin/stats
# 预期：401

curl -X POST http://localhost:8000/api/admin/login -H "Content-Type: application/json" -d '{"password":"test"}'
# 预期：200 + token

curl -H "Authorization: Bearer <token>" http://localhost:8000/api/admin/stats
# 预期：200

# 3. 测试认证（禁用状态）
# 修改 config.yaml: admin.enable_auth: false
curl http://localhost:8000/api/admin/stats
# 预期：200（无需登录）
```

- [ ] **Step 2: 更新配置文档**

在 README.md 或 config.yaml 注释中添加：

```yaml
# 安全配置说明
admin:
  enable_auth: true        # 启用管理员认证
  password: "your_secure_password"  # 管理员密码（支持明文或 SHA-256 哈希）

server:
  allow_origins:           # CORS 允许的源
    - "http://your-domain.com"
    - "https://your-domain.com"
```

- [ ] **Step 3: 最终提交**

```bash
git add -A
git commit -m "docs: update security configuration documentation"
```

---

## 验收清单

- [ ] CORS 只允许配置的域名
- [ ] 未认证时访问 admin API 返回 401
- [ ] 登录端点正常工作
- [ ] Token 机制正常（24小时过期）
- [ ] 前端登录面板显示正常
- [ ] XSS 防护生效（脚本被转义）
- [ ] `enable_auth: false` 时行为与之前一致
