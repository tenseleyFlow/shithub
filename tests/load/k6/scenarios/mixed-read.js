// SPDX-License-Identifier: AGPL-3.0-or-later
//
// k6 scenario: anonymous browsing of public repos at 100 RPS
// sustained for 10 minutes. This is the highest-volume traffic
// shape we expect at MVP launch — the home page, /explore, and
// repo overview pages dominate the read mix.
//
// Run:
//   k6 run --vus 50 --duration 10m \
//     -e BASE=https://staging.shithub.example \
//     tests/load/k6/scenarios/mixed-read.js
//
// `vus` is intentionally tunable: 50 VUs at ~50 ms/req averages
// to ~1000 RPS; the scenario is throttled by think-time so the
// effective rate lands near 100 RPS unless the staging instance
// is faster than expected.

import http from "k6/http";
import { check, sleep } from "k6";

const BASE = __ENV.BASE || "http://127.0.0.1:8080";

export const options = {
  thresholds: JSON.parse(open("../thresholds.json")),
  scenarios: {
    anonymous_browse: {
      executor: "constant-arrival-rate",
      rate: 100, // requests per timeUnit
      timeUnit: "1s",
      duration: __ENV.DURATION || "10m",
      preAllocatedVUs: 50,
      maxVUs: 200,
    },
  },
};

const PATHS = [
  "/",
  "/explore",
  "/explore?sort=stars",
  "/-/health",
  // Plug in seeded-fixture repo paths if your staging has them:
  // "/alice/example-repo",
  // "/alice/example-repo/tree/main",
];

export default function () {
  const path = PATHS[Math.floor(Math.random() * PATHS.length)];
  const res = http.get(`${BASE}${path}`, { tags: { route: path } });
  check(res, {
    "status 2xx or 304": (r) => r.status === 200 || r.status === 304,
    "no 5xx":            (r) => r.status < 500,
  });
}
