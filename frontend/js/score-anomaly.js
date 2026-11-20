(() => {
  const API = '/api/admin';
  const PAGE_SIZE = 100;
  let runs = [];
  let batches = [];
  let currentBatch = null;
  let currentFindings = [];
  let offset = 0;
  let pollTimer = null;

  const $ = id => document.getElementById(id);
  const esc = value => {
    const node = document.createElement('span');
    node.textContent = value == null ? '' : String(value);
    return node.innerHTML;
  };

  async function request(path, options = {}) {
    const response = await window.adminAuthFetch(`${API}${path}`, options);
    const data = await response.json().catch(() => ({}));
    if (!response.ok) throw new Error(data?.detail?.message || data?.error?.message || data?.message || `请求失败（${response.status}）`);
    return data;
  }

  function selectedRun() {
    return runs.find(run => run.run_id === $('scoreAnomalyRunSelect')?.value) || null;
  }

  function stopPolling() {
    if (pollTimer) clearTimeout(pollTimer);
    pollTimer = null;
  }

  function schedulePoll(batchID) {
    stopPolling();
    pollTimer = setTimeout(async () => {
      if (!$('view-score-anomalies')?.classList.contains('is-active')) return;
      try {
        await loadBatch(batchID, offset);
      } catch (error) {
        $('scoreAnomalyBatchState').textContent = error.message;
      }
    }, 1400);
  }

  async function loadRuns() {
    const data = await request('/score-anomalies/runs');
    runs = Array.isArray(data.runs) ? data.runs : [];
    const select = $('scoreAnomalyRunSelect');
    const previous = select.value;
    const eligible = runs.filter(run => run.status === 'closed' && run.submission_count > 0);
    select.innerHTML = eligible.map(run => `<option value="${esc(run.run_id)}">${esc(run.paper_id)} · 第 ${esc(run.round_no)} 轮 · ${esc(run.submission_count)} 份提交</option>`).join('');
    const choice = eligible.find(run => run.run_id === previous) || eligible[0];
    if (!choice) {
      select.innerHTML = '<option value="">暂无可检测的已关闭轮次</option>';
      $('scoreAnomalyStartBtn').disabled = true;
      $('scoreAnomalyRunState').textContent = '需要先关闭考试、完成所有评分，并生成相同答案版本的相似性报告。';
      await loadBatches();
      return;
    }
    select.value = choice.run_id;
    $('scoreAnomalyStartBtn').disabled = choice.active_session_count > 0;
    $('scoreAnomalyRunState').textContent = choice.active_session_count > 0
      ? '该轮次仍有答题会话，暂不能生成评分质量报告。'
      : '报告绑定当前评分代次和已完成的相似性批次；重新评分后请生成新报告。';
    await loadBatches();
  }

  async function loadBatches() {
    const run = selectedRun();
    const select = $('scoreAnomalyBatchSelect');
    if (!run) {
      batches = [];
      currentBatch = null;
      select.innerHTML = '<option value="">选择考试轮次</option>';
      select.disabled = true;
      $('scoreAnomalyBatchSummary').innerHTML = '';
      $('scoreAnomalyFindings').innerHTML = '';
      $('scoreAnomalyBatchState').textContent = '选择已关闭轮次以查看检测记录。';
      stopPolling();
      return;
    }
    const oldID = select.value;
    const data = await request(`/score-anomalies/runs/${encodeURIComponent(run.run_id)}/batches`);
    batches = Array.isArray(data.batches) ? data.batches : [];
    select.disabled = batches.length === 0;
    const labels = { queued: '排队中', running: '检测中', completed: '已完成', failed: '未完成' };
    select.innerHTML = batches.map(batch => `<option value="${batch.id}">${esc(labels[batch.status] || batch.status)} · ${esc(batch.finding_count)} 条 · ${esc(batch.rule_version)}</option>`).join('');
    const preferred = batches.find(batch => String(batch.id) === oldID) || batches[0];
    if (preferred) {
      select.value = String(preferred.id);
      await loadBatch(preferred.id, 0);
    } else {
      currentBatch = null;
      stopPolling();
      $('scoreAnomalyBatchSummary').innerHTML = '';
      $('scoreAnomalyFindings').innerHTML = '';
      $('scoreAnomalyBatchState').textContent = '此轮次还没有评分异常批次。';
    }
  }

  async function loadBatch(batchID, pageOffset = 0) {
    if (!batchID) return;
    offset = Math.max(0, pageOffset);
    const data = await request(`/score-anomalies/batches/${encodeURIComponent(batchID)}?limit=${PAGE_SIZE}&offset=${offset}`);
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
      batch.finding_count === 0 ? '检测完成，未发现规则线索' : '检测完成，请核对评分质量线索';
    $('scoreAnomalyBatchState').textContent = state;
    $('scoreAnomalyBatchSummary').innerHTML = `
      <div class="similarity-metric"><span>状态</span><strong>${esc(batch.status)}</strong></div>
      <div class="similarity-metric"><span>提交</span><strong>${esc(batch.submission_count)}</strong></div>
      <div class="similarity-metric"><span>异常线索</span><strong>${esc(batch.finding_count)}</strong></div>
      <div class="similarity-metric"><span>阻断级候选</span><strong>${esc(batch.blocking_count)}</strong></div>
      <div class="similarity-metric"><span>规则版本</span><strong>${esc(batch.rule_version)}</strong></div>
      <div class="similarity-metric"><span>答案批次</span><strong><code>${esc(batch.similarity_batch_id)}</code></strong></div>`;
    const pages = Math.max(1, Math.ceil(batch.finding_count / PAGE_SIZE));
    const pageNumber = Math.floor(offset / PAGE_SIZE) + 1;
    const paging = $('scoreAnomalyPaging');
    paging.classList.toggle('is-hidden', batch.status !== 'completed' || batch.finding_count <= PAGE_SIZE);
    $('scoreAnomalyPageLabel').textContent = `第 ${pageNumber} / ${pages} 页 · ${batch.finding_count} 条线索`;
    $('scoreAnomalyPrevPage').disabled = offset === 0;
    $('scoreAnomalyNextPage').disabled = offset + PAGE_SIZE >= batch.finding_count;
    if (batch.status !== 'completed') {
      const note = batch.status === 'failed'
        ? '报告没有完成；失败状态不代表“未发现评分异常”。'
        : '报告尚未完成，完成前不会显示为“未发现异常”。';
      $('scoreAnomalyFindings').innerHTML = `<div class="similarity-empty">${esc(note)}</div>`;
      return;
    }
    if (!currentFindings.length) {
      $('scoreAnomalyFindings').innerHTML = '<div class="similarity-empty">当前规则未生成评分异常线索；这不构成真实模型质量认证。</div>';
      return;
    }
    $('scoreAnomalyFindings').innerHTML = currentFindings.map(renderFinding).join('');
  }

  function renderFinding(item) {
    const labels = {
      score_out_of_range: '评分越界', point_score_out_of_range: '评分点越界',
      credited_without_evidence: '得分缺少证据', unsupported_point_score: '矛盾点仍得分',
      point_total_mismatch: '评分点合计不一致', duplicate_scoring_point: '评分点重复',
      identical_answer_score_gap: '相同答案分差', similar_answer_score_gap: '相似答案分差',
      shared_error_score_disagreement: '共同错误处理不一致', regrade_score_drift: '重评分差',
      grading_history_missing: '评分历史缺失',
    };
    const severity = item.severity === 'blocking' ? '阻断级候选' : '需核查';
    const participants = (item.participant_names || []).map((name, index) => {
      const id = item.submission_ids?.[index] || '';
      return `<span>${esc(name)} <small>#${esc(id)}</small></span>`;
    }).join('、');
    const gap = Number(item.score_gap) > 0 ? ` · 归一化分差 ${esc(Math.round(item.score_gap * 100))}%` : '';
    const disposition = item.review_disposition || 'open';
    const history = (item.review_history || []).map(event => `<li>${esc(event.actor)} · ${esc(event.note)}</li>`).join('');
    return `<article class="score-anomaly-finding">
      <div class="score-anomaly-finding-head">
        <div><span class="score-anomaly-severity ${item.severity === 'blocking' ? 'is-blocking' : ''}">${esc(severity)}</span>
          <strong>${esc(labels[item.rule_code] || item.rule_code)}</strong>
          <small>${esc(item.question_id || '整卷')} · ${esc(item.rule_code)}${gap}</small></div>
        <span>${participants || '评分任务'}</span>
      </div>
      <p>${esc(item.message)}</p>
      <details><summary>评分代次与证据</summary><pre>${esc(JSON.stringify(item.evidence || {}, null, 2))}</pre></details>
      <div class="score-anomaly-review">
        <label>处理结论<select data-review-disposition="${esc(item.id)}">
          <option value="open" ${disposition === 'open' ? 'selected' : ''}>待处理</option>
          <option value="false_positive" ${disposition === 'false_positive' ? 'selected' : ''}>确认误报</option>
          <option value="manually_resolved" ${disposition === 'manually_resolved' ? 'selected' : ''}>人工核查完成</option>
        </select></label>
        <label>处理说明<textarea data-review-note="${esc(item.id)}" minlength="5" maxlength="2000" required>${esc(item.review_note || '')}</textarea></label>
        <button class="btn secondary" type="button" data-review-finding="${esc(item.id)}" data-review-revision="${esc(item.review_revision)}">保存处理</button>
      </div>
      ${history ? `<details class="score-anomaly-review-history"><summary>处理审计（${(item.review_history || []).length}）</summary><ol>${history}</ol></details>` : ''}
    </article>`;
  }

  async function reviewFinding(button) {
    const id = button.dataset.reviewFinding;
    const disposition = document.querySelector(`[data-review-disposition="${CSS.escape(id)}"]`)?.value;
    const note = document.querySelector(`[data-review-note="${CSS.escape(id)}"]`)?.value || '';
    button.disabled = true;
    try {
      await request(`/score-anomalies/findings/${encodeURIComponent(id)}/review`, {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ expected_revision: Number(button.dataset.reviewRevision), disposition, note }),
      });
      window.adminToast('评分异常处理已保存');
      await loadBatch(currentBatch?.id, offset);
    } catch (error) {
      window.adminToast(error.message);
    } finally {
      button.disabled = false;
    }
  }

  async function startBatch() {
    const run = selectedRun();
    if (!run) return;
    $('scoreAnomalyStartBtn').disabled = true;
    try {
      await request(`/score-anomalies/runs/${encodeURIComponent(run.run_id)}/batches`, { method: 'POST' });
      window.adminToast('评分异常检测任务已加入队列');
      await loadBatches();
    } catch (error) {
      window.adminToast(error.message);
    } finally {
      $('scoreAnomalyStartBtn').disabled = run.active_session_count > 0;
    }
  }

  async function focusFinding(batchID, findingID) {
    const detail = await request(`/score-anomalies/batches/${encodeURIComponent(batchID)}?limit=1&offset=0`);
    await loadRuns();
    $('scoreAnomalyRunSelect').value = detail.batch.run_id;
    await loadBatches();
    $('scoreAnomalyBatchSelect').value = String(batchID);
    const limit = PAGE_SIZE;
    for (let pageOffset = 0; pageOffset < Math.max(limit, Number(detail.batch.finding_count)); pageOffset += limit) {
      await loadBatch(batchID, pageOffset);
      const button = [...document.querySelectorAll('[data-review-finding]')]
        .find(item => item.dataset.reviewFinding === String(findingID));
      if (button) {
        button.scrollIntoView({ behavior: 'smooth', block: 'center' });
        return;
      }
    }
  }

  function bindEvents() {
    $('scoreAnomalyRefreshBtn')?.addEventListener('click', () => loadRuns().catch(error => window.adminToast(error.message)));
    $('scoreAnomalyStartBtn')?.addEventListener('click', startBatch);
    $('scoreAnomalyRunSelect')?.addEventListener('change', () => loadBatches().catch(error => window.adminToast(error.message)));
    $('scoreAnomalyBatchSelect')?.addEventListener('change', event => loadBatch(event.target.value).catch(error => window.adminToast(error.message)));
    $('scoreAnomalyPrevPage')?.addEventListener('click', () => loadBatch(currentBatch?.id, Math.max(0, offset - PAGE_SIZE)).catch(error => window.adminToast(error.message)));
    $('scoreAnomalyNextPage')?.addEventListener('click', () => loadBatch(currentBatch?.id, offset + PAGE_SIZE).catch(error => window.adminToast(error.message)));
    $('scoreAnomalyFindings')?.addEventListener('click', event => {
      const button = event.target.closest('[data-review-finding]');
      if (button) reviewFinding(button);
    });
  }

  bindEvents();
  window.ScoreAnomalyAdmin = { load: loadRuns, focusFinding };
})();
