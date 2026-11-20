function $(id) { return document.getElementById(id); }

let authToken = localStorage.getItem('admin_token') || '';
const id = new URLSearchParams(location.search).get('id');
let submission = null;
let gradingHistory = [];
let reviewLogs = [];
let regradeEvents = [];

function esc(s) {
  if (s == null) return '';
  const div = document.createElement('div');
  div.textContent = String(s);
  return div.innerHTML.replaceAll('"', '&quot;').replaceAll("'", '&#39;');
}

function toast(msg) {
  const el = $('toast');
  el.textContent = msg;
  el.style.display = 'block';
  setTimeout(() => el.style.display = 'none', 2200);
}

// 分数显示: 保留两位小数, 多余位数直接舍弃 (非四舍五入).
// 先乘 100 截断再除回; 加 1e-9 修正浮点表示误差 (如 0.29*100=28.999... 会误截成 0.28).
function fmtScore(v) {
  if (v == null || v === '') return '';
  const n = Number(v);
  if (!Number.isFinite(n)) return String(v);
  return String(Math.trunc(n * 100 + 1e-9) / 100);
}

const REVIEW_STATUS_META = {
    reviewed: { className: 'badge-reviewed', label: '已复核' },
    pending: { className: 'badge-pending', label: '待处理' },
    low_confidence: { className: 'badge-low-confidence', label: '低置信' },
    auto_scored: { className: 'badge-auto-scored', label: '自动判分' },
    need_review: { className: 'badge-need-review', label: '待复核' },
    partial_manual: { className: 'badge-need-review', label: '部分人工' },
    high_confidence: { className: 'badge-high-confidence', label: '机器通过' },
    grading: { className: 'badge-grading', label: '评分中' },
    open_ended: { className: 'badge-open-ended', label: '开放题' },
};

function badge(s) {
  const meta = REVIEW_STATUS_META[s] || { className: 'badge-pending', label: s || '未知' };
  return `<span class="badge ${meta.className}">${esc(meta.label)}</span>`;
}

