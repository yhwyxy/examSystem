const AUTO_SUBMIT_AFTER_BLURS = 3;
const AWAY_TIMEOUT_MS = 30_000;

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

function safeQuestionId(id) {
  const raw = String(id);
  if (window.CSS && typeof CSS.escape === 'function') return CSS.escape(raw);
  return raw.replace(/\\/g, '\\\\').replace(/"/g, '\\"');
}

function questionControlSelector(q, suffix = '') {
  return `[name="${safeQuestionId(q.id)}"]${suffix}`;
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

  const res = await fetch(`/api/exam?paper=${encodeURIComponent(state.paperId)}`);
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
      for (const s of subs) {
        const block = document.createElement('div');
        block.className = 'sub-answer';
        const label = document.createElement('div');
        label.className = 'sub-label';
        label.textContent = `(${s.id}) ${s.question || ''}（${s.score} 分）`;
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
      }
      box.appendChild(wrap);
    } else {
      const ta = document.createElement('textarea');
      ta.name = String(q.id);
      ta.rows = 6;
      ta.placeholder = '请输入答案';
      if (q.scoring_mode === 'code' || q.code_language) {
        ta.className = 'code-answer';
        ta.placeholder = `请输入代码${q.code_language ? '（' + q.code_language + '）' : ''}`;
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

function collectAnswers() {
  const answers = {};
  for (const q of state.exam.questions) {
    if (q.type === 'multiple_choice') {
      answers[q.id] = [...document.querySelectorAll(questionControlSelector(q, ':checked'))].map(el => el.value);
    } else if (q.type === 'true_false') {
      const el = document.querySelector(questionControlSelector(q, ':checked'));
      answers[q.id] = el ? (el.value === 'true') : null;
    } else if (q.type === 'single_choice') {
      const el = document.querySelector(questionControlSelector(q, ':checked'));
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
        const el = document.querySelector(questionControlSelector(q));
        answers[q.id] = el ? el.value : '';
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
      ...(autoSubmitReason ? { auto_submit_reason: autoSubmitReason } : {}),
    };
    const res = await fetch('/api/submit', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.detail?.message || data.message || '提交失败');
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
$('examForm').addEventListener('submit', submitExam);

loadExam().catch(() => {});
