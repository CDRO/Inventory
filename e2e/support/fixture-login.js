// Logging in as a fixture user, with an assertion message that names the cause
// of its own failure (#382).
//
// This lives outside e2e/specs/ on purpose: playwright.config.js sets
// `testDir: "./specs"`, so a module here is imported by specs but never
// collected as one.
//
// Why it exists. On a loaded developer machine the local gate produced mass
// failures on a FRESH stack on an UNMODIFIED commit — 28 failed and 5 did not
// run in one measured run, against a commit CI passed 191/191 twice — and the
// most common single symptom was
//
//   expect(login.status(), "fixture login").toBe(200)
//
// receiving 404, for fixture users that were provably seeded. Read literally
// that looks like a seeding problem, and every session that hit it went and
// checked the seed first. It is not one: the application answers 401 for a
// credential it refuses and 401 for a user that does not exist, so it never
// answers 404 here at all. A 404 on this route is Traefik answering with no
// backend matched — the app container was not there to route to, because it
// was restarting, or because another checkout had just recreated the stack
// underneath the run.
//
// So the status code already distinguishes "the stack is broken" from "the
// fixtures are wrong" perfectly well; what was missing was anything saying so
// at the point of failure. That is all this module adds.
import { expect } from "@playwright/test";

// Every fixture user in e2e/fixtures/seed.sql shares one password.
export const FIXTURE_PASSWORD = "e2e-fixture-password";

// whyLoginFailed is the assertion message. It is built eagerly (Playwright
// takes the message, not a thunk) and is only ever displayed on failure, so
// the 200 case's wording is deliberately the short one.
export function whyLoginFailed(username, status) {
  const head = `fixture login as ${username}: expected 200, got ${status}`;
  switch (status) {
    case 401:
      return (
        `${head}. 401 is the application refusing the credential, which is the ` +
        `one shape that really does point at the fixtures: either seed.sql was ` +
        `not applied to this stack, or it does not create ${username}, or the ` +
        `password differs from FIXTURE_PASSWORD. It is NOT a stack problem — a ` +
        `missing backend answers 404 here, not 401.`
      );
    case 404:
      return (
        `${head}. 404 on POST /api/auth/login is Traefik answering with no ` +
        `backend matched, so the app container was not reachable — mid-restart, ` +
        `or the stack was recreated under this run. It is NOT a credential ` +
        `rejection and NOT a missing fixture user: the application answers 401 ` +
        `for both of those. Reset the stack and re-run (scripts/dev e2e).`
      );
    case 502:
    case 503:
    case 504:
      return (
        `${head}. ${status} is Traefik reaching no usable backend: the app is ` +
        `starting, restarting or gone. Same conclusion as a 404 here — this is ` +
        `the stack, not the credential. Reset and re-run (scripts/dev e2e).`
      );
    default:
      return (
        `${head}. Neither 401 (credential refused) nor 404/502/503/504 (no ` +
        `backend), so this is neither of the two usual causes — read the app's ` +
        `own log before concluding anything: ` +
        `docker compose -p inventory-e2e -f docker-compose.e2e.yml logs app`
      );
  }
}

// fixtureLogin posts the login and asserts it succeeded, returning the
// response for the rare caller that wants its headers or body.
export async function fixtureLogin(page, username, password = FIXTURE_PASSWORD) {
  const res = await page.request.post("/api/auth/login", {
    data: { username, password },
  });
  expect(res.status(), whyLoginFailed(username, res.status())).toBe(200);
  return res;
}
