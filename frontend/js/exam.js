const state = {
  exam: null,
  startedAt: null,
  durationSeconds: 0,
  timerId: null,
  submitting: false,
};

function $(id) { return document.getElementById(id); }

function toast(msg) {
  const el = $('toast');
  el.textContent = msg;
  el.style.display = 'block';
  setTimeout(() => { el.style.display = 'none'; }, 2600);
}

function pad(n) { return String(n).padStart(2, '0'); }

function safeQuestionId(id) {
  const raw = String(id);
  if (window.CSS && typeof CSS.escape === 'function') return CSS.escape(raw);
  return raw.replace(/\\/g, '\\\\').replace(/"/g, '\\"');
}

function questionControlSelector(q, suffix = '') {
  return `[name="${safeQuestionId(q.id)}"]${suffix}`;
}

async function loadExam() {
  const res = await fetch('/api/exam');
  if (!res.ok) {
    const err = await res.json().catch(() => ({}));
    throw new Error(err.detail?.message || '考试加载失败');
  }

  state.exam = await res.json();
  $('title').textContent = state.exam.exam_info?.title || state.exam.config?.title || '企业考试';
  $('desc').textContent = state.exam.exam_info?.description || '';
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
    const ta = document.createElement('textarea');
    ta.name = String(q.id);
    ta.placeholder = '请输入答案';
    box.appendChild(ta);
  }

  return box;
}

function renderExam() {
  const container = $('questions');
  container.textContent = '';
  state.exam.questions.forEach((q, idx) => container.appendChild(renderQuestion(q, idx)));
}

function updateTimer(endAt) {
  const left = Math.max(0, Math.floor((endAt - Date.now()) / 1000));
  $('timer').textContent = `${pad(Math.floor(left / 60))}:${pad(left % 60)}`;
  return left;
}

function startTimer(startedAt) {
  state.startedAt = startedAt || new Date().toISOString();
  const endAt = Date.now() + state.durationSeconds * 1000;
  updateTimer(endAt);
  state.timerId = setInterval(() => {
    const left = updateTimer(endAt);
    if (left <= 0) {
      clearInterval(state.timerId);
      const autoSubmit = state.exam.config?.auto_submit ?? state.exam.auto_submit ?? true;
      if (autoSubmit) submitExam();
      else $('submitBtn').disabled = true;
    }
  }, 500);
}

function collectAnswers() {
  const answers = {};
  for (const q of state.exam.questions) {
    if (q.type === 'multiple_choice') {
      answers[q.id] = Array.from(document.querySelectorAll(`${questionControlSelector(q, ':checked')}`))
        .map(x => x.value);
    } else if (q.type === 'single_choice') {
      const checked = document.querySelector(`${questionControlSelector(q, ':checked')}`);
      answers[q.id] = checked ? checked.value : null;
    } else if (q.type === 'true_false') {
      const checked = document.querySelector(`${questionControlSelector(q, ':checked')}`);
      answers[q.id] = checked ? checked.value === 'true' : null;
    } else {
      answers[q.id] = document.querySelector(questionControlSelector(q))?.value || '';
    }
  }
  return answers;
}

function showSubmitSuccess() {
  const content = $('resultContent');
  content.textContent = '';

  const mark = document.createElement('div');
  mark.className = 'success-mark';
  mark.textContent = '✓';

  const title = document.createElement('h2');
  title.textContent = '提交成功';

  const message = document.createElement('p');
  message.textContent = '您的试卷已提交成功。';

  const hint = document.createElement('p');
  hint.className = 'muted';
  hint.textContent = '成绩将在管理员复核后另行通知，请留意通知。';

  content.append(mark, title, message, hint);
  $('examForm').style.display = 'none';
  $('successPanel').style.display = 'block';
}

async function submitExam(evt) {
  if (evt) evt.preventDefault();
  if (state.submitting) return;

  const name = $('name').value.trim();
  const employeeId = $('employee_id').value.trim();
  if (!name || !employeeId) {
    toast('请填写姓名和工号');
    return;
  }

  state.submitting = true;
  $('submitBtn').disabled = true;

  const payload = {
    name,
    employee_id: employeeId,
    department: $('department').value.trim() || null,
    started_at: state.startedAt,
    answers: collectAnswers(),
  };

  try {
    const res = await fetch('/api/submit', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.detail?.message || '提交失败');

    clearInterval(state.timerId);
    showSubmitSuccess();
  } catch (err) {
    state.submitting = false;
    $('submitBtn').disabled = false;
    toast(err.message || '提交失败');
  }
}

$('startBtn').addEventListener('click', async () => {
  const name = $('name').value.trim();
  const employeeId = $('employee_id').value.trim();
  if (!name || !employeeId) {
    toast('请填写姓名和工号');
    return;
  }

  $('startBtn').disabled = true;
  try {
    const res = await fetch('/api/exam/start', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ employee_id: employeeId }),
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.detail?.message || '考试开始失败');

    $('login').style.display = 'none';
    $('examForm').style.display = 'block';
    renderExam();
    startTimer(data.started_at);
  } catch (err) {
    toast(err.message || '考试开始失败');
    $('startBtn').disabled = false;
  }
});

$('examForm').addEventListener('submit', submitExam);

loadExam().catch(err => toast(err.message));
