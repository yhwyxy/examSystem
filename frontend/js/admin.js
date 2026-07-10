const API = '/api/admin';
const PAGE_SIZE = 50;
const ADMIN_VIEWS = ['overview', 'publish', 'submissions'];

let authToken = localStorage.getItem('admin_token') || '';
let currentPage = 1;
let currentView = 'overview';

const STATUS_META = {
  reviewed: { className: 'badge-reviewed', label: '已复核' },
  pending: { className: 'badge-pending', label: '待处理' },
  low_confidence: { className: 'badge-low-confidence', label: '低置信' },
  auto_scored: { className: 'badge-auto-scored', label: '自动判分' },
  need_review: { className: 'badge-need-review', label: '待复核' },
  high_confidence: { className: 'badge-high-confidence', label: '高置信' },
  grading: { className: 'badge-grading', label: '评分中' },
};

function $(id) { return document.getElementById(id); }

function esc(s) {
  if (s == null) return '';
  const div = document.createElement('div');
  div.textContent = String(s);
  return div.innerHTML;
}

function fmtTime(s) {
  if (!s) return '';
  try {
    const d = new Date(s);
    if (Number.isNaN(d.getTime())) return esc(s);
    const pad = n => String(n).padStart(2, '0');
    return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
  } catch {
    return esc(s);
  }
}

function badge(s) {
  const meta = STATUS_META[s] || { className: 'badge-pending', label: s || '未知' };
  return `<span class="badge ${meta.className}">${esc(meta.label)}</span>`;
}

function toast(msg) {
  const el = $('toast');
  el.textContent = msg;
  el.style.display = 'block';
  setTimeout(() => { el.style.display = 'none'; }, 2200);
}

function apiErrorMessage(data, fallback) {
  const detail = data?.detail;
  if (detail?.code === 'ADMIN_PASSWORD_NOT_CONFIGURED') {
    return '管理员认证已开启但未配置密码，请在 config.yaml 设置 admin.password，或在本地开发关闭 admin.enable_auth。';
  }
  return detail?.message || data?.message || fallback;
}

function setTableState(message, visible = true) {
  const el = $('adminTableState');
  el.textContent = message;
  el.classList.toggle('is-hidden', !visible);
}

function showView(view, options = {}) {
  if (!ADMIN_VIEWS.includes(view)) view = 'overview';
  currentView = view;

  document.querySelectorAll('.view-panel').forEach(panel => {
    const active = panel.dataset.view === view;
    panel.classList.toggle('is-active', active);
    panel.hidden = !active;
  });

  document.querySelectorAll('.nav-item[data-view]').forEach(item => {
    item.classList.toggle('active', item.dataset.view === view);
  });

  const workspace = document.querySelector('.admin-shell .workspace');
  if (workspace) workspace.scrollTop = 0;
  window.scrollTo(0, 0);

  if (options.refresh === false) return;

  if (view === 'submissions') {
    loadRows().catch(e => toast(e.message));
  } else if (view === 'overview') {
    loadStats().catch(e => toast(e.message));
  } else if (view === 'publish') {
    loadExamLink().catch(e => toast(e.message));
  }
}

async function authFetch(url, options = {}) {
  const headers = { ...(options.headers || {}) };
  if (authToken && authToken !== 'auth_disabled') {
    headers.Authorization = `Bearer ${authToken}`;
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
  $('loginPanel').style.display = 'grid';
  $('mainContent').style.display = 'none';
}

function showMainContent() {
  $('loginPanel').style.display = 'none';
  $('mainContent').style.display = 'grid';
  showView(currentView || 'overview', { refresh: false });
  Promise.all([loadStats(), loadRows(), loadExamLink()]).catch(e => toast(e.message));
}

async function doLogin() {
  const password = $('loginPassword').value;
  const errorEl = $('loginError');
  errorEl.style.display = 'none';

  try {
    const resp = await fetch('/api/admin/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ password }),
    });
    const data = await resp.json().catch(() => ({}));

    if (!resp.ok) {
      errorEl.textContent = apiErrorMessage(data, '登录失败');
      errorEl.style.display = 'block';
      return;
    }

    authToken = data.token;
    localStorage.setItem('admin_token', authToken);
    showMainContent();
  } catch (e) {
    errorEl.textContent = e.message || '网络错误';
    errorEl.style.display = 'block';
  }
}

