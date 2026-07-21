const AUTO_SUBMIT_AFTER_BLURS = 3;
const AWAY_TIMEOUT_MS = 30_000;

// 预览模式检测
const urlParams = new URLSearchParams(window.location.search);
const isPreview = urlParams.get('preview') === 'true';

const state = {
  exam: null,
  paperId: null,
  startedAt: null,
  durationSeconds: 0,
  timerId: null,
  submitting: false,
  blurCount: 0,
  isPageAway: false,
  awayTimeoutId: null,
  autoSubmitStarted: false,
  antiSwitchSetup: false,
  showAnswers: true, // 预览模式下默认显示答案
};

function $(id) { return document.getElementById(id); }

function toast(msg) {
  const el = $('toast');
  el.textContent = msg;
  el.style.display = 'block';
  setTimeout(() => { el.style.display = 'none'; }, 2600);
}

function pad(n) { return String(n).padStart(2, '0'); }

function getPaperIdFromUrl() {
  const params = new URLSearchParams(window.location.search);
  const p = (params.get('paper') || '').trim();
  return p || null;
}

function findQuestionControls(qid) {
  return [...document.querySelectorAll('[name]')].filter(
    control => control.name === String(qid)
  );
}

function findSubquestionControl(tagName, qid, sid) {
  return [...document.querySelectorAll(`${tagName}[data-qid][data-sid]`)].find(
    control => control.dataset.qid === String(qid) && control.dataset.sid === String(sid)
  ) || null;
}

function showFatal(message) {
  $('title').textContent = '无法进入考试';
  $('desc').textContent = message;
  const login = $('login');
  if (login) login.style.display = 'none';
  const form = $('examForm');
  if (form) form.style.display = 'none';
}

async function loadExam() {
  state.paperId = getPaperIdFromUrl();
  if (!state.paperId) {
    showFatal('请使用管理员发放的专业考试链接（地址中需包含 paper 参数，例如 /exam?paper=mech）。');
    throw new Error('缺少 paper 参数');
  }

  let res;
  if (isPreview) {
    // 预览模式：使用管理员预览API
    res = await fetch(`/api/admin/papers/${encodeURIComponent(state.paperId)}/preview`, {
      headers: {
        'Authorization': `Bearer ${localStorage.getItem('token')}`
      }
    });
  } else {
    // 正常模式：使用考试API
    res = await fetch(`/api/exam?paper=${encodeURIComponent(state.paperId)}`);
  }

  const errBody = await res.json().catch(() => ({}));
  if (!res.ok) {
    const msg = errBody.detail?.message || errBody.message || '考试加载失败';
    showFatal(msg);
    throw new Error(msg);
  }

  state.exam = errBody;
  state.paperId = state.exam.paper_id || state.paperId;
  $('title').textContent = state.exam.exam_info?.title || state.exam.paper_name || '企业考试';
  const descParts = [];
  if (state.exam.paper_name) descParts.push(`专业：${state.exam.paper_name}`);
  if (state.exam.exam_info?.description) descParts.push(state.exam.exam_info.description);
  $('desc').textContent = descParts.join(' · ');
  state.durationSeconds = (state.exam.config?.duration_minutes || state.exam.duration_minutes || 60) * 60;

  // 预览模式：直接显示试卷，跳过登录
  if (isPreview) {
    setupPreviewMode();
  }
}

function createAnswerOption(q, opt, inputType, options = {}) {
  const label = document.createElement('label');
  label.className = 'option';

  const input = document.createElement('input');
  input.type = inputType;
  input.name = String(q.id);
  input.value = String(opt.value ?? opt.key);

  const text = document.createElement('span');
  text.className = 'option-text';
  text.textContent = options.hideKey ? String(opt.text) : `${opt.key}. ${opt.text}`;

  label.append(input, text);
  return label;
}

