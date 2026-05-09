// SPDX-License-Identifier: AGPL-3.0-or-later
//
// k6 scenario: 30 RPS to search endpoints with realistic query
// distribution. Search is one of the more DB-heavy paths; this
// scenario surfaces N+1 regressions and index-coverage gaps.
//
// Run:
//   k6 run -e BASE=https://staging.shithub.example \
//     tests/load/k6/scenarios/search-load.js

import http from "k6/http";
import { check } from "k6";

const BASE = __ENV.BASE || "http://127.0.0.1:8080";

export const options = {
  thresholds: JSON.parse(open("../thresholds.json")),
  scenarios: {
    search: {
      executor: "constant-arrival-rate",
      rate: 30,
      timeUnit: "1s",
      duration: __ENV.DURATION || "10m",
      preAllocatedVUs: 20,
      maxVUs: 80,
    },
  },
};

// Realistic distribution: short common terms dominate, with a long
// tail of more specific phrases.
const QUERIES = [
  "func", "main", "test", "config", "import",
  "TODO", "FIXME", "context.Context", "http.Handler",
  "policy.Can", "render.New", "go-chi", "golang",
  "argon2id", "session_key", "WAL archive",
];
const SCOPES = ["", "type:repo", "type:user", "type:issue"];

export default function () {
  const q = QUERIES[Math.floor(Math.random() * QUERIES.length)];
  const scope = SCOPES[Math.floor(Math.random() * SCOPES.length)];
  const path = `/search?q=${encodeURIComponent(q + (scope ? " " + scope : ""))}`;
  const res = http.get(`${BASE}${path}`, { tags: { kind: "search" } });
  check(res, {
    "no 5xx": (r) => r.status < 500,
  });
}
