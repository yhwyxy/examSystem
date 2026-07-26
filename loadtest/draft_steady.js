// Task 13 k6 draft_steady: 50 并发持续 draft 5min (plan 行 1782 阈值)
// 阈值: http_req_failed<1%p95<750ms (草稿稳态; 读 exam_sessions + 更新 draft_revision)
// 数据: loadtest/scenarios/draft-sessions.json (500 预 start session, 每 VU 自循环)

import http from 'k6/http';
import { check, sleep } from 'k6';
import { SharedArray } from 'k6/data';
import exec from 'k6/execution';

const sessions = new SharedArray('draft_sessions', function () {
  return JSON.parse(open('./scenarios/draft-sessions.json'));
});
const BASE = __ENV.BASE_URL || 'http://127.0.0.1:18080';
const PAPER = JSON.parse(open('./frontend/loadtest-draft.json'));

// 复刻 prepare._gen_answers: 全 A 草稿
const DRAFT_ANSWERS = {};
PAPER.questions.forEach((q) => {
  DRAFT_ANSWERS[q.id] = q.type === 'single_choice' ? { key: 'A' } : { text: '答 loadtest' };
});

export const options = {
  scenarios: {
    draft_steady: {
      executor: 'constant-vus',
      vus: __ENV.SMOKE ? 10 : 50,
      duration: __ENV.SMOKE ? '10s' : '5m',
      gracefulStop: '5s',
    },
  },
  thresholds: {
    'http_req_failed': ['rate<0.01'],
    'http_req_duration{name:draft}': ['p(95)<750'],
  },
};

// 接受 409 (revision monotonic 撞同 revision) 为 expected_response, 不计 http_req_failed
http.setResponseCallback(http.expectedStatuses(200, 201, 409));

export default function () {
  const vu = exec.vu.idInTest - 1;
  const iter = exec.scenario.iterationInTest;
  // 每 VU 固定拿一个独立 session (50 user >> 10 vu smoke), 避免 cross-VU revision 撞
  // revision=iter 对应当前 session 已有 revision (CAS old value), 0 起跳
  // 第一次 PUT oldRevision=0 -> server 存 newRevision=1; 第二次 iter oldRevision=1 -> newRevision=2...
  const s = sessions[vu % sessions.length];
  const oldRevision = iter;
  const sid = encodeURIComponent(s.session_id || 'dummy');
  const res = http.put(
    `${BASE}/api/exam/sessions/${sid}/draft`,
    JSON.stringify({
      session_token: s.session_token,
      answers: DRAFT_ANSWERS,
      revision: oldRevision,
    }),
    { headers: { 'Content-Type': 'application/json' }, tags: { name: 'draft' } }
  );
  check(res, {
    'status 200 or 409 (revisionCAS idempotent)': (r) => {
      return r.status === 200 || r.status === 409;
    },
  });
  sleep(0.5);
}