async function checkAuth() {
  try {
    const resp = await authFetch('/api/admin/stats');
    if (resp.ok) {
      showMainContent();
      return;
    }
    showLoginPanel();
  } catch {
    showLoginPanel();
  }
}

async function loadStats() {
  const resp = await authFetch('/api/admin/stats');
  const data = await resp.json().catch(() => ({}));
  if (!resp.ok) throw new Error(apiErrorMessage(data, '统计加载失败'));

  const metrics = [
    ['提交人数', data.submitted_count],
    ['平均分', data.avg_score],
    ['最高分', data.max_score],
    ['最低分', data.min_score],
    ['待复核', data.pending_review],
    ['低置信', data.low_confidence_count],
  ];

  $('stats').innerHTML = metrics.map(([label, value]) => `
    <div class="stat">
      <div class="muted">${esc(label)}</div>
      <h2>${esc(value ?? 0)}</h2>
    </div>
  `).join('');
}

function renderPagination(hasMore) {
  const pagination = $('pagination');
  pagination.textContent = '';

  if (currentPage > 1) {
    const prev = document.createElement('button');
    prev.className = 'btn secondary';
    prev.type = 'button';
    prev.textContent = '上一页';
    prev.addEventListener('click', () => changePage(-1));
    pagination.appendChild(prev);
  }

  const label = document.createElement('span');
  label.textContent = `第 ${currentPage} 页`;
  pagination.appendChild(label);

  if (hasMore) {
    const next = document.createElement('button');
    next.className = 'btn secondary';
    next.type = 'button';
    next.textContent = '下一页';
    next.addEventListener('click', () => changePage(1));
    pagination.appendChild(next);
  }
}

function renderRows(rows) {
  $('tbody').innerHTML = rows.map(r => `<tr>
    <td><input type="checkbox" class="row-check" value="${esc(r.id)}" aria-label="选择记录 ${esc(r.id)}"></td>
    <td>${esc(r.id)}</td>
    <td>${esc(r.name)}</td>
    <td>${esc(r.employee_id)}</td>
    <td>${esc(r.department || '')}</td>
    <td>${esc(r.objective_score)}</td>
    <td>${esc(r.subjective_score_final)}</td>
    <td><b>${esc(r.total_score)}</b></td>
    <td>${badge(r.review_status)}</td>
    <td>${fmtTime(r.submitted_at)}</td>
    <td class="toolbar">
      <a class="btn secondary" href="/detail?id=${encodeURIComponent(r.id)}">复核</a>
      <button class="btn danger del-btn" type="button" data-id="${esc(r.id)}">删除</button>
    </td>
  </tr>`).join('');
}

async function loadRows() {
  const params = new URLSearchParams();
  const keyword = $('keyword').value.trim();
  if (keyword) params.set('keyword', keyword);
  if ($('status').value) params.set('review_status', $('status').value);
  params.set('limit', PAGE_SIZE);
  params.set('offset', (currentPage - 1) * PAGE_SIZE);

  setTableState('正在加载提交记录...');
  $('tbody').textContent = '';

  try {
    const resp = await authFetch(`${API}/submissions?${params.toString()}`);
    const rows = await resp.json().catch(() => []);
    if (!resp.ok) throw new Error(apiErrorMessage(rows, '提交记录加载失败'));
    if (!Array.isArray(rows)) throw new Error('提交记录格式异常');

    renderRows(rows);
    const hasMore = rows.length === PAGE_SIZE;
    renderPagination(hasMore);
    $('checkAll').checked = false;

    if (rows.length === 0) setTableState('暂无符合条件的提交记录');
    else setTableState('', false);
  } catch (e) {
    renderPagination(false);
    setTableState(e.message || '提交记录加载失败');
    throw e;
  }
}

function changePage(delta) {
  currentPage = Math.max(1, currentPage + delta);
  loadRows().catch(e => toast(e.message));
}