function renderEvidence(entry) {
  const detail = { ...(entry.detail || {}), ...entry };
  const matched = Array.isArray(detail.matched_points) ? detail.matched_points : [];
  const missed = Array.isArray(detail.missed_points) ? detail.missed_points : [];
  const points = [...matched.map(p => ({ ...p, status: '命中' })),
    ...missed.map(p => ({ ...p, status: '遗漏' }))];
  const relationLabels = { supported: '支持', contradicted: '矛盾', unknown: '证据不足' };
  const rows = points.map(p => `<tr>
    <td>${esc(p.description || p.text || p.point_id || '结构验证')}</td>
    <td>${esc(fmtScore(p.score))} / ${esc(fmtScore(p.max_score))}</td>
    <td>${esc(relationLabels[p.relation] || p.status)}</td>
    <td class="evidence-text">${esc(p.evidence_kind === 'execution_record' && p.validation
      ? `${p.validation.name || '测试用例'}：${p.validation.passed ? '验证通过' : '未通过'}`
      : p.evidence || '未提供')}</td>
    <td>${esc(p.reason || '')}</td>
  </tr>`).join('');
  const source = [detail.scoring_backend || detail.track, detail.scoring_model,
    detail.selected_language ? `语言 ${detail.selected_language}` : '',
    detail.grading_generation != null ? `评分代次 ${detail.grading_generation}` : '',
    detail.scoring_config_version ? `配置 ${detail.scoring_config_version.slice(0, 12)}` : '']
    .filter(Boolean).map(esc).join(' · ');
  const warningLabels = {
    missing_score_evidence: '评分结果缺少得分证据，需人工确认',
    duplicate_scoring_result_point: '评分结果存在重复评分点，需人工确认',
    scoring_point_conflict: '评分点与答案存在冲突，需人工确认',
    unsupported_point_score: '不确定或矛盾的评分点不自动计分，需人工确认',
  };
  return `<div class="grading-evidence">
    ${source ? `<p class="muted">${source}</p>` : ''}
    ${detail.fallback_reason ? `<p class="badge badge-need-review">评分已降级：${esc(detail.fallback_reason)}，需人工确认</p>` : ''}
    ${detail.provisional_score != null && detail.need_manual_review ? `<p>待复核估分：${esc(fmtScore(detail.provisional_score))}</p>` : ''}
    ${points.length ? `<div class="evidence-scroll"><table><thead><tr><th>评分点</th><th>得分 / 满分</th><th>判断</th><th>答案片段 / 验证依据</th><th>理由</th></tr></thead><tbody>${rows}</tbody></table></div>` : ''}
    ${(detail.warnings || []).length ? `<p class="muted">${detail.warnings.map(w => esc(warningLabels[w] || w)).join('；')}</p>` : ''}
  </div>`;
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
  const roundLabel = submission.run_is_legacy || submission.historical_grade
    ? '历史成绩（未按新流程发布）' : (submission.round_no != null ? `第${submission.round_no}轮` : '');
  const reviewLabel = REVIEW_STATUS_META[submission.review_status]?.label || '未知状态';
  $('summary').textContent = `${submission.name || ''} / ${submission.employee_id || ''} / ${roundLabel ? roundLabel + ' / ' : ''}总分 ${fmtScore(submission.total_score)} / ${reviewLabel}`;

  // 失败态优先于"评分中"判断: grading_status='failed' 时显示失败原因,
  // 提供重新机器判分入口 (页面顶部 regradeBtn 已常驻).
  if (submission.grading_status === 'failed') {
    $('detail').innerHTML = `<div class="panel result-panel"><h2>评分失败</h2><p class="muted form-note">该试卷自动评分多次尝试后失败${submission.grading_error ? `，错误原因：${esc(submission.grading_error)}` : ''}。可点击右上角「重新机器判分」重试，或联系管理员人工处理。</p><button class="btn" onclick="location.reload()">刷新页面</button></div>` + renderHistory() + renderAudit();
    return;
  }

  if (submission.review_status === 'grading' || ['pending', 'grading'].includes(submission.grading_status)) {
    $('detail').innerHTML = '<div class="panel result-panel"><h2>评分进行中</h2><p class="muted form-note">该试卷正在后台评分，请稍后刷新页面查看结果。</p><button class="btn" onclick="location.reload()">刷新页面</button></div>' + renderHistory() + renderAudit();
    return;
  }

  const gradingDetail = Array.isArray(submission.grading_detail) ? submission.grading_detail : [];
  $('detail').innerHTML = gradingDetail.map(d => {
    const subjective = !['single_choice', 'multiple_choice', 'true_false'].includes(d.type);
    const questionId = d.question_id;
    const isComposite = (d.is_composite || d.type === 'composite') && Array.isArray(d.sub_results);

    if (isComposite) {
      const subHtml = (d.sub_results || []).map((s, subIdx) => {
        const sid = s.sub_question_id;
        const language = s.selected_language
          ? `<span class="sub-result-language">语言：${esc(s.selected_language)}</span>`
          : '';
        return `<div class="sub-result card nested">
          <h4>${subIdx + 1}、${esc(s.question || '')}</h4>
          <p class="muted">满分 ${fmtScore(s.max_score)}，机器分 ${fmtScore(s.score)}，最终分 <b>${fmtScore(s.final_score ?? s.score)}</b> ${language} ${badge(s.review_status)}</p>
          <div class="answer-box"><b>学生：</b><br>${esc(s.student_answer ?? '')}</div>
          <div class="answer-box"><b>参考：</b><br>${esc(s.reference_answer ?? '')}</div>
          ${s.reason ? `<p><b>理由：</b>${esc(s.reason)}</p>` : ''}
          ${renderEvidence(s)}
          <div class="toolbar">
            <input class="score-input" type="number" min="0" max="${esc(s.max_score)}" step="any" value="${esc(fmtScore(s.final_score ?? s.score))}">
            <input class="note-input" type="text" placeholder="子题复核原因（必填）" maxlength="2000">
            <button class="btn review-btn" data-qid="${esc(questionId)}" data-sid="${esc(sid)}">保存子题复核</button>
          </div>
        </div>`;
      }).join('');

      return `<div class="card">
        <div class="header"><h3>${esc(questionId)}. ${esc(d.question)}</h3>${badge(d.review_status || d.grading_status)}</div>
        <p class="muted">复合题 · 满分：${fmtScore(d.max_score)}，机器分：${fmtScore(d.score)}，最终分：<b>${fmtScore(d.final_score ?? d.score)}</b></p>
        ${d.reason ? `<p><b>汇总：</b>${esc(d.reason)}</p>` : ''}
        ${subHtml}
      </div>`;
    }

    const studentAnswer = Array.isArray(d.student_answer)
      ? d.student_answer.join(',')
      : (typeof d.student_answer === 'object' && d.student_answer !== null
          ? JSON.stringify(d.student_answer)
          : (d.student_answer ?? ''));
    const referenceAnswer = Array.isArray(d.reference_answer)
      ? d.reference_answer.join(',')
      : (typeof d.reference_answer === 'object' && d.reference_answer !== null
          ? JSON.stringify(d.reference_answer)
          : (d.reference_answer ?? ''));

    return `<div class="card">
      <div class="header"><h3>${esc(questionId)}. ${esc(d.question)}</h3>${badge(d.review_status || d.grading_status)}</div>
      <p class="muted">类型：${esc(d.type)}，满分：${fmtScore(d.max_score)}，机器分：${fmtScore(d.score)}，最终分：<b>${fmtScore(d.final_score ?? d.score)}</b>，方法：${esc(d.grading_method || '')}</p>
      <div class="answer-box"><b>学生答案：</b><br>${esc(studentAnswer)}</div>
      <div class="answer-box"><b>参考答案：</b><br>${esc(referenceAnswer)}</div>
      ${d.reason ? `<p><b>判分理由：</b>${esc(d.reason)}</p>` : ''}
      ${subjective ? renderEvidence(d) : ''}
      ${subjective ? `<div class="toolbar"><input class="score-input" type="number" min="0" max="${esc(d.max_score)}" step="any" value="${esc(fmtScore(d.final_score ?? d.score))}"><input class="note-input" type="text" placeholder="复核原因（必填）" maxlength="2000"><button class="btn review-btn" data-qid="${esc(questionId)}">保存复核</button></div>` : ''}
    </div>`;
  }).join('') + renderHistory() + renderAudit();
}

