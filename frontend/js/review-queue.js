(() => {
  const API = '/api/admin';
  const $ = id => document.getElementById(id);
  const esc = value => {
    const node = document.createElement('span');
    node.textContent = value == null ? '' : String(value);
    return node.innerHTML;
  };

  async function load() {
    const state = $('reviewQueueState');
    state.textContent = '待复核任务加载中…';
    const response = await window.adminAuthFetch(`${API}/review-queue?limit=500`);
    const data = await response.json().catch(() => ({}));
    if (!response.ok) throw new Error(data?.detail?.message || data?.error?.message || data?.message || '待复核任务加载失败');
    render(Array.isArray(data.tasks) ? data.tasks : []);
  }

  function render(tasks) {
    const labels = {
      submission: '评分复核',
      answer_similarity: '答案相似性核查',
      score_anomaly: '阻断异常处置',
    };
    const statusLabels = {
      low_confidence: '低置信', need_review: '待复核', partial_manual: '部分人工',
      reopened: '重新打开', rejected: '已退回复核', open: '待处理',
      needs_review: '需要进一步核查',
    };
    $('reviewQueueRows').innerHTML = tasks.map(task => {
      const round = task.round_no ? `第 ${task.round_no} 轮` : '历史轮次';
      const link = task.kind === 'submission'
        ? `<a class="btn secondary" href="/detail?id=${encodeURIComponent(task.submission_id)}">打开评分复核</a>`
        : task.kind === 'answer_similarity'
          ? `<button class="btn secondary" type="button" data-open-similarity data-batch-id="${esc(task.batch_id)}" data-finding-id="${esc(task.finding_id)}">查看并核查</button>`
          : `<button class="btn secondary" type="button" data-open-anomaly data-batch-id="${esc(task.batch_id)}" data-finding-id="${esc(task.finding_id)}">查看并处置</button>`;
      return `<tr>
        <td>${esc(labels[task.kind] || task.kind)}</td>
        <td>${esc(task.paper_id)} · ${esc(round)}</td>
        <td>${esc(task.participants)}</td>
        <td>${esc(statusLabels[task.status] || task.status)}${task.severity ? ` · ${esc(task.severity)}` : ''}</td>
        <td>${esc(task.description)}${task.question_id ? `<small class="muted"> · ${esc(task.question_id)}</small>` : ''}</td>
        <td>${link}</td>
      </tr>`;
    }).join('');
    $('reviewQueueState').textContent = tasks.length
      ? `当前有 ${tasks.length} 项必要人工处理任务。每项状态和审计保存在其对应的评分或检测记录中。`
      : '当前没有待复核评分或阻断级评分异常。';
  }

  $('reviewQueueRefreshBtn')?.addEventListener('click', () => load().catch(error => {
    $('reviewQueueState').textContent = error.message;
  }));
  $('reviewQueueRows')?.addEventListener('click', async event => {
    const similarity = event.target.closest('[data-open-similarity]');
    if (similarity) {
      window.showView('answer-similarity', { refresh: false });
      try {
        await window.AnswerSimilarityAdmin.focusFinding(similarity.dataset.batchId, similarity.dataset.findingId);
      } catch (error) {
        window.adminToast(error.message);
      }
      return;
    }
    const button = event.target.closest('[data-open-anomaly]');
    if (!button) return;
    window.showView('score-anomalies', { refresh: false });
    try {
      await window.ScoreAnomalyAdmin.focusFinding(button.dataset.batchId, button.dataset.findingId);
    } catch (error) {
      window.adminToast(error.message);
    }
  });

  window.ReviewQueueAdmin = { load };
})();
