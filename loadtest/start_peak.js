// Task 13 k6 start_peak: 500 并发开考 30s (plan 行 1771 阈值)
// 阈值: http_req_failed<1%p95<500ms (开考峰值; 大量插 exam_sessions)
// 数据: loadtest/scenarios/start-users.json (500 user + 1 run_token, 不预 start)

import http from 'k6/http';
import { check, sleep } from 'k6';
import { SharedArray } from 'k6/data';
import exec from 'k6/execution';

const data = new SharedArray('users', function () {
  return JSON.parse(open('./scenarios/start-users.json')).users;
});
const RUN_TOKEN = JSON.parse(open('./scenarios/start-users.json')).run_token;
const BASE = __ENV.BASE_URL || 'http://127.0.0.1:18080';

export const options = {
  scenarios: {
    start_peak: {
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
    'http_req_duration{name:start}': ['p(95)<500'],
  },
};

export default function () {
  const vu = exec.vu.idInTest - 1;
  const u = data[vu % data.length];
  const res = http.post(
    `${BASE}/api/exam/start`,
    JSON.stringify({
      run_token: RUN_TOKEN,
      employee_id: u.employee_id,
      name: u.name,
      department: u.department,
    }),
    { headers: { 'Content-Type': 'application/json' }, tags: { name: 'start' } }
  );
  check(res, {
    'status 200': (r) => r.status === 200,
    'has session_token OR resume null (idempotent)': (r) => {
      const b = r.json();
      return b && (b.session_token || b.session_id);
    },
  });
}