function renderQuestion(q, idx) {
  const box = document.createElement('div');
  box.className = 'question';

  const title = document.createElement('div');
  title.className = 'q-title';
  title.textContent = `${idx + 1}. ${q.question}（${q.score} 分）`;
  const typeLabels = {
    single_choice: '单选',
    multiple_choice: '多选',
    true_false: '判断',
    short_answer: '简答',
    essay: '论述',
    fill_blank: '填空',
    calculation: '计算',
  };
  const typeLabel = typeLabels[q.type] || null;
  if (typeLabel) {
    const badge = document.createElement('span');
    badge.className = 'q-type-badge';
    badge.textContent = typeLabel;
    title.appendChild(badge);
  }
  box.appendChild(title);

  if (q.type === 'single_choice') {
    for (const opt of q.options || []) {
      box.appendChild(createAnswerOption(q, opt, 'radio'));
    }
  } else if (q.type === 'multiple_choice') {
    for (const opt of q.options || []) {
      box.appendChild(createAnswerOption(q, opt, 'checkbox'));
    }
  } else if (q.type === 'true_false') {
    box.appendChild(createAnswerOption(q, { value: true, text: '正确' }, 'radio', { hideKey: true }));
    box.appendChild(createAnswerOption(q, { value: false, text: '错误' }, 'radio', { hideKey: true }));
  } else {
    const subs = Array.isArray(q.subquestions) ? q.subquestions : [];
    if (subs.length) {
      const wrap = document.createElement('div');
      wrap.className = 'composite-answers';
      subs.forEach((s, subIdx) => {
        const block = document.createElement('div');
        block.className = 'sub-answer';
        const label = document.createElement('div');
        label.className = 'sub-label';
        label.textContent = `${subIdx + 1}、${s.question || ''}（${s.score} 分）`;
        block.appendChild(label);
        const languages = Array.isArray(s.allowed_languages) ? s.allowed_languages : [];
        if (s.scoring_mode === 'code') {
          const languageField = document.createElement('label');
          languageField.className = 'code-language-field';
          const languageLabel = document.createElement('span');
          languageLabel.textContent = '编程语言';
          const select = document.createElement('select');
          select.className = 'code-language-select';
          select.dataset.qid = String(q.id);
          select.dataset.sid = String(s.id);
          for (const language of languages) {
            const option = document.createElement('option');
            option.value = language;
            option.textContent = language;
            select.appendChild(option);
          }
          languageField.append(languageLabel, select);
          block.appendChild(languageField);
        }
        const ta = document.createElement('textarea');
        ta.dataset.qid = String(q.id);
        ta.dataset.sid = String(s.id);
        ta.setAttribute('aria-label', `子题 ${s.id}：${s.question || '答案'}`);
        ta.rows = 6;
        ta.placeholder = '请输入子题答案';
        if (s.scoring_mode === 'code') {
          ta.className = 'code-answer';
          ta.placeholder = '请输入代码';
        }
        block.appendChild(ta);
        wrap.appendChild(block);
      });
      box.appendChild(wrap);
    } else {
      const languages = Array.isArray(q.allowed_languages) ? q.allowed_languages : [];
      const isCode = q.scoring_mode === 'code' || languages.length > 0 || !!q.code_language;
      if (isCode && languages.length > 0) {
        const languageField = document.createElement('label');
        languageField.className = 'code-language-field';
        const languageLabel = document.createElement('span');
        languageLabel.textContent = '编程语言';
        const select = document.createElement('select');
        select.className = 'code-language-select';
        select.name = `${q.id}__language`;
        select.dataset.qid = String(q.id);
        for (const language of languages) {
          const option = document.createElement('option');
          option.value = language;
          option.textContent = language;
          if (q.code_language && language === q.code_language) option.selected = true;
          select.appendChild(option);
        }
        languageField.append(languageLabel, select);
        box.appendChild(languageField);
      }
      const ta = document.createElement('textarea');
      ta.name = String(q.id);
      ta.rows = 6;
      ta.placeholder = '请输入答案';
      if (isCode) {
        ta.className = 'code-answer';
        const hintLang = languages.length ? '' : (q.code_language ? `（${q.code_language}）` : '');
        ta.placeholder = `请输入代码${hintLang}`;
      }
      box.appendChild(ta);
    }
  }
  return box;
}

function startTimer() {
  const tick = () => {
    const elapsed = Math.floor((Date.now() - state.startedAt) / 1000);
    const left = Math.max(0, state.durationSeconds - elapsed);
    const m = Math.floor(left / 60);
    const s = left % 60;
    $('timer').textContent = `${pad(m)}:${pad(s)}`;
    if (left <= 0) {
      clearInterval(state.timerId);
      const autoSubmit = state.exam.config?.auto_submit ?? state.exam.auto_submit ?? true;
      if (autoSubmit) submitExam();
      else $('submitBtn').disabled = true;
    }
  };
  tick();
  state.timerId = setInterval(tick, 1000);
}

// 预览模式：设置预览模式
function setupPreviewMode() {
  // 隐藏登录表单
  const login = $('login');
  if (login) login.style.display = 'none';

  //显示预览横幅
  const banner = document.createElement('div');
  banner.id = 'previewBanner';
  banner.className = 'preview-banner';
  banner.innerHTML = `
    <span>🔍 预览模式 - 当前显示：题目+答案</span>
    <button id="toggleAnswersBtn" class="btn secondary">隐藏答案</button>
  `;
  document.body.insertBefore(banner, document.body.firstChild);

  // 绑定切换按钮
  $('toggleAnswersBtn').addEventListener('click', toggleAnswers);

  // 显示试卷内容
  $('examForm').style.display = 'block';
  const container = $('questions');
  container.textContent = '';
  state.exam.questions.forEach((q, idx) => {
    const questionEl = renderQuestion(q, idx);
    // 如果显示答案，则添加答案显示
    if (state.showAnswers) {
      const answerEl = createAnswerDisplay(q);
      questionEl.appendChild(answerEl);
    }
    container.appendChild(questionEl);
  });

  // 修改提交按钮
  const submitBtn = $('submitBtn');
  if (submitBtn) {
    submitBtn.textContent = '预览模式（不可提交）';
    submitBtn.disabled = true;
  }
}

