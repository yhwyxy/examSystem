/* 管理端独立题库：版本、审核状态、检索与 JSON 导入导出。 */
(function () {
  const API = '/api/admin/question-bank';
  const PAGE_SIZE = 50;
  const TYPE_LABEL = {
    single_choice: '单选', multiple_choice: '多选', true_false: '判断',
    short_answer: '简答', essay: '论述', composite: '复合',
  };
  const STATUS = {
    draft: ['草稿', 'draft'], reviewed: ['已审核', 'reviewed'],
    enabled: ['已启用', 'enabled'], disabled: ['已停用', 'disabled'],
    archived: ['已归档', 'archived'], superseded: ['已被新版本替代', 'superseded'],
  };

  let page = 0;
  let total = 0;
  let current = null;
  let currentExtraction = null;

  function $(id) { return document.getElementById(id); }
  function esc(value) {
    const node = document.createElement('span');
    node.textContent = value == null ? '' : String(value);
    return node.innerHTML.replaceAll('"', '&quot;').replaceAll("'", '&#39;');
  }
  function toast(message) {
    if (typeof window.adminToast === 'function') window.adminToast(message);
    else alert(message);
  }
  function filters() {
    const p = new URLSearchParams();
    const values = {
      discipline: $('questionBankDiscipline')?.value.trim(),
      knowledge_point: $('questionBankKnowledgePoint')?.value.trim(),
      difficulty: $('questionBankDifficulty')?.value,
      type: $('questionBankType')?.value,
      status: $('questionBankStatus')?.value,
      q: $('questionBankSearch')?.value.trim(),
    };
    Object.entries(values).forEach(([key, value]) => { if (value) p.set(key, value); });
    return p;
  }
  async function request(url, options = {}) {
    const fetcher = window.adminAuthFetch || fetch;
    const headers = { ...(options.headers || {}) };
    if (options.body && !headers['Content-Type']) headers['Content-Type'] = 'application/json';
    const response = await fetcher(url, { ...options, headers });
    const data = await response.json().catch(() => ({}));
    if (!response.ok) throw new Error(data?.detail?.message || data?.error?.message || '题库请求失败');
    return data;
  }

  function statusBadge(status) {
    const [label, className] = STATUS[status] || [status || '未知', 'draft'];
    return `<span class="qbank-status qbank-status-${className}">${esc(label)}</span>`;
  }
  function render(items) {
    const body = $('questionBankRows');
    if (!body) return;
    if (!items.length) {
      body.innerHTML = '<tr><td colspan="7" class="qbank-empty">没有符合筛选条件的题目</td></tr>';
      return;
    }
    body.innerHTML = items.map(item => {
      const stem = item.question?.question || '（未命名题目）';
      const points = (item.knowledge_points || []).map(x => `<span class="qbank-tag">${esc(x)}</span>`).join('');
      const type = TYPE_LABEL[item.question?.type] || item.question?.type || '未知题型';
      const score = Number(item.question?.score) || 0;
      let actions = `<button class="btn secondary qbank-action" data-action="edit" data-id="${esc(item.id)}">编辑</button>
        <button class="btn secondary qbank-action" data-action="versions" data-id="${esc(item.id)}">版本</button>`;
      if (item.status === 'draft') actions += ` <button class="btn qbank-action" data-version="${esc(item.version)}" data-action="review" data-id="${esc(item.id)}">人工审核通过</button>`;
      if (item.status === 'reviewed' || item.status === 'disabled') actions += ` <button class="btn qbank-action" data-version="${esc(item.version)}" data-action="enable" data-id="${esc(item.id)}">启用</button>`;
      if (item.status === 'enabled') actions += ` <button class="btn secondary qbank-action" data-version="${esc(item.version)}" data-action="disable" data-id="${esc(item.id)}">停用</button>`;
      if (['draft', 'reviewed', 'disabled'].includes(item.status)) actions += ` <button class="btn danger qbank-action" data-version="${esc(item.version)}" data-action="archive" data-id="${esc(item.id)}">归档</button>`;
      return `<tr>
        <td class="qbank-question-cell"><strong>${esc(stem)}</strong><code>${esc(item.id)}</code></td>
        <td><strong>${esc(item.discipline)}</strong><div class="qbank-tags">${points}</div></td>
        <td>${esc(({ easy: '基础', medium: '中等', hard: '进阶' })[item.difficulty] || item.difficulty)}</td>
        <td>${esc(type)}<small class="qbank-subline">${esc(score)} 分</small></td>
        <td><span class="qbank-version">v${esc(item.version)}</span></td>
        <td>${statusBadge(item.status)}</td>
        <td><div class="toolbar qbank-row-actions">${actions}</div></td>
      </tr>`;
    }).join('');
  }

  async function load() {
    const state = $('questionBankState');
    if (state) state.textContent = '正在读取题库…';
    const params = filters();
    params.set('limit', PAGE_SIZE);
    params.set('offset', page * PAGE_SIZE);
    const data = await request(`${API}?${params}`);
    total = Number(data.total) || 0;
    render(data.items || []);
    if ($('questionBankCount')) $('questionBankCount').textContent = `${total.toLocaleString()} 道题目`;
    if (state) state.textContent = total ? '' : '调整筛选条件，或新建一道题。';
    if ($('questionBankPage')) $('questionBankPage').textContent = `${page + 1} / ${Math.max(1, Math.ceil(total / PAGE_SIZE))}`;
    if ($('questionBankPrev')) $('questionBankPrev').disabled = page === 0;
    if ($('questionBankNext')) $('questionBankNext').disabled = (page + 1) * PAGE_SIZE >= total;
  }

  function defaultQuestion() {
    return {
      id: '', type: 'single_choice', question: '', score: 2,
      options: [{ key: 'A', text: '' }, { key: 'B', text: '' }], answer: 'A',
    };
  }
  function openEditor(item = null) {
    current = item;
    currentExtraction = null;
    $('questionBankEditorTitle').textContent = item ? `编辑 ${item.id} · v${item.version}` : '新建题目';
    $('questionBankEditorHint').textContent = item
      ? `${item.status === 'enabled' ? '当前版本已启用；保存后会生成新的草稿版本。' : '保存后会生成新的草稿版本。'} 当前内容不会被覆盖。`
      : '填写元数据与题目 JSON，保存后进入草稿状态。';
    $('questionBankEditID').value = item?.id || '';
    $('questionBankEditID').disabled = Boolean(item);
    $('questionBankEditDiscipline').value = item?.discipline || '';
    $('questionBankEditDifficulty').value = item?.difficulty || 'medium';
    $('questionBankEditPoints').value = (item?.knowledge_points || []).join(', ');
    $('questionBankEditJSON').value = JSON.stringify(item?.question || defaultQuestion(), null, 2);
    $('questionBankHistory').classList.add('is-hidden');
    $('questionBankHistory').innerHTML = '';
    $('questionBankCandidatePanel').classList.add('is-hidden');
    $('questionBankCandidateDiagnostics').textContent = '';
    $('questionBankCandidatePreview').textContent = '';
    $('questionBankApplyCandidatesBtn').disabled = true;
    $('questionBankHistoryBtn').disabled = !item;
    $('questionBankEditorModal').classList.remove('is-hidden');
    $('questionBankEditDiscipline').focus();
  }
  function closeEditor() {
    $('questionBankEditorModal').classList.add('is-hidden');
    current = null;
    currentExtraction = null;
  }
  async function extractScoringPointCandidates() {
    let question;
    try { question = JSON.parse($('questionBankEditJSON').value); }
    catch (error) { throw new Error(`题目 JSON 格式错误：${error.message}`); }
    if (!question || Array.isArray(question) || typeof question !== 'object') throw new Error('题目 JSON 必须是一个对象');
    const button = $('questionBankExtractPointsBtn');
    button.disabled = true;
    button.textContent = '正在生成候选…';
    try {
      const result = await request(`${API}/scoring-points/extract`, {
        method: 'POST',
        body: JSON.stringify({ question }),
      });
      currentExtraction = { result, questionFingerprint: JSON.stringify(question) };
      $('questionBankCandidateDiagnostics').textContent = (result.diagnostics || []).join(' ') ||
        '请逐项核对评分点及其来源摘录。候选内容不会自动生效。';
      $('questionBankCandidatePreview').textContent = JSON.stringify({
        candidates: result.candidates || [],
        diagnostics: result.diagnostics || [],
      }, null, 2);
      $('questionBankCandidatePanel').classList.remove('is-hidden');
      $('questionBankApplyCandidatesBtn').disabled = !(result.candidates || []).length;
    } finally {
      button.disabled = false;
      button.textContent = 'AI 起草评分点候选';
    }
  }
  function applyScoringPointCandidates() {
    if (!currentExtraction?.result?.candidates?.length) throw new Error('请先生成评分点候选');
    let question;
    try { question = JSON.parse($('questionBankEditJSON').value); }
    catch (error) { throw new Error(`题目 JSON 格式错误：${error.message}`); }
    if (JSON.stringify(question) !== currentExtraction.questionFingerprint) {
      throw new Error('题目内容在生成候选后发生了变化，请重新生成候选');
    }
    const points = currentExtraction.result.candidates.map(point => ({
      id: point.id,
      text: point.text,
      score: point.score,
      accepted_expressions: point.accepted_expressions || [],
      key_conditions: point.key_conditions || [],
      ...(point.source_evidence ? { source_evidence: point.source_evidence } : {}),
    }));
    if (question.scoring_points_by_language) {
      const language = question.code_language;
      if (!language) throw new Error('多语言评分题缺少 code_language，无法确定候选应填入的语言');
      if (!confirm(`这批候选将写入 ${language} 的评分点，并替换该语言现有评分点。请在保存前继续审核。确定继续吗？`)) return;
      question.scoring_points_by_language[language] = points;
    } else {
      const existing = question.scoring_points || [];
      if (existing.length && !confirm('这批候选将替换当前评分点。请在保存前继续审核。确定继续吗？')) return;
      question.scoring_points = points;
    }
    $('questionBankEditJSON').value = JSON.stringify(question, null, 2);
    toast('候选已填入未保存的题目 JSON；保存后仍是草稿，需人工审核并启用。');
  }
  async function saveEditor() {
    let question;
    try { question = JSON.parse($('questionBankEditJSON').value); }
    catch (error) { throw new Error(`题目 JSON 格式错误：${error.message}`); }
    if (!question || Array.isArray(question) || typeof question !== 'object') throw new Error('题目 JSON 必须是一个对象');
    const input = {
      id: current?.id || $('questionBankEditID').value.trim(),
      discipline: $('questionBankEditDiscipline').value.trim(),
      difficulty: $('questionBankEditDifficulty').value,
      knowledge_points: $('questionBankEditPoints').value.split(',').map(x => x.trim()).filter(Boolean),
      question,
    };
    const reason = prompt("请填写本次题目创建或修改的原因（最多 2000 字）：");
    if (reason === null) return;
    if (!reason.trim() || reason.trim().length > 2000) throw new Error("原因须为 1–2000 字");
    input.reason = reason.trim();
    const wasEditing = Boolean(current);
    let result;
    if (wasEditing) {
      input.expected_version = current.version;
      result = await request(`${API}/${encodeURIComponent(current.id)}`, { method: 'PUT', body: JSON.stringify(input) });
    } else {
      result = await request(API, { method: 'POST', body: JSON.stringify(input) });
    }
    closeEditor();
    toast(wasEditing ? `已保存为新草稿版本 v${result.version}` : `已创建草稿 ${result.id}`);
    await load();
  }

  async function showVersions(id) {
    const data = await request(`${API}/${encodeURIComponent(id)}/versions`);
    const history = $('questionBankHistory');
    history.innerHTML = `<h3>版本记录</h3>${(data.versions || []).map(item => `
      <details class="qbank-history-version" ${item.current ? 'open' : ''}>
        <summary><strong>v${esc(item.version)}</strong> ${statusBadge(item.status)} <span>${esc(new Date(item.created_at).toLocaleString())}</span></summary>
        <pre>${esc(JSON.stringify(item.question, null, 2))}</pre>
      </details>`).join('')}<h3>操作记录</h3>${(data.audit_events || []).length ? (data.audit_events || []).map(event => `<details><summary>v${esc(event.version)} · ${esc(event.action)} · ${esc(event.actor || '操作者未知')} · ${esc(new Date(event.created_at).toLocaleString())}</summary><p>${esc(event.reason || '未提供原因')}</p><pre>${esc(JSON.stringify({ before: event.before_state, after: event.after_state }, null, 2))}</pre></details>`).join('') : '<p>历史版本未记录操作者。</p>'}`;
    history.classList.remove('is-hidden');
    history.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
  }

  async function transition(id, action, version) {
    if (action === 'archive' && !confirm(`归档题目 ${id}？内容和版本记录会保留，归档项无法加入试卷。`)) return;
    const reason = prompt("请填写审核、启停或归档原因（审核代表确认题目与评分标准，最多 2000 字）：");
    if (reason === null) return;
    if (!reason.trim() || reason.trim().length > 2000) throw new Error("原因须为 1–2000 字");
    const item = await request(`${API}/${encodeURIComponent(id)}/${action}`, {
      method: action === 'archive' ? 'DELETE' : 'POST',
      body: JSON.stringify({ reason: reason.trim(), expected_version: Number(version) }),
    });
    toast(({ review: '已记录人工审核', enable: '题目已启用，可用于组卷', disable: '题目已停用', archive: '题目已归档' })[action] || '已更新');
    await load();
    if (current?.id === id) openEditor(item);
  }

  async function exportItems() {
    const fetcher = window.adminAuthFetch || fetch;
    const response = await fetcher(`${API}/export?${filters()}`);
    if (!response.ok) {
      const data = await response.json().catch(() => ({}));
      throw new Error(data?.detail?.message || '导出失败');
    }
    const blob = await response.blob();
    const url = URL.createObjectURL(blob);
    const anchor = document.createElement('a');
    anchor.href = url;
    anchor.download = 'question-bank.json';
    document.body.appendChild(anchor);
    anchor.click();
    anchor.remove();
    URL.revokeObjectURL(url);
    toast('已导出当前筛选结果');
  }

  async function importFile(file) {
    const parsed = JSON.parse(await file.text());
    if (!parsed || !Array.isArray(parsed.items)) throw new Error('文件格式应为题库导出的 {"items":[...]}');
    const reason = prompt("请填写本次题库导入的原因（最多 2000 字）：");
    if (reason === null) return;
    if (!reason.trim() || reason.trim().length > 2000) throw new Error("原因须为 1–2000 字");
    parsed.reason = reason.trim();
    const result = await request(`${API}/import`, { method: 'POST', body: JSON.stringify(parsed) });
    toast(`已导入 ${result.count} 道草稿题目`);
    page = 0;
    await load();
  }

  function bind() {
    $('questionBankSearchBtn')?.addEventListener('click', () => { page = 0; load().catch(e => toast(e.message)); });
    $('questionBankSearch')?.addEventListener('keydown', e => { if (e.key === 'Enter') { page = 0; load().catch(err => toast(err.message)); } });
    ['questionBankDiscipline', 'questionBankKnowledgePoint', 'questionBankDifficulty', 'questionBankType', 'questionBankStatus']
      .forEach(id => $(id)?.addEventListener('keydown', e => { if (e.key === 'Enter') { page = 0; load().catch(err => toast(err.message)); } }));
    $('questionBankPrev')?.addEventListener('click', () => { if (page > 0) { page--; load().catch(e => toast(e.message)); } });
    $('questionBankNext')?.addEventListener('click', () => { if ((page + 1) * PAGE_SIZE < total) { page++; load().catch(e => toast(e.message)); } });
    $('questionBankCreateBtn')?.addEventListener('click', () => openEditor());
    $('questionBankEditorClose')?.addEventListener('click', closeEditor);
    $('questionBankEditorCancel')?.addEventListener('click', closeEditor);
    $('questionBankEditorSave')?.addEventListener('click', () => saveEditor().catch(e => toast(e.message)));
    $('questionBankExtractPointsBtn')?.addEventListener('click', () => extractScoringPointCandidates().catch(e => toast(e.message)));
    $('questionBankApplyCandidatesBtn')?.addEventListener('click', () => {
      try { applyScoringPointCandidates(); } catch (e) { toast(e.message); }
    });
    $('questionBankHistoryBtn')?.addEventListener('click', () => current && showVersions(current.id).catch(e => toast(e.message)));
    $('questionBankRows')?.addEventListener('click', e => {
      const button = e.target.closest('.qbank-action');
      if (!button) return;
      const { id, action } = button.dataset;
      if (action === 'edit') request(`${API}/${encodeURIComponent(id)}`).then(openEditor).catch(err => toast(err.message));
      else if (action === 'versions') request(`${API}/${encodeURIComponent(id)}`).then(item => {
        openEditor(item);
        return showVersions(id);
      }).catch(err => toast(err.message));
      else transition(id, action, button.dataset.version).catch(err => toast(err.message));
    });
    $('questionBankImportBtn')?.addEventListener('click', () => $('questionBankFileInput')?.click());
    $('questionBankFileInput')?.addEventListener('change', e => {
      const file = e.target.files?.[0];
      if (file) importFile(file).catch(err => toast(err.message)).finally(() => { e.target.value = ''; });
    });
    $('questionBankExportBtn')?.addEventListener('click', () => exportItems().catch(e => toast(e.message)));
  }

  bind();
  window.QuestionBankAdmin = { load };
})();
