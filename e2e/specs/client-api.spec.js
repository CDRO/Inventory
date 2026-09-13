// The retry-safety and paging conventions of docs/specs/04-backend-api-conventions.md
// and docs/specs/12-client-api-contract.md, against the real stack — in
// particular the real idempotency_records table, whose JSONB column is what a
// replayed body actually round-trips through.

import { test, expect } from "@playwright/test";
import { randomUUID } from "node:crypto";

const HOUSEHOLD = "00000000-0000-7000-8000-000000000010";
const BASE = `/api/storages/${HOUSEHOLD}`;

async function logInAsBob(request) {
  const res = await request.post("/api/auth/login", {
    data: { username: "e2e-bob", password: "e2e-fixture-password" },
  });
  expect(res.status()).toBe(200);
}

test("a retried write with the same Idempotency-Key is applied once", async ({ request }) => {
  await logInAsBob(request);
  const key = randomUUID();
  const name = `Shelf ${key.slice(0, 8)}`;

  const first = await request.post(`${BASE}/locations`, {
    data: { name },
    headers: { "Idempotency-Key": key },
  });
  expect(first.status()).toBe(201);
  const created = await first.json();

  const retry = await request.post(`${BASE}/locations`, {
    data: { name },
    headers: { "Idempotency-Key": key },
  });
  expect(retry.status()).toBe(201);
  expect(retry.headers()["idempotent-replayed"]).toBe("true");
  expect((await retry.json()).id, "the retry returns the original location").toBe(created.id);

  // Exactly one location by that name exists.
  const tree = await (await request.get(`${BASE}/locations`)).json();
  const matches = JSON.stringify(tree).split(`"name":"${name}"`).length - 1;
  expect(matches).toBe(1);

  // The same key for a different request is a client bug, not a replay.
  const reused = await request.post(`${BASE}/locations`, {
    data: { name: `${name} (different)` },
    headers: { "Idempotency-Key": key },
  });
  expect(reused.status()).toBe(422);
});

test("the job inbox answers as a paged collection and is behind the gates", async ({ request, playwright }) => {
  const anonymous = await playwright.request.newContext({
    baseURL: test.info().project.use.baseURL,
    ignoreHTTPSErrors: true,
  });
  const gated = await anonymous.get(`${BASE}/jobs`);
  expect(gated.status()).toBe(401);
  await anonymous.dispose();

  await logInAsBob(request);

  const inbox = await request.get(`${BASE}/jobs`);
  expect(inbox.status()).toBe(200);
  const body = await inbox.json();
  expect(Array.isArray(body.items)).toBe(true);
  expect(body).toHaveProperty("next_cursor", null);

  expect((await request.get(`${BASE}/jobs?limit=0`)).status()).toBe(422);
  expect((await request.get(`${BASE}/jobs?status=finished`)).status()).toBe(422);
});
