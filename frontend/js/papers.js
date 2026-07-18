/** 管理端：多专业试卷列表与录入（挂到 window.PapersAdmin） */
(function () {
  const API = '/api/admin';
  const TYPE_LABEL = {
    single_choice: '单选',
    multiple_choice: '多选',
    true_false: '判断',
    short_answer: '简答',
    essay: '论述',
  };

  let papersCache = [];
  let editing = null; // { slug, name, exam_info, questions, status, editable }
  let editingQuestionIndex = -1; // -1 = new

  function $(id) { return document.getElementById(id); }
  function esc(s) {
    if (s == null) return '';
    const d = document.createElement('div');
    d.textContent = String(s);
    return d.innerHTML;
  }

  async function authFetch(url, options) {
    if (typeof window.adminAuthFetch === 'function') {
      return window.adminAuthFetch(url, options);
    }
    return fetch(url, options);
  }

  function toast(msg) {
    if (typeof window.adminToast === 'function') window.adminToast(msg);
    else alert(msg);
  }

  function apiError(data, fallback) {
    return data?.detail?.message || data?.message || fallback;
  }

  async function loadPapersList() {
    const resp = await authFetch(`${API}/papers`);
    const data = await resp.json().catch(() => []);
    if (!resp.ok) throw new Error(apiError(data, '加载专业列表失败'));
    papersCache = Array.isArray(data) ? data : [];
    renderPapersList();
    fillPaperSelects();
    return papersCache;
  }

  function fillPaperSelects() {
    const publishSel = $('publishPaperSelect');
    const filterSel = $('filterPaper');
    const opts = papersCache.map(p =>
      `<option value="${esc(p.slug)}">${esc(p.name)} (${esc(p.slug)})</option>`
    ).join('');
    if (publishSel) {
      const cur = publishSel.value;
      publishSel.innerHTML = `<option value="">请选择专业</option>${opts}`;
      if (cur) publishSel.value = cur;
    }
    if (filterSel) {
      const cur = filterSel.value;
      filterSel.innerHTML = `<option value="">全部专业</option>${opts}`;
      if (cur) filterSel.value = cur;
    }
  }

  function statusBadge(st) {
    if (st === 'open') return '<span class="badge badge-reviewed">开考中</span>';
    return '<span class="badge badge-pending">未开考</span>';
  }

  function renderPapersList() {
    const tbody = $('papersTbody');
    const state = $('papersState');
    if (!tbody) return;
    if (!papersCache.length) {
      tbody.innerHTML = '';
      if (state) state.textContent = '暂无专业试卷，请点击「新建专业」。';
      return;
    }
    if (state) state.textContent = '';
    tbody.innerHTML = papersCache.map(p => `<tr>
      <td>${esc(p.name)}</td>
      <td><code>${esc(p.slug)}</code></td>
      <td>${statusBadge(p.status)}</td>
      <td>${esc(p.question_count ?? 0)}</td>
      <td>${esc(p.total_score ?? 0)}</td>
      <td>${esc(p.submission_count ?? 0)}</td>
      <td class="toolbar">
        <button class="btn secondary paper-edit-btn" type="button" data-slug="${esc(p.slug)}">录入</button>
        <button class="btn secondary paper-pub-btn" type="button" data-slug="${esc(p.slug)}">发布</button>
        <button class="btn danger paper-del-btn" type="button" data-slug="${esc(p.slug)}">删除</button>
      </td>
    </tr>`).join('');
  }

  async function createPaper() {
    const name = prompt('专业名称（展示用）');
    if (!name || !name.trim()) return;
    let slug = prompt('专业编码（英文/数字，如 mech）', name.trim().toLowerCase().replace(/[^a-z0-9_-]+/g, '-').replace(/^-|-$/g, '').slice(0, 32));
    if (!slug) return;
    const resp = await authFetch(`${API}/papers`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ slug: slug.trim(), name: name.trim() }),
    });
    const data = await resp.json().catch(() => ({}));
    if (!resp.ok) throw new Error(apiError(data, '创建失败'));
    toast('已创建专业');
    await loadPapersList();
  }

  async function deletePaper(slug) {
    if (!confirm(`确认删除专业「${slug}」？有提交记录时无法删除。`)) return;
    const resp = await authFetch(`${API}/papers/${encodeURIComponent(slug)}`, { method: 'DELETE' });
    const data = await resp.json().catch(() => ({}));
    if (!resp.ok) throw new Error(apiError(data, '删除失败'));
    toast('已删除');
    await loadPapersList();
  }

  async function openEditor(slug) {
    const resp = await authFetch(`${API}/papers/${encodeURIComponent(slug)}`);
    const data = await resp.json().catch(() => ({}));
    if (!resp.ok) throw new Error(apiError(data, '加载试卷失败'));
    editing = {
      slug: data.paper_id || slug,
      name: data.name || '',
      exam_info: { ...(data.exam_info || {}) },
      questions: Array.isArray(data.questions) ? data.questions.map(q => ({ ...q })) : [],
      status: data.status,
      editable: data.editable !== false,
    };
    $('papersListPanel').classList.add('is-hidden');
    $('paperEditorPanel').classList.remove('is-hidden');
    $('paperEditorTitle').textContent = `编辑：${editing.name}`;
    $('paperEditorStatus').textContent = editing.editable
      ? '可编辑（考试未开放）'
      : '考试进行中，禁止改题 — 请先在「发布考试」结束考试';
    $('editPaperName').value = editing.name || '';
    $('editExamTitle').value = editing.exam_info.title || '';
    $('editExamDesc').value = editing.exam_info.description || '';
    $('editPassingScore').value = editing.exam_info.passing_score ?? 0;
    recomputeTotalUI();
    setEditorEnabled(editing.editable);
    renderQuestionList();
    hideQuestionForm();
  }

  function setEditorEnabled(on) {
    ['editPaperName', 'editExamTitle', 'editExamDesc', 'editPassingScore', 'savePaperBtn', 'addQuestionBtn']
      .forEach(id => { const el = $(id); if (el) el.disabled = !on; });
  }

  function backToList() {
    editing = null;
    $('paperEditorPanel').classList.add('is-hidden');
    $('papersListPanel').classList.remove('is-hidden');
    loadPapersList().catch(e => toast(e.message));
  }

  function recomputeTotalUI() {
    if (!editing) return;
    const total = editing.questions.reduce((s, q) => s + (Number(q.score) || 0), 0);
    editing.exam_info.total_score = total;
    $('editTotalScore').value = total;
  }

  function renderQuestionList() {
    const box = $('questionList');
    if (!editing || !box) return;
    if (!editing.questions.length) {
      box.innerHTML = '<p class="muted">暂无题目，请新增。</p>';
      return;
    }
    box.innerHTML = editing.questions.map((q, i) => {
      const mode = q.scoring_mode || (q.code_language ? (String(q.code_language).toLowerCase() === 'sql' ? 'sql' : 'code') : '');
      const subN = Array.isArray(q.sub_questions) ? q.sub_questions.length : 0;
      const subLabel = subN ? ` · 复合题 · ${subN} 子题` : '';
      return `<div class="question-editor-item">
        <div>
          <strong>${i + 1}. [${esc(TYPE_LABEL[q.type] || q.type)}] ${esc((q.question || '').slice(0, 80))}</strong>
          <div class="muted">${esc(q.id)} · ${esc(q.score)} 分${mode ? ' · ' + esc(mode) : ''}${subLabel}</div>
        </div>
        <div class="toolbar">
          <button class="btn secondary q-up" type="button" data-i="${i}" ${!editing.editable ? 'disabled' : ''}>上移</button>
          <button class="btn secondary q-down" type="button" data-i="${i}" ${!editing.editable ? 'disabled' : ''}>下移</button>
          <button class="btn secondary q-edit" type="button" data-i="${i}">编辑</button>
          <button class="btn danger q-del" type="button" data-i="${i}" ${!editing.editable ? 'disabled' : ''}>删除</button>
        </div>
      </div>`;
    }).join('');
  }

  function hideQuestionForm() {
    $('questionFormPanel').classList.add('is-hidden');
    editingQuestionIndex = -1;
  }

  function showQuestionForm(index) {
    if (!editing?.editable && index < 0) return;
    editingQuestionIndex = index;
    $('questionFormPanel').classList.remove('is-hidden');
    $('questionFormTitle').textContent = index < 0 ? '新增题目' : '编辑题目';
    const q = index < 0 ? {
      type: 'single_choice', score: 5, question: '', options: [
        { key: 'A', text: '' }, { key: 'B', text: '' },
      ], answer: 'A',
    } : { ...editing.questions[index], options: (editing.questions[index].options || []).map(o => ({ ...o })) };

    $('qType').value = q.type || 'single_choice';
    $('qScore').value = q.score ?? 5;
    $('qId').value = index < 0 ? '' : (q.id || '');
    $('qId').disabled = index >= 0;
    $('qStem').value = q.question || '';
    renderOptionsEditor(q);
    $('qTrueFalseAnswer').value = String(q.answer === true || q.answer === 'true');
    $('qAnswer').value = typeof q.answer === 'string' ? q.answer : '';
    $('qScoringMode').value = q.scoring_mode || 'text';
    $('qCodeLang').value = q.code_language || '';
    $('qRubric').value = q.scoring_rubric || '';
    renderPointsEditor(q.scoring_points || []);
    renderSubQuestionsEditor(q.sub_questions || []);
    onTypeChange();
    if (!editing.editable) {
      $('questionFormPanel').querySelectorAll('input,textarea,select,button').forEach(el => {
        if (el.id !== 'cancelQuestionBtn') el.disabled = true;
      });
    } else {
      $('questionFormPanel').querySelectorAll('input,textarea,select,button').forEach(el => {
        el.disabled = false;
      });
      if (index >= 0) $('qId').disabled = true;
    }
  }

  function onTypeChange() {
    const t = $('qType').value;
    const isObj = t === 'single_choice' || t === 'multiple_choice';
    const isTf = t === 'true_false';
    const isSub = t === 'short_answer' || t === 'essay';
    $('qOptionsBlock').classList.toggle('is-hidden', !isObj);
    $('qTrueFalseBlock').classList.toggle('is-hidden', !isTf);
    $('qSubjectiveBlock').classList.toggle('is-hidden', !isSub);
  }

  function renderOptionsEditor(q) {
    const list = $('qOptionsList');
    const opts = q.options || [];
    const multi = (q.type || $('qType').value) === 'multiple_choice';
    const ans = q.answer;
    list.innerHTML = opts.map((o, i) => {
      const checked = multi
        ? (Array.isArray(ans) && ans.map(String).includes(String(o.key)))
        : String(ans) === String(o.key);
      return `<div class="option-row">
        <input class="opt-key" data-i="${i}" value="${esc(o.key)}" style="width:3rem">
        <input class="opt-text" data-i="${i}" value="${esc(o.text)}" style="flex:1">
        <label><input type="${multi ? 'checkbox' : 'radio'}" name="optCorrect" data-i="${i}" ${checked ? 'checked' : ''}> 正确</label>
        <button class="btn danger opt-del" type="button" data-i="${i}">删</button>
      </div>`;
    }).join('');
  }

  function collectOptionsFromUI() {
    const rows = [...document.querySelectorAll('#qOptionsList .option-row')];
    const options = rows.map(row => ({
      key: row.querySelector('.opt-key').value.trim(),
      text: row.querySelector('.opt-text').value.trim(),
    }));
    const multi = $('qType').value === 'multiple_choice';
    const checks = [...document.querySelectorAll('#qOptionsList input[name="optCorrect"]')];
    let answer;
    if (multi) {
      answer = checks.filter(c => c.checked).map(c => options[Number(c.dataset.i)]?.key).filter(Boolean);
    } else {
      const c = checks.find(x => x.checked);
      answer = c ? options[Number(c.dataset.i)]?.key : (options[0]?.key || 'A');
    }
    return { options, answer };
  }

  function renderPointsEditor(points) {
    const list = $('qPointsList');
    list.innerHTML = (points.length ? points : []).map((p, i) => `
      <div class="option-row point-row">
        <input class="pt-text" data-i="${i}" value="${esc(p.text || '')}" placeholder="评分点" style="flex:1">
        <input class="pt-score" data-i="${i}" type="number" min="0" step="0.5" value="${esc(p.score ?? 0)}" style="width:5rem">
        <label><input class="pt-req" type="checkbox" data-i="${i}" ${p.required ? 'checked' : ''}> 必答</label>
        <button class="btn danger pt-del" type="button" data-i="${i}">删</button>
      </div>`).join('') || '<p class="muted">可为空；文本题建议填写评分点。</p>';
  }

  function collectPointsFromUI() {
    return [...document.querySelectorAll('#qPointsList .point-row')].map((row, i) => ({
      id: `p${i + 1}`,
      text: row.querySelector('.pt-text').value.trim(),
      score: Number(row.querySelector('.pt-score').value) || 0,
      required: row.querySelector('.pt-req').checked,
    })).filter(p => p.text);
  }


  function renderSubQuestionsEditor(subs) {
    const list = $('qSubQuestionsList');
    if (!list) return;
    const items = Array.isArray(subs) ? subs : [];
    list.innerHTML = items.map((s, i) => `
      <div class="sub-q-row panel nested-panel" data-i="${i}" style="margin:8px 0;padding:10px;border:1px solid #e5e7eb;border-radius:8px;">
        <div class="form-grid">
          <label>子题 ID <input class="sq-id" value="${esc(s.id || `s${i+1}`)}"></label>
          <label>分值 <input class="sq-score" type="number" min="0.5" step="0.5" value="${esc(s.score ?? 1)}"></label>
          <label>评分模式
            <select class="sq-mode">
              <option value="text" ${s.scoring_mode==='text'?'selected':''}>文本</option>
              <option value="sql" ${s.scoring_mode==='sql'?'selected':''}>SQL</option>
              <option value="code" ${s.scoring_mode==='code'?'selected':''}>代码</option>
              <option value="calculation" ${s.scoring_mode==='calculation'?'selected':''}>计算</option>
            </select>
          </label>
          <label>代码语言 <input class="sq-lang" value="${esc(s.code_language || '')}" placeholder="python / sql"></label>
        </div>
        <label class="full-label">子题题干 <textarea class="sq-stem" rows="2">${esc(s.question || '')}</textarea></label>
        <label class="full-label">子题参考答案 <textarea class="sq-answer code-answer" rows="3">${esc(s.answer || '')}</textarea></label>
        <button class="btn danger sq-del" type="button" data-i="${i}">删除子题</button>
      </div>
    `).join('') || '<p class="muted">无子题（单题模式）</p>';
  }

  function collectSubQuestionsFromUI() {
    return [...document.querySelectorAll('#qSubQuestionsList .sub-q-row')].map((row, i) => {
      const mode = row.querySelector('.sq-mode').value || 'text';
      const item = {
        id: row.querySelector('.sq-id').value.trim() || `s${i+1}`,
        question: row.querySelector('.sq-stem').value.trim(),
        answer: row.querySelector('.sq-answer').value,
        score: Number(row.querySelector('.sq-score').value) || 0,
        scoring_mode: mode,
      };
      const lang = row.querySelector('.sq-lang').value.trim();
      if (lang) item.code_language = lang;
      return item;
    }).filter(s => s.question);
  }

  function applyQuestionFromForm() {
    if (!editing?.editable) return;
    const type = $('qType').value;
    const score = Number($('qScore').value);
    const stem = $('qStem').value.trim();
    if (!stem) { toast('请填写题干'); return; }
    if (!(score > 0)) { toast('分值须大于 0'); return; }

    const q = {
      id: editingQuestionIndex >= 0 ? editing.questions[editingQuestionIndex].id : ($('qId').value.trim() || undefined),
      type,
      question: stem,
      score,
    };

    if (type === 'single_choice' || type === 'multiple_choice') {
      const { options, answer } = collectOptionsFromUI();
      if (!options.length || options.some(o => !o.key || !o.text)) {
        toast('请完善选项');
        return;
      }
      q.options = options;
      q.answer = answer;
    } else if (type === 'true_false') {
      q.answer = $('qTrueFalseAnswer').value === 'true';
    } else {
      const subs = collectSubQuestionsFromUI();
      if (subs.length) {
        const ids = new Set();
        for (const s of subs) {
          if (!s.answer || !String(s.answer).trim()) { toast(`子题 ${s.id} 需要参考答案`); return; }
          if (!(s.score > 0)) { toast(`子题 ${s.id} 分值须 > 0`); return; }
          if (ids.has(s.id)) { toast(`子题 ID 重复: ${s.id}`); return; }
          ids.add(s.id);
          if (s.scoring_mode === 'code' && !s.code_language) {
            toast(`子题 ${s.id} 代码模式必须填写语言`); return;
          }
        }
        q.sub_questions = subs;
        q.score = subs.reduce((a, s) => a + (Number(s.score) || 0), 0);
        $('qScore').value = q.score;
      } else {
        const ans = $('qAnswer').value;
        if (!ans || !ans.trim()) { toast('请填写参考答案'); return; }
        q.answer = ans;
        q.scoring_mode = $('qScoringMode').value || 'text';
        if (q.scoring_mode === 'code' && !$('qCodeLang').value.trim()) {
          toast('代码模式必须填写语言'); return;
        }
        const lang = $('qCodeLang').value.trim();
        if (lang) q.code_language = lang;
        const rubric = $('qRubric').value.trim();
        if (rubric) q.scoring_rubric = rubric;
        const points = collectPointsFromUI();
        if (points.length) q.scoring_points = points;
      }
    }

    if (editingQuestionIndex >= 0) {
      editing.questions[editingQuestionIndex] = q;
    } else {
      if (!q.id) {
        let n = 1;
        const ids = new Set(editing.questions.map(x => x.id));
        while (ids.has(`q${n}`)) n++;
        q.id = `q${n}`;
      }
      if (editing.questions.some(x => x.id === q.id)) {
        toast('题目 ID 已存在');
        return;
      }
      editing.questions.push(q);
    }
    recomputeTotalUI();
    renderQuestionList();
    hideQuestionForm();
  }

  async function savePaper() {
    if (!editing?.editable) { toast('考试进行中，禁止保存'); return; }
    if (!editing.questions.length) { toast('至少需要一道题'); return; }
    const payload = {
      name: $('editPaperName').value.trim() || editing.name,
      exam_info: {
        title: $('editExamTitle').value.trim() || payloadNameFallback(editing),
        description: $('editExamDesc').value.trim(),
        passing_score: Number($('editPassingScore').value) || 0,
        total_score: editing.questions.reduce((s, q) => s + (Number(q.score) || 0), 0),
      },
      questions: editing.questions,
    };
    const resp = await authFetch(`${API}/papers/${encodeURIComponent(editing.slug)}`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    });
    const data = await resp.json().catch(() => ({}));
    if (!resp.ok) throw new Error(apiError(data, '保存失败'));
    toast('试卷已保存');
    await openEditor(editing.slug);
  }

  function payloadNameFallback(ed) {
    return ed.exam_info?.title || ed.name || ed.slug;
  }

  // publish helpers
  async function loadPublishFor(slug) {
    if (!slug) {
      $('examLink').textContent = '请选择专业';
      $('examLink').href = '#';
      $('examQR').removeAttribute('src');
      $('publishStatus').value = '';
      return;
    }
    const resp = await authFetch(`${API}/papers/${encodeURIComponent(slug)}/exam-link`);
    const data = await resp.json().catch(() => ({}));
    if (!resp.ok) throw new Error(apiError(data, '获取链接失败'));
    $('examLink').textContent = data.url || '';
    $('examLink').href = data.url || '#';
    if (data.qr_base64) $('examQR').src = data.qr_base64;
    $('publishStatus').value = data.status === 'open' ? '开考中' : '未开考';
  }

  async function setPaperStatus(slug, open) {
    const path = open ? 'open' : 'close';
    const resp = await authFetch(`${API}/papers/${encodeURIComponent(slug)}/${path}`, { method: 'POST' });
    const data = await resp.json().catch(() => ({}));
    if (!resp.ok) throw new Error(apiError(data, open ? '开考失败' : '结束失败'));
    toast(open ? '已开考' : '已结束考试');
    await loadPapersList();
    await loadPublishFor(slug);
  }

  function bind() {
    const createBtn = $('createPaperBtn');
    if (createBtn) createBtn.addEventListener('click', () => createPaper().catch(e => toast(e.message)));
    const back = $('backToPapersBtn');
    if (back) back.addEventListener('click', backToList);
    const save = $('savePaperBtn');
    if (save) save.addEventListener('click', () => savePaper().catch(e => toast(e.message)));
    const addQ = $('addQuestionBtn');
    if (addQ) addQ.addEventListener('click', () => showQuestionForm(-1));
    const cancelQ = $('cancelQuestionBtn');
    if (cancelQ) cancelQ.addEventListener('click', hideQuestionForm);
    const applyQ = $('applyQuestionBtn');
    if (applyQ) applyQ.addEventListener('click', applyQuestionFromForm);

    const addSubBtn = $('addSubQuestionBtn');
    if (addSubBtn) addSubBtn.addEventListener('click', () => {
      const cur = collectSubQuestionsFromUI();
      cur.push({ id: `s${cur.length + 1}`, question: '', answer: '', score: 1, scoring_mode: 'text' });
      renderSubQuestionsEditor(cur);
    });
    const subList = $('qSubQuestionsList');
    if (subList) subList.addEventListener('click', (e) => {
      const btn = e.target.closest('.sq-del');
      if (!btn) return;
      const cur = collectSubQuestionsFromUI();
      const i = Number(btn.dataset.i);
      cur.splice(i, 1);
      renderSubQuestionsEditor(cur);
    });

    const qType = $('qType');
    if (qType) qType.addEventListener('change', () => {
      onTypeChange();
      // re-render options if switching multi
      const { options, answer } = collectOptionsFromUI();
      renderOptionsEditor({ type: qType.value, options: options.length ? options : [{ key: 'A', text: '' }, { key: 'B', text: '' }], answer });
    });
    const addOpt = $('addOptionBtn');
    if (addOpt) addOpt.addEventListener('click', () => {
      const { options, answer } = collectOptionsFromUI();
      const next = String.fromCharCode(65 + options.length);
      options.push({ key: next, text: '' });
      renderOptionsEditor({ type: $('qType').value, options, answer });
    });
    const addPt = $('addPointBtn');
    if (addPt) addPt.addEventListener('click', () => {
      const pts = collectPointsFromUI();
      pts.push({ text: '', score: 1, required: false });
      renderPointsEditor(pts);
    });

    const qList = $('questionList');
    if (qList) qList.addEventListener('click', e => {
      const t = e.target.closest('button');
      if (!t || !editing) return;
      const i = Number(t.dataset.i);
      if (t.classList.contains('q-edit')) showQuestionForm(i);
      if (t.classList.contains('q-del') && editing.editable) {
        if (!confirm('删除该题？')) return;
        editing.questions.splice(i, 1);
        recomputeTotalUI();
        renderQuestionList();
      }
      if (t.classList.contains('q-up') && editing.editable && i > 0) {
        [editing.questions[i - 1], editing.questions[i]] = [editing.questions[i], editing.questions[i - 1]];
        renderQuestionList();
      }
      if (t.classList.contains('q-down') && editing.editable && i < editing.questions.length - 1) {
        [editing.questions[i + 1], editing.questions[i]] = [editing.questions[i], editing.questions[i + 1]];
        renderQuestionList();
      }
    });

    const optList = $('qOptionsList');
    if (optList) optList.addEventListener('click', e => {
      const btn = e.target.closest('.opt-del');
      if (!btn) return;
      const { options, answer } = collectOptionsFromUI();
      options.splice(Number(btn.dataset.i), 1);
      renderOptionsEditor({ type: $('qType').value, options, answer });
    });
    const ptList = $('qPointsList');
    if (ptList) ptList.addEventListener('click', e => {
      const btn = e.target.closest('.pt-del');
      if (!btn) return;
      const pts = collectPointsFromUI();
      pts.splice(Number(btn.dataset.i), 1);
      renderPointsEditor(pts);
    });

    const papersTbody = $('papersTbody');
    if (papersTbody) papersTbody.addEventListener('click', e => {
      const btn = e.target.closest('button');
      if (!btn) return;
      const slug = btn.dataset.slug;
      if (btn.classList.contains('paper-edit-btn')) openEditor(slug).catch(err => toast(err.message));
      if (btn.classList.contains('paper-del-btn')) deletePaper(slug).catch(err => toast(err.message));
      if (btn.classList.contains('paper-pub-btn')) {
        if (typeof window.showView === 'function') window.showView('publish');
        const sel = $('publishPaperSelect');
        if (sel) {
          sel.value = slug;
          loadPublishFor(slug).catch(err => toast(err.message));
        }
      }
    });

    const pubSel = $('publishPaperSelect');
    if (pubSel) pubSel.addEventListener('change', () => {
      loadPublishFor(pubSel.value).catch(e => toast(e.message));
    });
    const openBtn = $('openExamBtn');
    if (openBtn) openBtn.addEventListener('click', () => {
      const slug = $('publishPaperSelect')?.value;
      if (!slug) return toast('请选择专业');
      setPaperStatus(slug, true).catch(e => toast(e.message));
    });
    const closeBtn = $('closeExamBtn');
    if (closeBtn) closeBtn.addEventListener('click', () => {
      const slug = $('publishPaperSelect')?.value;
      if (!slug) return toast('请选择专业');
      setPaperStatus(slug, false).catch(e => toast(e.message));
    });
  }

  window.PapersAdmin = {
    loadPapersList,
    loadPublishFor,
    fillPaperSelects,
    getPapers: () => papersCache,
  };

  bind();
})();
