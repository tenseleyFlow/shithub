import encoding from "k6/encoding";
import http from "k6/http";
import { Counter, Rate, Trend } from "k6/metrics";
import { fail, sleep } from "k6";

const baseURL = (__ENV.SHITHUB_BASE_URL || "").replace(/\/+$/, "");
const runnerTokens = (__ENV.SHITHUB_RUNNER_TOKENS || "")
  .split(",")
  .map((token) => token.trim())
  .filter(Boolean);
const runnerLabels = (__ENV.SHITHUB_RUNNER_LABELS || "self-hosted,linux,ubuntu-latest")
  .split(",")
  .map((label) => label.trim())
  .filter(Boolean);
const runnerCapacity = parseInt(__ENV.SHITHUB_RUNNER_CAPACITY || "17", 10);
const logBytes = Math.min(parseInt(__ENV.SHITHUB_ACTIONS_LOG_BYTES || "4096", 10), 512 * 1024);
const idleSleepSeconds = parseFloat(__ENV.SHITHUB_ACTIONS_IDLE_SLEEP || "20");

if (!baseURL) {
  throw new Error("SHITHUB_BASE_URL is required");
}
if (runnerTokens.length === 0) {
  throw new Error("SHITHUB_RUNNER_TOKENS must contain at least one runner registration token");
}

export const options = {
  scenarios: {
    actions_jobs: {
      executor: "constant-vus",
      vus: parseInt(__ENV.SHITHUB_ACTIONS_VUS || "50", 10),
      duration: __ENV.SHITHUB_ACTIONS_DURATION || "10m",
    },
  },
  thresholds: {
    api_errors: ["count==0"],
    job_failures: ["count==0"],
    log_append_duration: ["p(99)<5000"],
    successful_job_rate: ["rate>0.95"],
  },
};

const claimedJobs = new Counter("claimed_jobs");
const noJobHeartbeats = new Counter("no_job_heartbeats");
const completedJobs = new Counter("completed_jobs");
const jobFailures = new Counter("job_failures");
const apiErrors = new Counter("api_errors");
const successfulJobRate = new Rate("successful_job_rate");
const logAppendDuration = new Trend("log_append_duration");

export default function () {
  const registrationToken = runnerTokens[(__VU - 1) % runnerTokens.length];
  const claim = heartbeat(registrationToken);
  if (!claim) {
    sleep(idleSleepSeconds + Math.random() * idleSleepSeconds);
    return;
  }

  claimedJobs.add(1);
  executeClaim(claim);
}

function heartbeat(registrationToken) {
  const res = http.post(
    `${baseURL}/api/v1/runners/heartbeat`,
    JSON.stringify({ labels: runnerLabels, capacity: runnerCapacity }),
    jsonParams(registrationToken),
  );
  if (res.status === 204) {
    noJobHeartbeats.add(1);
    return null;
  }
  if (res.status !== 200) {
    apiErrors.add(1);
    fail(`heartbeat returned ${res.status}: ${res.body}`);
  }
  return parseJSON(res, "heartbeat claim");
}

function executeClaim(claim) {
  const job = claim.job;
  let token = claim.token;
  try {
    token = postJob(job.id, "status", token, { status: "running" }, 200).next_token;

    for (const step of job.steps || []) {
      token = postJob(job.id, `steps/${step.id}/status`, token, { status: "running" }, 200).next_token;
      if (step.run) {
        const logResult = appendLogs(job, step, token);
        token = logResult.token;
        if (logResult.cancelled) {
          successfulJobRate.add(true);
          return;
        }
      }
      token = postJob(
        job.id,
        `steps/${step.id}/status`,
        token,
        { status: "completed", conclusion: "success" },
        200,
      ).next_token;
    }

    postJob(job.id, "status", token, { status: "completed", conclusion: "success" }, 200);
    completedJobs.add(1);
    successfulJobRate.add(true);
  } catch (err) {
    jobFailures.add(1);
    successfulJobRate.add(false);
    throw err;
  }
}

function appendLogs(job, step, token) {
  let next = token;
  for (let seq = 0; seq < 3; seq += 1) {
    const chunk = logChunk(job, step, seq);
    const res = http.post(
      `${baseURL}/api/v1/jobs/${job.id}/logs`,
      JSON.stringify({
        seq,
        step_id: step.id,
        chunk: encoding.b64encode(chunk, "std"),
      }),
      jsonParams(next),
    );
    logAppendDuration.add(res.timings.duration);
    if (res.status !== 202) {
      apiErrors.add(1);
      fail(`log append returned ${res.status}: ${res.body}`);
    }
    next = parseJSON(res, "log append").next_token;

    if (seq === 1) {
      const cancel = postJob(job.id, "cancel-check", next, {}, 200);
      next = cancel.next_token;
      if (cancel.cancelled) {
        next = postJob(
          job.id,
          `steps/${step.id}/status`,
          next,
          { status: "cancelled", conclusion: "cancelled" },
          200,
        ).next_token;
        postJob(job.id, "status", next, { status: "cancelled", conclusion: "cancelled" }, 200);
        return { token: next, cancelled: true };
      }
    }
  }
  return { token: next, cancelled: false };
}

function postJob(jobID, path, token, body, expectedStatus) {
  const res = http.post(
    `${baseURL}/api/v1/jobs/${jobID}/${path}`,
    JSON.stringify(body),
    jsonParams(token),
  );
  if (res.status !== expectedStatus) {
    apiErrors.add(1);
    fail(`${path} returned ${res.status}: ${res.body}`);
  }
  return parseJSON(res, path);
}

function parseJSON(res, name) {
  try {
    return res.json();
  } catch (err) {
    apiErrors.add(1);
    fail(`${name} returned invalid JSON: ${err}`);
  }
}

function jsonParams(token) {
  return {
    headers: {
      Authorization: `Bearer ${token}`,
      "Content-Type": "application/json",
    },
  };
}

function logChunk(job, step, seq) {
  const prefix = `vu=${__VU} iter=${__ITER} run=${job.run_id} job=${job.id} step=${step.id} seq=${seq}\n`;
  if (prefix.length >= logBytes) {
    return prefix.slice(0, logBytes);
  }
  return prefix + ".".repeat(logBytes - prefix.length);
}
