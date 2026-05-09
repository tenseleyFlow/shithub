// SPDX-License-Identifier: AGPL-3.0-or-later
//
// k6 scenario: 50 RPS of authenticated API + browsing.
// Each VU presents a PAT (Bearer token) on /api/v1 calls and
// rotates between API reads, dashboard renders, and notification
// inbox views.
//
// Required env:
//   BASE   — staging URL
//   TOKEN  — a PAT for a test user with `user:read` + `repo:read`
//
// Run:
//   k6 run -e BASE=https://staging.shithub.example -e TOKEN=shp_... \
//     tests/load/k6/scenarios/auth-mix.js

import http from "k6/http";
import { check } from "k6";

const BASE  = __ENV.BASE  || "http://127.0.0.1:8080";
const TOKEN = __ENV.TOKEN;
if (!TOKEN) {
  throw new Error("TOKEN env var required (a valid shp_ PAT)");
}

export const options = {
  thresholds: JSON.parse(open("../thresholds.json")),
  scenarios: {
    auth_mix: {
      executor: "constant-arrival-rate",
      rate: 50,
      timeUnit: "1s",
      duration: __ENV.DURATION || "10m",
      preAllocatedVUs: 25,
      maxVUs: 100,
    },
  },
};

const apiHeaders = {
  "Authorization": `Bearer ${TOKEN}`,
  "Accept":        "application/json",
};

const ACTIONS = [
  () => http.get(`${BASE}/api/v1/user`,         { headers: apiHeaders, tags: { kind: "api-user" } }),
  () => http.get(`${BASE}/api/v1/user/starred`, { headers: apiHeaders, tags: { kind: "api-stars" } }),
  () => http.get(`${BASE}/`,                    { tags: { kind: "ui-home" } }),
  () => http.get(`${BASE}/notifications`,       { tags: { kind: "ui-notifications" } }),
];

export default function () {
  const action = ACTIONS[Math.floor(Math.random() * ACTIONS.length)];
  const res = action();
  check(res, {
    "no 5xx": (r) => r.status < 500,
    "no auth 401": (r) => r.status !== 401,
  });
}
