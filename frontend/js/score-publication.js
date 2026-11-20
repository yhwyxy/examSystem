(() => {
  const API = '/api/admin';
  let runs = [];
  let batches = [];
  let current = null;

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
    return runs.find(run => run.run_id === $('publicationRunSelect')?.value) || null;
  }

  async function load() {
    const data = await request('/publications/runs');
    runs = Array.isArray(data.runs) ? data.runs : [];
    const select = $('publicationRunSelect');
    const previous = select.value;
    const eligible = runs.filter(run => run.status === 'closed' && Number(run.submission_count) > 0);
    select.innerHTML = eligible.map(run => `<option value="${esc(run.run_id)}">${esc(run.paper_id)} · 第 ${esc(run.round_no)} 轮 · ${esc(run.submission_count)} 份提交</option>`).join('');
    const choice = eligible.find(run => run.run_id === previous) || eligible[0];
    if (!choice) {
      select.innerHTML = '<option value="">暂无可发布的已关闭轮次</option>';
      $('publicationPublishBtn').disabled = true;
      $('publicationRunState').textContent = '没有找到已关闭且含有提交的轮次。';
      await loadBatches();
      return;
    }
    select.value = choice.run_id;
    $('publicationPublishBtn').disabled = choice.active_session_count > 0;
    $('publicationRunState').textContent = choice.active_session_count > 0
      ? '该轮次仍有活动答题会话。'
      : '发布接口会再次核验当前评分、复核、检测和阻断候选状态。';
    await loadBatches();
  }

  async function loadBatches() {
    const run = selectedRun();
    const select = $('publicationBatchSelect');
    if (!run) {
      batches = [];
      current = null;
      select.innerHTML = '<option value="">选择考试轮次</option>';
      select.disabled = true;
      renderPublication(null);
      return;
    }
    const previous = select.value;
    const data = await request(`/publications/runs/${encodeURIComponent(run.run_id)}/batches`);
    batches = Array.isArray(data.batches) ? data.batches : [];
    select.disabled = batches.length === 0;
    select.innerHTML = batches.map(batch => `<option value="${batch.id}">版本 ${esc(batch.version)} · ${esc(formatTime(batch.published_at))} · ${esc(batch.submission_count)} 份</option>`).join('');
    const choice = batches.find(batch => String(batch.id) === previous) || batches[0];
    if (choice) {
      select.value = String(choice.id);
      await loadPublication(choice.id);
    } else {
      renderPublication(null);
      $('publicationBatchState').textContent = '此轮次尚无已发布成绩版本。';
    }
  }

  async function loadPublication(id) {
    if (!id) return;
    const data = await request(`/publications/batches/${encodeURIComponent(id)}`);
    current = data.publication;
    renderPublication(current);
  }

  function formatTime(value) {
    if (!value) return '—';
    const date = new Date(value);
    return Number.isNaN(date.getTime()) ? String(value) : date.toLocaleString();
  }

  function renderPublication(item) {
    const summary = $('publicationBatchSummary');
    const rows = $('publicationGradeRows');
    rows.innerHTML = '';
    if (!item?.batch) {
      summary.innerHTML = '';
      $('publicationBatchState').textContent = '选择轮次以查看已发布版本。';
      return;
    }
    const batch = item.batch;
    summary.innerHTML = `
      <div class="similarity-metric"><span>发布版本</span><strong>v${esc(batch.version)}</strong></div>
      <div class="similarity-metric"><span>人数</span><strong>${esc(batch.submission_count)}</strong></div>
      <div class="similarity-metric"><span>发布人 / 时间</span><strong>${esc(batch.actor)} · ${esc(formatTime(batch.published_at))}</strong></div>
      <div class="similarity-metric"><span>相似性 / 评分异常批次</span><strong>${esc(batch.similarity_batch_id)} / ${esc(batch.anomaly_batch_id)}</strong></div>
      ${batch.supersedes_batch_id ? `<div class="similarity-metric"><span>更正前版本</span><strong>#${esc(batch.supersedes_batch_id)} · ${esc(batch.correction_reason)}</strong></div>` : ''}`;
    rows.innerHTML = (item.grades || []).map(grade => `<tr>
      <td>${esc(grade.submission_id)}</td><td>${esc(grade.participant_name)}</td><td>${esc(grade.employee_id)}</td>
      <td>${esc(grade.review_status)}</td><td>${esc(formatScore(grade.subjective_machine_score))}</td>
      <td>${esc(formatScore(grade.subjective_final_score))}</td><td><strong>${esc(formatScore(grade.total_score))}</strong></td>
      <td>${esc(formatScore(grade.max_score))}</td><td>${esc(grade.grading_generation)}</td>
    </tr>`).join('');
    $('publicationBatchState').textContent = `版本 v${batch.version} 已固定；更正会创建新版本，历史发布记录保持不变。`;
  }

  function formatScore(value) {
    const number = Number(value);
    return Number.isFinite(number) ? number.toFixed(2) : '—';
  }

  async function publish() {
    const run = selectedRun();
    if (!run) return;
    const prior = batches[0];
    const reason = $('publicationCorrectionReason').value.trim();
    if (prior && reason.length < 5) {
      window.adminToast('更正已发布成绩需要填写至少 5 个字符的原因');
      return;
    }
    const button = $('publicationPublishBtn');
    button.disabled = true;
    try {
      const data = await request(`/publications/runs/${encodeURIComponent(run.run_id)}/batches`, {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ correction_reason: reason }),
      });
      $('publicationCorrectionReason').value = '';
      window.adminToast(data.created ? '成绩已发布为新版本' : '当前成绩版本已发布');
      await loadBatches();
    } catch (error) {
      $('publicationRunState').textContent = error.message;
      window.adminToast(error.message);
    } finally {
      button.disabled = Boolean(selectedRun()?.active_session_count > 0);
    }
  }

  function bindEvents() {
    $('publicationRefreshBtn')?.addEventListener('click', () => load().catch(error => window.adminToast(error.message)));
    $('publicationRunSelect')?.addEventListener('change', () => loadBatches().catch(error => window.adminToast(error.message)));
    $('publicationBatchSelect')?.addEventListener('change', event => loadPublication(event.target.value).catch(error => window.adminToast(error.message)));
    $('publicationPublishBtn')?.addEventListener('click', publish);
  }

  bindEvents();
  window.PublicationAdmin = { load };
})();