function renderHistory() {
  return (gradingHistory.length ? `<details class="card"><summary>评分历史（${gradingHistory.length} 个版本）</summary>${gradingHistory.map(h =>
    `<details><summary>评分代次 ${esc(h.generation)} · 总分 ${esc(fmtScore(h.total_score))}</summary>${(h.grading_detail || []).map(d => `<h4>${esc(d.question_id)} · ${esc(d.question)}</h4><p>机器分 ${esc(fmtScore(d.machine_score))} · 当时最终分 ${esc(fmtScore(d.final_score))}</p>${renderEvidence(d)}`).join('')}</details>`
  ).join('')}</details>` : '');
}

function renderAudit() {
  if (!reviewLogs.length && !regradeEvents.length) return '';
  return `<details class="card"><summary>复核与重评审计</summary>${reviewLogs.map(log =>
    `<p>${esc(log.actor || '历史记录：未记录操作者')} · ${esc(log.question_id)}${log.sub_question_id ? ' / ' + esc(log.sub_question_id) : ''} · 代次 ${esc(log.grading_generation ?? '未记录')} · ${esc(fmtScore(log.old_score))} → ${esc(fmtScore(log.new_score))}<br>${esc(log.note)} · ${esc(log.created_at)}</p>`
  ).join('')}${regradeEvents.map(event =>
    `<p>${esc(event.actor)} · 重评 ${esc(event.from_generation)} → ${esc(event.to_generation)} · ${esc(event.reason)} · ${esc(event.created_at)}</p>`
  ).join('')}</details>`;
}

async function load() {
  if (!id) throw new Error('缺少提交 ID');
  const res = await authFetch('/api/admin/submissions/' + encodeURIComponent(id));
  if (!res.ok) throw new Error('详情加载失败');
  const payload = await res.json();
  // Go 后端返回 {submission:{...}, review_logs:[...]} 信封；兼容旧扁平形态。
  submission = payload.submission ?? payload;
  gradingHistory = Array.isArray(payload.grading_history) ? payload.grading_history : [];
  reviewLogs = Array.isArray(payload.review_logs) ? payload.review_logs : [];
  regradeEvents = Array.isArray(payload.regrade_events) ? payload.regrade_events : [];
  render();
}

async function review(qid, sid, resultContainer) {
  const scoreInput = resultContainer?.querySelector('.score-input');
  const noteInput = resultContainer?.querySelector('.note-input');
  if (!scoreInput || !noteInput) throw new Error('未找到复核输入框');
  const score = Number(scoreInput.value);
  if (!scoreInput.value.trim() || !Number.isFinite(score) || !scoreInput.checkValidity()) throw new Error('请填写范围内的复核分数');
  const note = noteInput.value.trim();
  if (!note || note.length > 2000) throw new Error('请填写1至2000字的复核原因');
  const body = { question_id: qid, new_score: score, note };
  if (sid) body.sub_question_id = sid;
  const res = await authFetch('/api/admin/submissions/' + encodeURIComponent(id) + '/review', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body)
  });
  const data = await res.json();
  if (!res.ok || !data.success) { toast(data.detail?.message || data.message || '保存失败'); return; }
  toast('保存成功');
  await load();
}

$('detail').addEventListener('click', (e) => {
  const btn = e.target.closest('.review-btn');
  if (!btn) return;
  const resultContainer = btn.closest('.sub-result') || btn.closest('.card');
  review(btn.dataset.qid, btn.dataset.sid || null, resultContainer).catch(err => toast(err.message));
});

$('regradeBtn').addEventListener('click', async () => {
  if (!confirm('确认重新机器判分？人工复核分将保留。')) return;
  const reason = prompt('请填写重新评分原因（必填）：')?.trim();
  if (!reason) return;
  if (reason.length > 2000) { toast('原因不能超过2000字'); return; }
  const res = await authFetch('/api/admin/regrade/' + encodeURIComponent(id), { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ reason }) });
  const data = await res.json();
  if (!res.ok || !data.success) { toast(data.detail?.message || data.message || '重新判分失败'); return; }
  toast('已加入重新判分队列');
  await load();
});

load().catch(e => toast(e.message));
