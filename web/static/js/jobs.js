// Shared background-job polling helper, used by every photo-driven feature —
// shelf ingestion, shopping-list photos, consumption photos — rather than
// three separate implementations (docs/specs/05-frontend-pwa-foundations.md).
//
// A job moves through `pending` → `done` | `failed`
// (docs/specs/04-backend-api-conventions.md); this module polls until one of
// the terminal states is reached and resolves or rejects accordingly. Live
// polling is used only when the caller chooses to wait on the upload screen —
// it is a convenience, never the only way to reach a result, since the
// underlying job stays queryable indefinitely
// (docs/specs/06-vision-shelf-ingestion.md).

import { get, post, ApiError } from "./api.js";

const POLL_INTERVAL_MS = 1500;

/**
 * reanalyzeJob asks for a finished job's photo to be analysed again —
 * "Analyze again" (docs/specs/09-consumption-logging.md). The whole proposal is
 * replaced, corrections included. It resolves once the job is pending again;
 * pollJob then waits for the new proposal exactly as it did after upload.
 *
 * @param {string} storageId
 * @param {string} jobId
 * @returns {Promise<{job_id: string}>}
 * @throws {ApiError} 409 for a job still being analysed, already applied, or
 *   with no photo; 503 model_unavailable when the vision model is gone.
 */
export function reanalyzeJob(storageId, jobId) {
  return post(`/api/storages/${storageId}/jobs/${jobId}/reanalyze`);
}

/**
 * reanalyzeFailureMessage turns a failed reanalyzeJob into what the reviewer
 * reads. The proposal on screen is untouched by a refusal, so each message says
 * why, not that anything was lost.
 *
 * @param {unknown} err
 * @returns {string}
 */
export function reanalyzeFailureMessage(err) {
  if (!(err instanceof ApiError)) {
    return "Could not reach the server. Check your connection and try again.";
  }
  if (err.code === "model_unavailable") {
    return "The AI model is unavailable, so this photo cannot be analysed again right now. Ask an admin to choose another model.";
  }
  if (err.status === 409) {
    return "This photo cannot be analysed again right now: it is already being analysed, or its proposal has been applied. Reload to see where it stands.";
  }
  return err.message;
}

/** JobFailedError is thrown when a job reaches status "failed". */
export class JobFailedError extends Error {
  /** @param {{id: string, status: string, error?: string}} job */
  constructor(job) {
    super(job.error || "The job failed.");
    this.name = "JobFailedError";
    this.job = job;
  }
}

/**
 * pollJob polls `GET /api/storages/{storageId}/jobs/{jobId}` every 1.5s until
 * the job is `done` or `failed`.
 *
 * @param {string} storageId
 * @param {string} jobId
 * @param {Object} [options]
 * @param {(job: Object) => void} [options.onUpdate] - called with the raw job
 *   on every poll, including the ones that are still `pending`, so a caller
 *   can render a loading state; and again on the terminal poll, before the
 *   promise settles.
 * @param {AbortSignal} [options.signal] - stops polling and rejects with an
 *   AbortError. Not in the original spec text, but a page that navigates away
 *   mid-poll needs a way to stop asking; without it the polling loop outlives
 *   the page that started it.
 * @returns {Promise<any>} resolves with `job.payload` once `status === "done"`.
 * @throws {JobFailedError} when `status === "failed"`.
 */
export function pollJob(storageId, jobId, { onUpdate, signal } = {}) {
  return new Promise((resolve, reject) => {
    let stopped = false;
    let timer = null;

    function stop(reason) {
      if (stopped) return;
      stopped = true;
      if (timer != null) clearTimeout(timer);
      reject(reason);
    }

    if (signal) {
      if (signal.aborted) {
        stop(new DOMException("aborted", "AbortError"));
        return;
      }
      signal.addEventListener("abort", () => stop(new DOMException("aborted", "AbortError")));
    }

    async function tick() {
      if (stopped) return;

      let job;
      try {
        job = await get(`/api/storages/${storageId}/jobs/${jobId}`);
      } catch (err) {
        if (!stopped) {
          stopped = true;
          reject(err);
        }
        return;
      }
      if (stopped) return;

      onUpdate?.(job);

      if (job.status === "done") {
        stopped = true;
        resolve(job.payload);
        return;
      }
      if (job.status === "failed") {
        stopped = true;
        reject(new JobFailedError(job));
        return;
      }

      timer = setTimeout(tick, POLL_INTERVAL_MS);
    }

    tick();
  });
}
