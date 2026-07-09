const state = { exam: null, startedAt: null, durationSeconds: 0, timerId: null };

function $(id) { return document.getElementById(id); }
function toast(msg) { const el = $('toast'); el.textContent = msg; el.style.display = 'block'; setTimeout(() => el.style.display = 'none', 2600); }
function pad(n) { return String(n).padStart(2, '0'); }

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

function renderQuestion(q, idx) {
  const box = document.createElement('div');
  box.className = 'question';
  const title = document.createElement('div');
  title.className = 'q-title';
  title.textContent = `${idx + 1}. ${q.question}（${q.score} 分）`;
  box.appendChild(title);

  if (q.type === 'single_choice') {
    for (const opt of q.options || []) {
      const label = document.createElement('label');
      label.className = 'option';
      label.innerHTML = `<input type="radio" name="${q.id}" value="${opt.key}"> ${opt.key}. ${opt.text}`;
      box.appendChild(label);
    }
  } else if (q.type === 'multiple_choice') {
    for (const opt of q.options || []) {
      const label = document.createElement('label');
      label.className = 'option';
      label.innerHTML = `<input type="checkbox" name="${q.id}" value="${opt.key}"> ${opt.key}. ${opt.text}`;
      box.appendChild(label);
    }
  } else if (q.type === 'true_false') {
    const t = document.createElement('label');
    t.className = 'option';
    t.innerHTML = `<input type="radio" name="${q.id}" value="true"> 正确`;
    const f = document.createElement('label');
    f.className = 'option';
    f.innerHTML = `<input type="radio" name="${q.id}" value="false"> 错误`;
    box.appendChild(t); box.appendChild(f);
  } else {
    const ta = document.createElement('textarea');
    ta.name = q.id;
    ta.placeholder = '请输入答案';
    box.appendChild(ta);
  }
  return box;
}

function renderExam() {
  const container = $('questions');
  container.innerHTML = '';
  state.exam.questions.forEach((q, idx) => container.appendChild(renderQuestion(q, idx)));
}

function startTimer(startedAt) {
  state.startedAt = startedAt || new Date().toISOString();
  const endAt = Date.now() + state.durationSeconds * 1000;
  state.timerId = setInterval(() => {
    const left = Math.max(0, Math.floor((endAt - Date.now()) / 1000));
    $('timer').textContent = `${pad(Math.floor(left / 60))}:${pad(left % 60)}`;
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
      answers[q.id] = Array.from(document.querySelectorAll(`input[name="${q.id}"]:checked`)).map(x => x.value);
    } else if (q.type === 'single_choice') {
      const checked = document.querySelector(`input[name="${q.id}"]:checked`);
      answers[q.id] = checked ? checked.value : null;
    } else if (q.type === 'true_false') {
      const checked = document.querySelector(`input[name="${q.id}"]:checked`);
      answers[q.id] = checked ? checked.value === 'true' : null;
    } else {
      answers[q.id] = document.querySelector(`[name="${q.id}"]`)?.value || '';
    }
  }
  return answers;
}

async function submitExam(evt) {
  if (evt) evt.preventDefault();
  const name = $('name').value.trim();
  const employeeId = $('employee_id').value.trim();
  if (!name || !employeeId) { toast('请填写姓名和工号'); return; }
  $('submitBtn').disabled = true;
  const payload = {
    name,
    employee_id: employeeId,
    department: $('department').value.trim() || null,
    started_at: state.startedAt,
    answers: collectAnswers(),
  };
  const res = await fetch('/api/submit', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload) });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) {
    toast(data.detail?.message || '提交失败');
    $('submitBtn').disabled = false;
    return;
  }
  clearInterval(state.timerId);
  // 隐藏答题表单，直接显示提交成功页面（评分在后台进行，不暴露给员工）
  $('examForm').style.display = 'none';
  $('result').style.display = 'block';
  $('resultContent').innerHTML = `
    <div class="card" style="text-align:center;padding:30px">
      <h3 style="color:#10b981">✅ 提交成功</h3>
      <p style="margin-top:10px">您的试卷已提交成功！</p>
      <p style="color:#64748b;margin-top:8px">成绩将在管理员复核后另行通知，请留意通知。</p>
    </div>`;
}

async function pollGradingStatus(submissionId) {
  let elapsed = 0;
  const interval = 2000; // 每 2 秒轮询一次
  const timer = setInterval(async () => {
    elapsed += interval;
    $('grading-hint').textContent = `已等待 ${Math.floor(elapsed / 1000)} 秒…`;
    try {
      const res = await fetch(`/api/submission/${submissionId}/status`);
      if (!res.ok) return;
      const info = await res.json();
      // grading 状态继续等待，其他状态（pass / need_review / reviewed）表示评分完成
      if (info.status !== 'grading') {
        clearInterval(timer);
        // 评分完成，获取完整结果并展示
        showResult(submissionId);
      }
    } catch (_) {
      // 网络异常时继续重试
    }
  }, interval);
}

async function showResult(submissionId) {
  $('grading').style.display = 'none';
  $('result').style.display = 'block';
  // 简单展示完成信息（不暴露 submission_id，避免枚举攻击）
  $('resultContent').innerHTML = `
    <div class="card" style="text-align:center;padding:30px">
      <h3 style="color:#10b981">✅ 评分已完成</h3>
      <p style="margin-top:10px">您的试卷已评分完成！</p>
      <p style="color:#64748b;margin-top:8px">最终成绩需管理员复核后确认，请留意通知。</p>
    </div>`;
}

$('startBtn').addEventListener('click', async () => {
  const name = $('name').value.trim();
  const employeeId = $('employee_id').value.trim();
  if (!name || !employeeId) { toast('请填写姓名和工号'); return; }
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
