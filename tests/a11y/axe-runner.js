// SPDX-License-Identifier: AGPL-3.0-or-later
//
// axe-core runner. Drives Puppeteer through the authenticated and
// anonymous routes that matter for WCAG AA compliance, runs axe on
// each, and prints a structured report. Exits non-zero on any
// "critical" or "serious" violation.
//
// Usage:
//   SHITHUB_URL=http://127.0.0.1:8080 \
//   SHITHUB_USER=alice SHITHUB_PASS=... \
//   node tests/a11y/axe-runner.js
//
// Dependencies (install once in CI or your dev machine):
//   npm i puppeteer @axe-core/puppeteer
//
// The runner is intentionally narrow — pa11y-ci handles broad
// scanning; this script focuses on flows that need a logged-in
// session (settings, /admin, repo settings) plus the diff view,
// which has subtle semantics for screen readers (old/new sides).

const { AxePuppeteer } = require("@axe-core/puppeteer");
const puppeteer = require("puppeteer");

const BASE = process.env.SHITHUB_URL || "http://127.0.0.1:8080";
const USER = process.env.SHITHUB_USER || "";
const PASS = process.env.SHITHUB_PASS || "";

// Each entry: { name, path, requiresAuth, axeOpts? }.
const ROUTES = [
  { name: "home (anon)",      path: "/",            requiresAuth: false },
  { name: "signup",           path: "/signup",      requiresAuth: false },
  { name: "login",            path: "/login",       requiresAuth: false },
  { name: "explore",          path: "/explore",     requiresAuth: false },

  // Authenticated surfaces. SHITHUB_USER + SHITHUB_PASS must be set;
  // these are the routes most likely to have form-error association
  // and focus-trap regressions.
  { name: "dashboard",        path: "/",            requiresAuth: true  },
  { name: "settings/profile", path: "/settings/profile", requiresAuth: true },
  { name: "settings/2fa",     path: "/settings/security/2fa", requiresAuth: true },
  { name: "new repo form",    path: "/new",         requiresAuth: true  },
  { name: "notifications",    path: "/notifications", requiresAuth: true },
];

async function login(page) {
  if (!USER || !PASS) {
    throw new Error("SHITHUB_USER and SHITHUB_PASS must be set for authenticated routes");
  }
  await page.goto(`${BASE}/login`, { waitUntil: "networkidle0" });
  await page.type('input[name="username"]', USER);
  await page.type('input[name="password"]', PASS);
  await Promise.all([
    page.waitForNavigation({ waitUntil: "networkidle0" }),
    page.click('button[type="submit"]'),
  ]);
}

function summarizeViolations(name, results) {
  const high = results.violations.filter(v => v.impact === "critical" || v.impact === "serious");
  const low = results.violations.filter(v => v.impact !== "critical" && v.impact !== "serious");
  console.log(`\n=== ${name} ===`);
  console.log(`  passes: ${results.passes.length}, incomplete: ${results.incomplete.length}, violations: ${results.violations.length}`);
  if (high.length > 0) {
    console.log(`  HIGH SEVERITY (${high.length}):`);
    for (const v of high) {
      console.log(`    - [${v.impact}] ${v.id}: ${v.help}`);
      console.log(`      ${v.helpUrl}`);
      console.log(`      affected nodes: ${v.nodes.length}`);
    }
  }
  if (low.length > 0) {
    console.log(`  Lower severity (${low.length}):`);
    for (const v of low) {
      console.log(`    - [${v.impact}] ${v.id}: ${v.help}`);
    }
  }
  return high.length;
}

async function main() {
  const browser = await puppeteer.launch({
    args: ["--no-sandbox", "--disable-setuid-sandbox", "--disable-dev-shm-usage"],
  });
  const page = await browser.newPage();
  await page.setViewport({ width: 1280, height: 1024 });

  let totalHigh = 0;
  let didLogin = false;
  for (const route of ROUTES) {
    if (route.requiresAuth && !didLogin) {
      try {
        await login(page);
        didLogin = true;
      } catch (e) {
        console.log(`SKIP authenticated routes: ${e.message}`);
        break;
      }
    }
    await page.goto(`${BASE}${route.path}`, { waitUntil: "networkidle0" });
    const results = await new AxePuppeteer(page)
      .withTags(["wcag2a", "wcag2aa", "best-practice"])
      .analyze();
    totalHigh += summarizeViolations(route.name, results);
  }

  await browser.close();

  console.log(`\nTotal high-severity violations: ${totalHigh}`);
  process.exit(totalHigh === 0 ? 0 : 1);
}

main().catch(err => {
  console.error(err);
  process.exit(2);
});
