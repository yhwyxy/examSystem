(() => {
  const API = '/api/admin/answer-similarity';
  const $ = id => document.getElementById(id);
  let runs = [];
  let batches = [];
  let currentBatch = null;
  let currentFindings = [];
  let findingOffset = 0;
  const FINDING_PAGE_SIZE = 100;
  let pollTimer = null;

  function esc(value) {
    const node = document.createElement('span');
    node.textContent = value == null ? '' : String(value);
    return node.innerHTML;
  }

  function fmtTime(value) {
    if (!value) return '—';
    const date = new Date(value);
    return Number.isNaN(date.getTime()) ? esc(value) : date.toLocaleString();
  }

  async function authFetch(url, options = {}) {
    const headers = { ...(options.headers || {}) };
    const token = localStorage.getItem('admin_token') || '';
    if (token && token !== 'auth_disabled') headers.Authorization = `Bearer ${token}`;
    const response = await fetch(url, { ...options, headers });
    if (response.status === 401) throw new Error('认证已过期，请重新登录');
    return response;
  }

  async function jsonRequest(url, options = {}) {
    const response = await authFetch(url, options);
    const data = await response.json().catch(() => ({}));
    if (!response.ok) {
      const message = data?.detail?.message || data?.error?.message || data?.message;
      throw new Error(message || `请求失败（${response.status}）`);
    }
    return data;
  }

  function selectedRun() {
    return runs.find(item => item.run_id === $('similarityRunSelect')?.value) || null;
  }

  function setRunOptions() {
    const select = $('similarityRunSelect');
    if (!select) return;
    const previous = select.value;
    const eligible = runs.filter(item => item.status === 'closed' && item.submission_count > 0);
    select.innerHTML = eligible.map(item => {
      const label = `${item.paper_id} · 第 ${item.round_no} 轮 · ${item.submission_count} 份提交`;
      return `<option value="${esc(item.run_id)}">${esc(label)}</option>`;
    }).join('');
    const choice = eligible.find(item => item.run_id === previous) || eligible[0];
    if (choice) select.value = choice.run_id;
    $('similarityStartBtn').disabled = !choice || choice.active_session_count > 0;
    if (!eligible.length) {
      select.innerHTML = '<option value="">暂无可检测的已关闭轮次</option>';
      $('similarityStartBtn').disabled = true;
      $('similarityRunState').textContent = '需要先关闭考试并完成所有会话的提交。';
    } else if (choice?.active_session_count > 0) {
      $('similarityRunState').textContent = '该轮次仍有答题中的会话，暂不能生成最终报告。';
    } else {
      $('similarityRunState').textContent = '检测按轮次快照和提交答案版本保存；重新运行会生成独立批次。';
    }
  }

  async function loadRuns() {
    $('similarityRunState').textContent = '正在读取已关闭轮次…';
    const data = await jsonRequest(`${API}/runs?limit=100`);
    runs = Array.isArray(data.runs) ? data.runs : [];
    setRunOptions();
    await loadBatches();
  }

  async function loadBatches() {
    const run = selectedRun();
    const select = $('similarityBatchSelect');
    if (!run) {
      select.innerHTML = '<option value="">选择考试轮次</option>';
      select.disabled = true;
      batches = [];
      currentBatch = null;
      $('similarityFindings').innerHTML = '';
      $('similarityBatchSummary').innerHTML = '';
      $('similarityBatchState').textContent = '选择已关闭轮次以查看检测记录。';
      stopPolling();
      return;
    }
    const oldID = select.value;
    const data = await jsonRequest(`${API}/runs/${encodeURIComponent(run.run_id)}/batches`);
    batches = Array.isArray(data.batches) ? data.batches : [];
    select.disabled = batches.length === 0;
    select.innerHTML = batches.map(batch => {
      const count = batch.status === 'completed' ? ` · ${batch.finding_count} 条线索` : '';
      const labels = { queued: '排队中', running: '检测中', completed: '已完成', failed: '未完成' };
      return `<option value="${batch.id}">${esc(labels[batch.status] || batch.status)} · ${esc(fmtTime(batch.created_at))}${esc(count)} · ${esc(batch.rule_version)}</option>`;
    }).join('');
    const preferred = batches.find(batch => String(batch.id) === oldID) || batches[0];
    if (preferred) {
      select.value = String(preferred.id);
      await loadBatch(preferred.id);
    } else {
      currentBatch = null;
      stopPolling();
      $('similarityBatchSummary').innerHTML = '';
      $('similarityFindings').innerHTML = '';
      $('similarityBatchState').textContent = '此轮次还没有检测批次。';
    }
  }

  async function loadBatch(batchID, offset = 0) {
    if (!batchID) return;
    findingOffset = Math.max(0, offset);
    const data = await jsonRequest(`${API}/batches/${encodeURIComponent(batchID)}?limit=${FINDING_PAGE_SIZE}&offset=${findingOffset}`);
    currentBatch = data.batch;
    currentFindings = Array.isArray(data.findings) ? data.findings : [];
    renderBatch(data);
    if (currentBatch.status === 'queued' || currentBatch.status === 'running') schedulePoll(batchID);
    else stopPolling();
  }

  function renderBatch(data) {
    const batch = data.batch;
    const state = batch.status === 'queued' ? '等待检测队列处理' :
      batch.status === 'running' ? '正在检测' :
      batch.status === 'failed' ? `检测未完成（${batch.error_code || 'PROCESSING_FAILED'}）` :
      batch.finding_count === 0 ? '检测完成，未发现可核查线索' : '检测完成，请人工查看线索';
    $('similarityBatchState').textContent = state;
    $('similarityBatchSummary').innerHTML = `
      <div class="similarity-metric"><span>状态</span><strong>${esc(batch.status)}</strong></div>
      <div class="similarity-metric"><span>提交</span><strong>${esc(batch.submission_count)}</strong></div>
      <div class="similarity-metric"><span>题目</span><strong>${esc(batch.question_count)}</strong></div>
      <div class="similarity-metric"><span>核查线索</span><strong>${esc(batch.finding_count)}</strong></div>
      <div class="similarity-metric"><span>规则版本</span><strong>${esc(batch.rule_version)}</strong></div>
      <div class="similarity-metric"><span>快照</span><strong><code>${esc(batch.snapshot_hash.slice(0, 12))}…</code></strong></div>`;
    const paging = $('similarityPaging');
    const pages = Math.max(1, Math.ceil(batch.finding_count / FINDING_PAGE_SIZE));
    const pageNumber = Math.floor(findingOffset / FINDING_PAGE_SIZE) + 1;
    paging.classList.toggle('is-hidden', batch.status !== 'completed' || batch.finding_count <= FINDING_PAGE_SIZE);
    $('similarityPageLabel').textContent = `第 ${pageNumber} / ${pages} 页 · ${batch.finding_count} 条线索`;
    $('similarityPrevPage').disabled = findingOffset === 0;
    $('similarityNextPage').disabled = findingOffset + FINDING_PAGE_SIZE >= batch.finding_count;
    if (batch.status !== 'completed') {
      const detail = batch.status === 'failed'
        ? '该批次没有形成完整报告。请检查数据或运行限制后重新检测；此状态不代表“未发现异常”。'
        : '报告尚未完成，完成前不会显示为“未发现异常”。';
      $('similarityFindings').innerHTML = `<div class="similarity-empty">${esc(detail)}</div>`;
      return;
    }
    if (batch.finding_count === 0) {
      $('similarityFindings').innerHTML = '<div class="similarity-empty">本规则版本没有生成核查线索。结果不代表对语义相似性的完整判定。</div>';
      return;
    }
    $('similarityFindings').innerHTML = currentFindings.map(renderFinding).join('');
  }

  function highlighted(value, excerpt) {
    const source = String(value || '（空答案）');
    const needle = String(excerpt || '');
    const position = needle ? source.indexOf(needle) : -1;
    if (position < 0) return esc(source);
    return `${esc(source.slice(0, position))}<mark>${esc(source.slice(position, position + needle.length))}</mark>${esc(source.slice(position + needle.length))}`;
  }

  function renderFinding(item) {
    const risk = item.risk_level === 'high' ? '高风险线索' : '中风险线索';
    const stateLabels = {
      normal_consistent: '正常一致',
      needs_review: '需要进一步核查',
      confirmed_abnormal: '确认异常',
    };
    const history = (item.review_history || []).map(event =>
      `<li>第 ${esc(event.revision)} 次处理：${esc(stateLabels[event.from_state] || event.from_state)} → ${esc(stateLabels[event.to_state] || event.to_state)} · ${esc(event.actor)} · ${esc(event.note)} · ${esc(fmtTime(event.created_at))}</li>`
    ).join('');
    const reasons = (item.reasons || []).map(reason => `<li>${esc(reason)}</li>`).join('');
    const related = (item.related_question_ids || []).map(id => `<code>${esc(id)}</code>`).join(' ');
    const commonErrors = (item.common_error_evidence || []).length
      ? `<p class="similarity-common-error">共同错误候选（待教师核验）：${item.common_error_evidence.map(value => `<code>${esc(value)}</code>`).join(' ')}</p>`
      : '';
    const semanticMeasure = Number(item.semantic_similarity) > 0
      ? ` · 语义接近 ${esc(Math.round(item.semantic_similarity * 100))}%`
      : '';
    const sharedExcerpt = item.shared_excerpt
      ? `<mark>${esc(item.shared_excerpt)}</mark>`
      : '<span class="muted">未找到可标注的字面重合片段</span>';
    return `<article class="similarity-finding" data-finding-id="${esc(item.id)}">
      <div class="similarity-finding-head">
        <div><span class="similarity-risk ${item.risk_level === 'high' ? 'is-high' : ''}">${esc(risk)}</span>
          <strong>${esc(item.question_label || item.question_id)}</strong>
          <small>风险 ${esc(Math.round(item.risk_score * 100))}% · 文本重合 ${esc(Math.round(item.text_overlap * 100))}%${semanticMeasure}</small></div>
        <span class="similarity-review-state">${esc(stateLabels[item.review_state] || item.review_state)}</span>
      </div>
      <div class="similarity-answer-pair">
        <section><h3>${esc(item.left_name)} <small>${esc(item.left_participant_id)}</small></h3><pre>${highlighted(item.left_answer, item.left_excerpt)}</pre></section>
        <section><h3>${esc(item.right_name)} <small>${esc(item.right_participant_id)}</small></h3><pre>${highlighted(item.right_answer, item.right_excerpt)}</pre></section>
      </div>
      <div class="similarity-evidence"><strong>共享片段</strong>${sharedExcerpt}
        <ul>${reasons}</ul>${commonErrors}${related ? `<p>同一考生对的关联题目：${related}</p>` : ''}</div>
      <details class="similarity-features"><summary>检测特征</summary><pre>${esc(JSON.stringify(item.features || {}, null, 2))}</pre></details>
      <div class="similarity-review" data-finding-id="${esc(item.id)}">
        <label>核查结论<select class="similarity-review-choice">
          <option value="normal_consistent" ${item.review_state === 'normal_consistent' ? 'selected' : ''}>正常一致</option>
          <option value="needs_review" ${item.review_state === 'needs_review' ? 'selected' : ''}>需要进一步核查</option>
          <option value="confirmed_abnormal" ${item.review_state === 'confirmed_abnormal' ? 'selected' : ''}>确认异常</option>
        </select></label>
        <label>处理说明<textarea class="similarity-review-note" maxlength="2000" placeholder="记录核查依据" required>${esc(item.review_note || '')}</textarea></label>
        <button class="btn secondary similarity-review-save" type="button">保存核查记录</button>
      </div>
      ${history ? `<details class="similarity-history"><summary>操作记录（${item.review_history.length}）</summary><ol>${history}</ol></details>` : ''}
    </article>`;
  }

  function schedulePoll(batchID) {
    stopPolling();
    pollTimer = setTimeout(async () => {
      try {
        if (!$('view-answer-similarity')?.classList.contains('is-active')) return;
        await loadBatch(batchID);
        if (currentBatch?.status === 'queued' || currentBatch?.status === 'running') {
          await loadBatches();
        }
      } catch (error) {
        $('similarityBatchState').textContent = error.message;
      }
    }, 1500);
  }

  function stopPolling() {
    if (pollTimer) clearTimeout(pollTimer);
    pollTimer = null;
  }

  async function startBatch() {
    const run = selectedRun();
    if (!run) return;
    $('similarityStartBtn').disabled = true;
    $('similarityRunState').textContent = '正在创建版本化检测批次…';
    try {
      const data = await jsonRequest(`${API}/runs/${encodeURIComponent(run.run_id)}/batches`, { method: 'POST' });
      await loadBatches();
      $('similarityBatchSelect').value = String(data.batch.id);
      await loadBatch(data.batch.id);
      $('similarityRunState').textContent = data.created ? '检测已排队。' : '相同快照和答案版本已有批次，已打开现有报告。';
    } finally {
      $('similarityStartBtn').disabled = run.active_session_count > 0;
    }
  }

  async function focusFinding(batchID, findingID) {
    const detail = await jsonRequest(`${API}/batches/${encodeURIComponent(batchID)}?limit=1&offset=0`);
    await loadRuns();
    $('similarityRunSelect').value = detail.batch.run_id;
    await loadBatches();
    $('similarityBatchSelect').value = String(batchID);
    for (let pageOffset = 0; pageOffset < Math.max(FINDING_PAGE_SIZE, Number(detail.batch.finding_count)); pageOffset += FINDING_PAGE_SIZE) {
      await loadBatch(batchID, pageOffset);
      const card = [...document.querySelectorAll('.similarity-review')]
        .find(item => item.dataset.findingId === String(findingID));
      if (card) {
        card.scrollIntoView({ behavior: 'smooth', block: 'center' });
        return;
      }
    }
  }

  async function saveReview(button) {
    const card = button.closest('.similarity-review');
    const findingID = card?.dataset.findingId;
    const finding = currentFindings.find(item => String(item.id) === findingID);
    const note = card?.querySelector('.similarity-review-note')?.value.trim() || '';
    const state = card?.querySelector('.similarity-review-choice')?.value || '';
    if (!finding || !note) {
      card?.querySelector('.similarity-review-note')?.focus();
      throw new Error('请填写处理说明后保存');
    }
    button.disabled = true;
    try {
      await jsonRequest(`${API}/findings/${encodeURIComponent(findingID)}/review`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ expected_revision: finding.review_revision, state, note }),
      });
      await loadBatch(currentBatch.id);
    } finally {
      button.disabled = false;
    }
  }

  function bind() {
    $('similarityRefreshBtn')?.addEventListener('click', () => loadRuns().catch(error => {
      $('similarityRunState').textContent = error.message;
    }));
    $('similarityRunSelect')?.addEventListener('change', () => loadBatches().catch(error => {
      $('similarityBatchState').textContent = error.message;
    }));
    $('similarityBatchSelect')?.addEventListener('change', event => loadBatch(event.target.value).catch(error => {
      $('similarityBatchState').textContent = error.message;
    }));
    $('similarityPrevPage')?.addEventListener('click', () => {
      if (currentBatch) loadBatch(currentBatch.id, findingOffset - FINDING_PAGE_SIZE).catch(error => {
        $('similarityBatchState').textContent = error.message;
      });
    });
    $('similarityNextPage')?.addEventListener('click', () => {
      if (currentBatch) loadBatch(currentBatch.id, findingOffset + FINDING_PAGE_SIZE).catch(error => {
        $('similarityBatchState').textContent = error.message;
      });
    });
    $('similarityStartBtn')?.addEventListener('click', () => startBatch().catch(error => {
      $('similarityRunState').textContent = error.message;
      const run = selectedRun();
      $('similarityStartBtn').disabled = !run || run.active_session_count > 0;
    }));
    $('similarityFindings')?.addEventListener('click', event => {
      const button = event.target.closest('.similarity-review-save');
      if (button) saveReview(button).catch(error => window.alert(error.message));
    });
  }

  bind();
  window.AnswerSimilarityAdmin = { load: loadRuns, focusFinding };
})();