// 预览模式：切换答案显示
function toggleAnswers() {
  state.showAnswers = !state.showAnswers;
  const banner = $('previewBanner');
  if (banner) {
    banner.querySelector('span').textContent = `预览模式 - 当前显示：${state.showAnswers ? '题目+答案' : '仅题目'}`;
  }
  $('toggleAnswersBtn').textContent = state.showAnswers ? '隐藏答案' : '显示答案';

  // 重新渲染试卷
  const container = $('questions');
  container.textContent = '';
  state.exam.questions.forEach((q, idx) => {
    const questionEl = renderQuestion(q, idx);
    if (state.showAnswers) {
      const answerEl = createAnswerDisplay(q);
      questionEl.appendChild(answerEl);
    }
    container.appendChild(questionEl);
  });
}

// 预览模式：创建答案显示元素
function createAnswerDisplay(q) {
  const wrapper = document.createElement('div');
  wrapper.className = 'answer-display';

  if (q.type === 'single_choice' || q.type === 'multiple_choice') {
    const correctAnswer = q.answer;
    const answerText = Array.isArray(correctAnswer) ? correctAnswer.join(', ') : String(correctAnswer);
    wrapper.innerHTML = `
      <div class="answer-label">正确答案</div>
      <div class="answer-content">${answerText}</div>
    `;
  } else if (q.type === 'true_false') {
    const correctAnswer = q.answer ? '正确' : '错误';
    wrapper.innerHTML = `
      <div class="answer-label">正确答案</div>
      <div class="answer-content">${correctAnswer}</div>
    `;
  } else {
    // 主观题/编程题
    const subs = Array.isArray(q.subquestions) ? q.subquestions : [];
    if (subs.length) {
      let html = '<div class="answer-label">参考答案</div>';
      subs.forEach((s, idx) => {
        html += `
          <div class="sub-answer-display">
            <div class="sub-label">${idx + 1}. ${s.question || ''}</div>
            <div class="sub-content">${s.answer || '暂无参考答案'}</div>
          </div>
        `;
      });
      wrapper.innerHTML = html;
    } else {
      wrapper.innerHTML = `
        <div class="answer-label">参考答案</div>
        <div class="answer-content">${q.answer || '暂无参考答案'}</div>
      `;
    }
  }

  return wrapper;
}

function collectAnswers() {
  const answers = Object.create(null);
  for (const q of state.exam.questions) {
    if (q.type === 'multiple_choice') {
      answers[q.id] = findQuestionControls(q.id).filter(control => control.checked).map(control => control.value);
    } else if (q.type === 'true_false') {
      const el = findQuestionControls(q.id).find(control => control.checked);
      answers[q.id] = el ? (el.value === 'true') : null;
    } else if (q.type === 'single_choice') {
      const el = findQuestionControls(q.id).find(control => control.checked);
      answers[q.id] = el ? el.value : null;
    } else {
      const subs = Array.isArray(q.subquestions) ? q.subquestions : [];
      if (subs.length) {
        const map = Object.create(null);
        for (const s of subs) {
          const el = findSubquestionControl('textarea', q.id, s.id);
          const subAnswer = { answer: el ? el.value : '' };
          if (s.scoring_mode === 'code') {
            const language = findSubquestionControl('select', q.id, s.id);
            subAnswer.language = language ? language.value : '';
          }
          map[s.id] = subAnswer;
        }
        answers[q.id] = map;
      } else {
        const languages = Array.isArray(q.allowed_languages) ? q.allowed_languages : [];
        const isCode = q.scoring_mode === 'code' || languages.length > 0;
        const el = findQuestionControls(q.id).find((c) => c.tagName === 'TEXTAREA' || c.tagName === 'INPUT')
          || findQuestionControls(q.id)[0];
        const text = el ? el.value : '';
        if (isCode && languages.length > 0) {
          const langSelect = document.querySelector(
            `select.code-language-select[data-qid="${String(q.id)}"]:not([data-sid])`
          );
          answers[q.id] = {
            answer: text,
            language: langSelect ? langSelect.value : (q.code_language || languages[0] || ''),
          };
        } else {
          answers[q.id] = text;
        }
      }
    }
  }
  return answers;
}

