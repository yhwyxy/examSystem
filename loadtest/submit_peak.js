// Task 13 k6 submit_peak: 500 并发提交 30s (plan 行 1771 阈值)
// 阈值: http_req_failed<1%p95<750ms (提交峰值; 落 submissions 行 + 评分入队)
// 数据: loadtest/scenarios/submit-sessions.json (500 预 start)

import http from 'k6/http';
import { check, sleep } from 'k6';
import { SharedArray } from 'k6/data';
import exec from 'k6/execution';

const sessions = new SharedArray('submit_sessions', function () {
  return JSON.parse(open('./scenarios/submit-sessions.json'));
});
const BASE = __ENV.BASE_URL || 'http://127.0.0.1:18080';
const PAPER = JSON.parse(open('./frontend/loadtest-submit.json'));

const SUBMIT_ANSWERS = {};
PAPER.questions.forEach((q) => {
  SUBMIT_ANSWERS[q.id] = q.type === 'single_choice' ? { key: 'A' } : { text: '答 loadtest' };
});

export const options = {
  scenarios: {
    submit_peak: {
      executor: 'ramping-vus',
      startVUs: 0,
      stages: __ENV.SMOKE ? [
        { duration: '3s', target: 10 },
        { duration: '5s', target: 20 },
        { duration: '2s', target: 0 },
      ] : [
        { duration: '5s', target: 200 },
        { duration: '10s', target: 500 },
        { duration: '5s', target: 0 },
      ],
      gracefulStop: '2s',
    },
  },
  thresholds: {
    'http_req_failed': ['rate<0.01'],
    'http_req_duration{name:submit}': ['p(95)<750'],
  },
};

// 200/201 (ok) / 409 (duplicate submit) 均为 expected, 不计 http_req_failed
http.setResponseCallback(http.expectedStatuses(200, 201, 409));

export default function () {
  const vu = exec.vu.idInTest - 1;
  const s = sessions[vu % sessions.length];
  const res = http.post(
    `${BASE}/api/session/submit`,
    JSON.stringify({
      session_token: s.session_token,
      answers: SUBMIT_ANSWERS,
    }),
    { headers: { 'Content-Type': 'application/json' }, tags: { name: 'submit' } }
  );
  check(res, {
    'status 200/201/409 (submit ok / duplicate idempotent)': (r) => {
      return r.status === 200 || r.status === 201 || r.status === 409;
    },
  });
}
