function $(id) { return document.getElementById(id); }

let authToken = localStorage.getItem('admin_token') || '';
const id = new URLSearchParams(location.search).get('id');
let submission = null;

function esc(s) {
  if (s == null) return '';
  const div = document.createElement('div');
  div.textContent = String(s);
  return div.innerHTML;
}

function toast(msg) {
  const el = $('toast');
  el.textContent = msg;
  el.style.display = 'block';
  setTimeout(() => el.style.display = 'none', 2200);
}

function badge(s) {
  const cls = {
    reviewed: 'badge-pass',
    pending: 'badge-pending',
    low_confidence: 'badge-low',
    auto_scored: 'badge-pass',
    need_review: 'badge-low',
    high_confidence: 'badge-pass',
  }[s] || '';
  return `<span class="badge ${cls}">${esc(s || '')}</span>`;
}

async function authFetch(url, options = {}) {
  const headers = options.headers || {};
  if (authToken && authToken !== 'auth_disabled') {
    headers['Authorization'] = `Bearer ${authToken}`;
  }

  const res = await fetch(url, { ...options, headers });
  if (res.status === 401) {
    localStorage.removeItem('admin_token');
    authToken = '';
    toast('认证已过期，请重新登录');
    setTimeout(() => { location.href = '/admin'; }, 800);
    throw new Error('认证已过期，请重新登录');
  }
  return res;
}

function render() {
  $('summary').textContent = `${submission.name || ''} / ${submission.employee_id || ''} / 总分 ${submission.total_score ?? ''} / ${submission.review_status || ''}`;

  if (submission.review_status === 'grading') {
    $('detail').innerHTML = '<div class="card" style="text-align:center;padding:40px"><h2 style="color:#3b82f6">⏳ 评分进行中</h2><p style="color:#64748b;margin-top:10px">该试卷正在后台评分，请稍后刷新页面查看结果</p><button class="btn" onclick="location.reload()" style="margin-top:15px">刷新页面</button></div>';
    return;
  }

  const gradingDetail = Array.isArray(submission.grading_detail) ? submission.grading_detail : [];
  $('detail').innerHTML = gradingDetail.map(d => {
    const subjective = ['short_answer', 'essay'].includes(d.type);
    const questionId = d.question_id;
    const studentAnswer = Array.isArray(d.student_answer) ? d.student_answer.join(',') : (d.student_answer ?? '');
    const referenceAnswer = Array.isArray(d.reference_answer) ? d.reference_answer.join(',') : (d.reference_answer ?? '');

    return `<div class="card">
      <div class="header"><h3>${esc(questionId)}. ${esc(d.question)}</h3>${badge(d.review_status || d.grading_status)}</div>
      <p class="muted">类型：${esc(d.type)}，满分：${esc(d.max_score)}，机器分：${esc(d.score)}，最终分：<b>${esc(d.final_score ?? d.score)}</b>，方法：${esc(d.grading_method || '')}</p>
      <div class="answer-box"><b>学生答案：</b><br>${esc(studentAnswer)}</div>
      <div class="answer-box"><b>参考答案：</b><br>${esc(referenceAnswer)}</div>
      ${d.reason ? `<p><b>判分理由：</b>${esc(d.reason)}</p>` : ''}
      ${subjective ? `<div class="toolbar"><input type="text" id="score_${esc(questionId)}" value="${esc(d.final_score ?? d.score)}" style="max-width:120px"><input type="text" id="note_${esc(questionId)}" placeholder="复核备注" style="max-width:320px"><button class="btn review-btn" data-qid="${esc(questionId)}">保存复核</button></div>` : ''}
    </div>`;
  }).join('');
}

async function load() {
  if (!id) throw new Error('缺少提交 ID');
  const res = await authFetch('/api/admin/submissions/' + encodeURIComponent(id));
  if (!res.ok) throw new Error('详情加载失败');
  submission = await res.json();
  render();
}

async function review(qid) {
  const score = Number($('score_' + qid).value);
  const note = $('note_' + qid).value;
  const res = await authFetch('/api/admin/review', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ submission_id: Number(id), question_id: qid, new_score: score, note })
  });
  const data = await res.json();
  if (!res.ok || !data.success) { toast(data.detail?.message || data.message || '保存失败'); return; }
  toast('保存成功');
  await load();
}

$('detail').addEventListener('click', (e) => {
  const btn = e.target.closest('.review-btn');
  if (!btn) return;
  review(btn.dataset.qid).catch(err => toast(err.message));
});

$('regradeBtn').addEventListener('click', async () => {
  if (!confirm('确认重新机器判分？人工复核分将保留。')) return;
  const res = await authFetch('/api/admin/regrade/' + encodeURIComponent(id), { method: 'POST' });
  const data = await res.json();
  if (!res.ok || !data.success) { toast(data.detail?.message || data.message || '重新判分失败'); return; }
  toast('重新判分完成');
  await load();
});

load().catch(e => toast(e.message));
