// SPDX-License-Identifier: AGPL-3.0-or-later
//
// k6 scenario: 100 comments/sec across 50 issues. Stresses the
// notification fan-out path — every comment triggers email + inbox
// + websocket fan-out via the worker queue.
//
// Verifies that:
//   - Comment POSTs return 200 (or 429 if rate-limited).
//   - Worker queue stays bounded under sustained load.
//   - DB connection pool doesn't exhaust.
//
// Required env:
//   BASE       — staging URL
//   TOKEN      — a PAT for a test user with `repo` write to all 50 issues
//   REPO       — `owner/name` of the test repo
//   FIRST_ISSUE — int, the first issue number (script writes to FIRST..FIRST+49)
//
// NOTE: this scenario currently uses the issue web form (HTML) since
// the issues API is not yet shipped (see docs/public/api/issues.md).
// When the API lands, switch to it.

import http from "k6/http";
import { check } from "k6";

const BASE        = __ENV.BASE || "http://127.0.0.1:8080";
const REPO        = __ENV.REPO || "alice/loadtest";
const FIRST_ISSUE = parseInt(__ENV.FIRST_ISSUE || "1", 10);
const TOKEN       = __ENV.TOKEN;

export const options = {
  thresholds: JSON.parse(open("../thresholds.json")),
  scenarios: {
    comment_storm: {
      executor: "constant-arrival-rate",
      rate: 100,
      timeUnit: "1s",
      duration: __ENV.DURATION || "5m",
      preAllocatedVUs: 50,
      maxVUs: 200,
    },
  },
};

export default function () {
  const issueNum = FIRST_ISSUE + Math.floor(Math.random() * 50);
  const url = `${BASE}/${REPO}/issues/${issueNum}/comments`;
  const body = {
    body: `Load-test comment at ${Date.now()} (vu ${__VU} iter ${__ITER})`,
  };
  const headers = TOKEN
    ? { "Authorization": `Bearer ${TOKEN}`, "Content-Type": "application/json" }
    : { "Content-Type": "application/json" };

  const res = http.post(url, JSON.stringify(body), { headers, tags: { kind: "issue-comment" } });
  check(res, {
    "no 5xx":             (r) => r.status < 500,
    "200 or 429 expected": (r) => r.status === 200 || r.status === 201 || r.status === 429,
  });
}
