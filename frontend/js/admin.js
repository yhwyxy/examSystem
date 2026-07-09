const API = '/api/admin';
let authToken = localStorage.getItem('admin_token') || '';

function $(id) { return document.getElementById(id); }

// HTML 转义函数（XSS 防护）
function esc(s) {
  if (s == null) return '';
  const div = document.createElement('div');
  div.textContent = String(s);
  return div.innerHTML;
}

// 时间格式化：ISO 字符串 → "YYYY-MM-DD HH:mm:ss"
function fmtTime(s) {
  if (!s) return '';
  try {
    const d = new Date(s);
    if (isNaN(d.getTime())) return esc(s);
    const pad = n => String(n).padStart(2, '0');
    return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
  } catch { return esc(s); }
}

function badge(s) {
  const cls = {
    reviewed: 'badge-pass', pending: 'badge-pending', low_confidence: 'badge-low',
    auto_scored: 'badge-pass', need_review: 'badge-low', high_confidence: 'badge-pass'
  }[s] || '';
  return `<span class="badge ${cls}">${esc(s)}</span>`;
}

function toast(msg) {
  const el = $('toast');
  el.textContent = msg;
  el.style.display = 'block';
  setTimeout(() => el.style.display = 'none', 2200);
}

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

// 加载统计
async function loadStats() {
  const s = await authFetch('/api/admin/stats').then(r => r.json());
  $('stats').innerHTML = [
    ['提交人数', s.submitted_count], ['平均分', s.avg_score], ['最高分', s.max_score],
    ['最低分', s.min_score], ['待复核', s.pending_review], ['低置信', s.low_confidence_count],
  ].map(([k, v]) => `<div class="stat"><div class="muted">${esc(k)}</div><h2>${esc(v)}</h2></div>`).join('');
}

const PAGE_SIZE = 50;
let currentPage = 1;

async function loadRows() {
  const params = new URLSearchParams();
  if ($('keyword').value.trim()) params.set('keyword', $('keyword').value.trim());
  if ($('status').value) params.set('review_status', $('status').value);
  params.set('limit', PAGE_SIZE);
  params.set('offset', (currentPage - 1) * PAGE_SIZE);
  const rows = await authFetch('/api/admin/submissions?' + params.toString()).then(r => r.json());
  $('tbody').innerHTML = rows.map(r => `<tr>
    <td><input type="checkbox" class="row-check" value="${esc(r.id)}"></td>
    <td>${esc(r.id)}</td><td>${esc(r.name)}</td><td>${esc(r.employee_id)}</td><td>${esc(r.department || '')}</td>
    <td>${esc(r.objective_score)}</td><td>${esc(r.subjective_score_final)}</td><td><b>${esc(r.total_score)}</b></td>
    <td>${badge(r.review_status)}</td><td>${fmtTime(r.submitted_at)}</td>
    <td>
      <a class="btn" href="/detail?id=${esc(r.id)}">复核</a>
      <button class="btn del-btn" data-id="${esc(r.id)}" style="background:#ef4444;margin-left:4px">删除</button>
    </td>
  </tr>`).join('');
  // 分页控件
  const hasMore = rows.length === PAGE_SIZE;
  $('pagination').innerHTML =
    (currentPage > 1 ? `<button class="btn secondary" onclick="changePage(-1)">上一页</button>` : '') +
    ` <span style="padding:0 10px">第 ${esc(currentPage)} 页</span>` +
    (hasMore ? `<button class="btn secondary" onclick="changePage(1)">下一页</button>` : '');
  // 重置全选框
  $('checkAll').checked = false;
}

function changePage(delta) {
  currentPage = Math.max(1, currentPage + delta);
  loadRows().catch(e => toast(e.message));
}

$('searchBtn').addEventListener('click', () => { currentPage = 1; loadRows().catch(e => toast(e.message)); });

// 全选/取消全选本页
$('checkAll').addEventListener('change', (e) => {
  document.querySelectorAll('.row-check').forEach(cb => cb.checked = e.target.checked);
});
$('selectAlBtn').addEventListener('click', () => {
  const boxes = document.querySelectorAll('.row-check');
  const all = [...boxes].every(cb => cb.checked);
  boxes.forEach(cb => cb.checked = !all);
  $('checkAll').checked = !all;
});

// 删除所选（批量）
$('deleteSelBtn').addEventListener('click', async () => {
  const ids = [...document.querySelectorAll('.row-check:checked')].map(cb => Number(cb.value));
  if (ids.length === 0) { toast('请先勾选要删除的记录'); return; }
  if (!confirm(`确认删除所选的 ${ids.length} 条记录？此操作不可恢复。`)) return;
  try {
    const resp = await authFetch('/api/admin/submissions', {
      method: 'DELETE',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ ids }),
    });
    const data = await resp.json();
    if (!resp.ok) throw new Error(data.detail?.message || '删除失败');
    toast(`已删除 ${data.deleted || 0} 条`);
    currentPage = 1;
    await Promise.all([loadStats(), loadRows()]);
  } catch (e) {
    if (e.message) toast(e.message);
  }
});

// 单条删除（事件委托）
$('tbody').addEventListener('click', async (e) => {
  const btn = e.target.closest('.del-btn');
  if (!btn) return;
  const id = Number(btn.dataset.id);
  if (!confirm(`确认删除记录 #${id}？此操作不可恢复。`)) return;
  try {
    const resp = await authFetch('/api/admin/submissions', {
      method: 'DELETE',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ ids: [id] }),
    });
    const data = await resp.json();
    if (!resp.ok) throw new Error(data.detail?.message || '删除失败');
    toast(`已删除记录 #${id}`);
    await Promise.all([loadStats(), loadRows()]);
  } catch (err) {
    if (err.message) toast(err.message);
  }
});

async function loadExamLink() {
  try {
    const data = await authFetch('/api/admin/exam-link').then(r => r.json());
    $('examLink').textContent = data.url;
    $('examLink').href = data.url;
    if (data.qr_base64) $('examQR').src = data.qr_base64;
  } catch (e) {
    $('examLink').textContent = '获取失败';
  }
}

function copyLink() {
  const url = $('examLink').textContent;
  navigator.clipboard.writeText(url).then(() => toast('链接已复制')).catch(() => toast('复制失败，请手动复制'));
}

// 导出功能
async function exportData() {
  try {
    const resp = await authFetch('/api/admin/export');
    if (resp.ok) {
      const blob = await resp.blob();
      const url = window.URL.createObjectURL(blob);
      const a = document.createElement('a');
      a.href = url;
      a.download = '考试成绩.xlsx';
      document.body.appendChild(a);
      a.click();
      document.body.removeChild(a);
      window.URL.revokeObjectURL(url);
      toast('导出成功');
    } else {
      toast('导出失败');
    }
  } catch (e) {
    toast('导出失败: ' + e.message);
  }
}

// 初次加载
checkAuth();