function showSuccess() {
  if (state.timerId) clearInterval(state.timerId);
  $('login').style.display = 'none';
  $('examForm').style.display = 'none';
  $('successPanel').style.display = 'block';
  const rc = $('resultContent');
  rc.textContent = '';
  const h2 = document.createElement('h2');
  h2.textContent = '提交成功';
  const p = document.createElement('p');
  p.className = 'muted';
  p.textContent = '系统已收到答卷，成绩由管理员复核后公布，请勿重复提交。';
  rc.append(h2, p);
}

async function submitExam(autoSubmitReason = null) {
  // 预览模式：禁用提交
  if (isPreview) {
    alert('当前为预览模式，试卷不可提交。');
    return;
  }

  const isAutoSubmit = typeof autoSubmitReason === 'string';
  if (isAutoSubmit) {
    if (state.autoSubmitStarted || state.submitting) return;
    state.autoSubmitStarted = true;
    clearAwayTimer();
    toast('检测到切屏，正在自动交卷…');
  } else if (autoSubmitReason) {
    autoSubmitReason.preventDefault();
  }
  if (state.submitting) return;

  const name = $('name').value.trim();
  const employeeId = $('employee_id').value.trim();
  if (!name || !employeeId) {
    toast('请填写姓名和工号');
    return;
  }

  state.submitting = true;
  $('submitBtn').disabled = true;
  try {
    const payload = {
      name,
      employee_id: employeeId,
      paper_id: state.paperId,
      department: ($('department').value || '').trim() || null,
      answers: collectAnswers(),
      started_at: state.startedAt ? new Date(state.startedAt).toISOString() : null,
      ...(isAutoSubmit ? { auto_submit_reason: autoSubmitReason } : {}),
    };
    const res = await fetch('/api/submit', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) {
      const detail = data.detail;
      let message = data.message || '提交失败';
      if (typeof detail === 'string') {
        message = detail;
      } else if (detail && typeof detail === 'object' && !Array.isArray(detail) && detail.message) {
        message = detail.message;
      } else if (Array.isArray(detail) && detail.length) {
        // Pydantic 422: 展示可读字段错误，避免只显示“提交失败”
        message = detail.map((item) => item.msg || JSON.stringify(item)).join('; ');
      }
      throw new Error(message);
    }
    showSuccess();
  } catch (e) {
    toast(e.message || '提交失败');
    state.submitting = false;
    state.autoSubmitStarted = false;
    $('submitBtn').disabled = false;
  }
}

function clearAwayTimer() {
  if (state.awayTimeoutId !== null) {
    window.clearTimeout(state.awayTimeoutId);
    state.awayTimeoutId = null;
  }
}

function handlePageAway() {
  if (state.isPageAway || state.autoSubmitStarted || !state.startedAt) return;
  state.isPageAway = true;
  state.blurCount += 1;
  if (state.blurCount >= AUTO_SUBMIT_AFTER_BLURS) {
    clearAwayTimer();
    submitExam('third_blur');
    return;
  }
  clearAwayTimer();
  state.awayTimeoutId = window.setTimeout(() => {
    if (state.isPageAway && !state.autoSubmitStarted) submitExam('blur_timeout_30s');
  }, AWAY_TIMEOUT_MS);
}

function handlePageReturn() {
  if (!state.isPageAway) return;
  state.isPageAway = false;
  clearAwayTimer();
}

function setupAntiSwitchAutoSubmit() {
  if (state.antiSwitchSetup) return;
  state.antiSwitchSetup = true;
  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState === 'hidden') handlePageAway();
    else handlePageReturn();
  });
  window.addEventListener('blur', handlePageAway);
  window.addEventListener('focus', handlePageReturn);
}

async function startExam() {
  // 预览模式：跳过登录，直接显示试卷
  if (isPreview) {
    await loadExam();
    return;
  }

  const name = $('name').value.trim();
  const employeeId = $('employee_id').value.trim();
  if (!name || !employeeId) {
    toast('请填写姓名和工号');
    return;
  }
  try {
    const res = await fetch('/api/exam/start', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ employee_id: employeeId, paper_id: state.paperId }),
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.detail?.message || '开始考试失败');

    state.startedAt = Date.now();
    $('login').style.display = 'none';
    $('examForm').style.display = 'block';
    const container = $('questions');
    container.textContent = '';
    state.exam.questions.forEach((q, idx) => container.appendChild(renderQuestion(q, idx)));
    startTimer();
    setupAntiSwitchAutoSubmit();
  } catch (e) {
    toast(e.message || '开始失败');
  }
}

$('startBtn').addEventListener('click', startExam);
$('examForm').addEventListener('submit', (event) => {
  event.preventDefault();
  submitExam(null);
});

// 预览模式：自动开始考试
if (isPreview) {
  startExam().catch(err => {
    console.error('预览模式启动失败:', err);
    showFatal('预览模式启动失败: ' + err.message);
  });
} else {
  loadExam().catch(() => {});
}
