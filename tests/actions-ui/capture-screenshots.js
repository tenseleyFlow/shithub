#!/usr/bin/env node
// SPDX-License-Identifier: AGPL-3.0-or-later

const fs = require("fs/promises");
const path = require("path");

let puppeteer;
try {
  puppeteer = require("puppeteer");
} catch (err) {
  console.error("puppeteer is not installed; run: npm i -g puppeteer");
  process.exit(2);
}

const baseURL = (process.env.SHITHUB_URL || "http://127.0.0.1:8080").replace(/\/+$/, "");
const repo = process.env.SHITHUB_ACTIONS_REPO || "mfwolffe/scratch";
const workflow = normalizeWorkflow(process.env.SHITHUB_ACTIONS_WORKFLOW || "smoke.yml");
const runIndex = process.env.SHITHUB_ACTIONS_RUN || "";
const stepPath = process.env.SHITHUB_ACTIONS_STEP || "jobs/0/steps/0";
const outRoot = process.env.SHITHUB_ACTIONS_AUDIT_OUT ||
  path.join(".refs", "actions-ui-audit", new Date().toISOString().replace(/[:.]/g, "-"));

const viewports = [
  { name: "desktop-dark", width: 1440, height: 1000, colorScheme: "dark", reducedMotion: "no-preference" },
  { name: "desktop-light", width: 1440, height: 1000, colorScheme: "light", reducedMotion: "no-preference" },
  { name: "mobile-dark", width: 390, height: 844, colorScheme: "dark", reducedMotion: "no-preference" },
  { name: "mobile-light-reduce", width: 390, height: 844, colorScheme: "light", reducedMotion: "reduce" },
];

const routes = [
  { name: "actions-list", path: `/${repo}/actions` },
  { name: "workflow", path: `/${repo}/actions/workflows/${workflow}` },
  { name: "caches", path: `/${repo}/actions/caches` },
  { name: "attestations", path: `/${repo}/actions/attestations` },
  { name: "runners", path: `/${repo}/actions/runners` },
  { name: "usage-metrics", path: `/${repo}/actions/metrics/usage` },
  { name: "performance-metrics", path: `/${repo}/actions/metrics/performance` },
];

if (runIndex) {
  routes.push(
    { name: "run-summary", path: `/${repo}/actions/runs/${runIndex}` },
    { name: "step-log", path: `/${repo}/actions/runs/${runIndex}/${stepPath}` },
  );
} else {
  console.warn("SHITHUB_ACTIONS_RUN is unset; skipping run summary and step log screenshots.");
}

for (const extra of parseExtraRoutes(process.env.SHITHUB_ACTIONS_AUDIT_ROUTES || "")) {
  routes.push(extra);
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});

async function main() {
  await fs.mkdir(outRoot, { recursive: true });
  const browser = await puppeteer.launch({
    headless: "new",
    args: ["--no-sandbox", "--disable-dev-shm-usage"],
  });
  try {
    const page = await browser.newPage();
    await maybeLogin(page);
    await page.close();

    const manifest = {
      baseURL,
      repo,
      workflow,
      runIndex,
      capturedAt: new Date().toISOString(),
      routes: [],
    };

    for (const route of routes) {
      for (const viewport of viewports) {
        const page = await browser.newPage();
        await page.setViewport({ width: viewport.width, height: viewport.height, deviceScaleFactor: 1 });
        await page.emulateMediaFeatures([
          { name: "prefers-color-scheme", value: viewport.colorScheme },
          { name: "prefers-reduced-motion", value: viewport.reducedMotion },
        ]);

        const url = baseURL + route.path;
        const response = await page.goto(url, { waitUntil: "networkidle2", timeout: 45000 });
        const status = response ? response.status() : 0;
        await page.keyboard.press("Tab");
        await page.keyboard.press("Tab");
        const audit = await page.evaluate(() => {
          const doc = document.scrollingElement || document.documentElement;
          const active = document.activeElement;
          const graph = document.querySelector("[data-actions-graph-shell]");
          return {
            title: document.title,
            scrollWidth: doc.scrollWidth,
            clientWidth: doc.clientWidth,
            activeTag: active ? active.tagName.toLowerCase() : "",
            activeText: active ? (active.textContent || active.getAttribute("aria-label") || "").trim().slice(0, 80) : "",
            hasGraphShell: Boolean(graph),
            graphNodeCount: document.querySelectorAll("[data-actions-graph-node]").length,
            hasGraphPopover: Boolean(document.querySelector("[data-actions-graph-popover]")),
            hasLogOutput: Boolean(document.querySelector(".shithub-actions-log-output")),
          };
        });

        const name = `${slug(route.name)}-${viewport.name}`;
        const screenshotPath = path.join(outRoot, `${name}.png`);
        await page.screenshot({ path: screenshotPath, fullPage: true });
        await page.close();

        const record = {
          route: route.name,
          path: route.path,
          viewport: viewport.name,
          status,
          screenshot: path.relative(outRoot, screenshotPath),
          audit,
        };
        manifest.routes.push(record);
        console.log(`${status} ${viewport.name} ${route.path} -> ${screenshotPath}`);
        if (status >= 400) {
          process.exitCode = 1;
        }
        if (audit.scrollWidth > audit.clientWidth + 1 && !audit.hasGraphShell) {
          console.warn(`horizontal overflow outside graph shell: ${route.path} ${viewport.name}`);
          process.exitCode = 1;
        }
      }
    }

    await fs.writeFile(path.join(outRoot, "manifest.json"), JSON.stringify(manifest, null, 2) + "\n");
    console.log(`wrote ${path.join(outRoot, "manifest.json")}`);
  } finally {
    await browser.close();
  }
}

async function maybeLogin(page) {
  const user = process.env.SHITHUB_USER || "";
  const pass = process.env.SHITHUB_PASS || "";
  if (!user || !pass) {
    return;
  }
  await page.goto(baseURL + "/login", { waitUntil: "networkidle2", timeout: 45000 });
  const userSelector = await firstSelector(page, [
    'input[name="login"]',
    'input[name="username"]',
    'input[name="email"]',
  ]);
  const passSelector = await firstSelector(page, ['input[name="password"]']);
  if (!userSelector || !passSelector) {
    console.warn("login form selectors not found; continuing unauthenticated");
    return;
  }
  await page.type(userSelector, user);
  await page.type(passSelector, pass);
  await Promise.all([
    page.waitForNavigation({ waitUntil: "networkidle2", timeout: 45000 }).catch(() => null),
    page.click('button[type="submit"], input[type="submit"]'),
  ]);
}

async function firstSelector(page, selectors) {
  for (const selector of selectors) {
    if (await page.$(selector)) {
      return selector;
    }
  }
  return "";
}

function normalizeWorkflow(value) {
  return value.replace(/^\.shithub\/workflows\//, "").replace(/^\/+/, "");
}

function parseExtraRoutes(raw) {
  if (!raw.trim()) {
    return [];
  }
  return raw.split(",").map((item, index) => {
    const [name, routePath] = item.includes("=") ? item.split(/=(.*)/s, 2) : [`extra-${index + 1}`, item];
    return { name: name.trim(), path: routePath.trim() };
  }).filter((route) => route.path.startsWith("/"));
}

function slug(value) {
  return value.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-|-$/g, "") || "route";
}