async function deleteSubmissions(ids) {
  const resp = await authFetch(`${API}/submissions`, {
    method: 'DELETE',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ ids }),
  });
  const data = await resp.json().catch(() => ({}));
  if (!resp.ok) throw new Error(apiErrorMessage(data, '删除失败'));
  return data.deleted || 0;
}

async function loadExamLink() {
  try {
    const resp = await authFetch(`${API}/exam-link`);
    const data = await resp.json().catch(() => ({}));
    if (!resp.ok) throw new Error(apiErrorMessage(data, '获取考试链接失败'));
    $('examLink').textContent = data.url || '考试链接未生成';
    $('examLink').href = data.url || '#';
    if (data.qr_base64) $('examQR').src = data.qr_base64;
  } catch {
    $('examLink').textContent = '获取失败';
    $('examLink').href = '#';
  }
}

function copyLink() {
  const url = $('examLink').textContent;
  if (!url || url === '获取失败') {
    toast('考试链接不可复制');
    return;
  }
  navigator.clipboard.writeText(url)
    .then(() => toast('链接已复制'))
    .catch(() => toast('复制失败，请手动复制'));
}

async function exportData() {
  try {
    const resp = await authFetch(`${API}/export`);
    if (!resp.ok) {
      const data = await resp.json().catch(() => ({}));
      throw new Error(apiErrorMessage(data, '导出失败'));
    }

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
  } catch (e) {
    toast(e.message || '导出失败');
  }
}

function bindEvents() {
  $('loginBtn').addEventListener('click', doLogin);
  $('loginPassword').addEventListener('keydown', e => {
    if (e.key === 'Enter') doLogin();
  });
  $('searchBtn').addEventListener('click', () => {
    currentPage = 1;
    loadRows().catch(e => toast(e.message));
  });
  $('keyword').addEventListener('keydown', e => {
    if (e.key === 'Enter') {
      currentPage = 1;
      loadRows().catch(err => toast(err.message));
    }
  });
  $('exportBtn').addEventListener('click', exportData);
  $('copyLinkBtn').addEventListener('click', copyLink);

  $('checkAll').addEventListener('change', e => {
    document.querySelectorAll('.row-check').forEach(cb => { cb.checked = e.target.checked; });
  });
  $('selectAllBtn').addEventListener('click', () => {
    const boxes = document.querySelectorAll('.row-check');
    const all = [...boxes].length > 0 && [...boxes].every(cb => cb.checked);
    boxes.forEach(cb => { cb.checked = !all; });
    $('checkAll').checked = !all;
  });
  $('deleteSelBtn').addEventListener('click', async () => {
    const ids = [...document.querySelectorAll('.row-check:checked')].map(cb => Number(cb.value));
    if (ids.length === 0) {
      toast('请先勾选要删除的记录');
      return;
    }
    if (!confirm(`确认删除所选的 ${ids.length} 条记录？此操作不可恢复。`)) return;

    try {
      const deleted = await deleteSubmissions(ids);
      toast(`已删除 ${deleted} 条`);
      currentPage = 1;
      await Promise.all([loadStats(), loadRows()]);
    } catch (e) {
      toast(e.message || '删除失败');
    }
  });
  $('tbody').addEventListener('click', async e => {
    const btn = e.target.closest('.del-btn');
    if (!btn) return;

    const id = Number(btn.dataset.id);
    if (!confirm(`确认删除记录 #${id}？此操作不可恢复。`)) return;

    try {
      await deleteSubmissions([id]);
      toast(`已删除记录 #${id}`);
      await Promise.all([loadStats(), loadRows()]);
    } catch (err) {
      toast(err.message || '删除失败');
    }
  });

  document.querySelectorAll('.nav-item[data-view]').forEach(item => {
    item.addEventListener('click', e => {
      e.preventDefault();
      showView(item.dataset.view);
    });
  });
  document.querySelectorAll('[data-view-jump]').forEach(item => {
    item.addEventListener('click', e => {
      e.preventDefault();
      showView(item.dataset.viewJump);
    });
  });
  const exportSub = $('exportBtnSub');
  if (exportSub) exportSub.addEventListener('click', exportData);
}

bindEvents();
checkAuth();
