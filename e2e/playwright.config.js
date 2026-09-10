// Playwright configuration for the E2E deployment gate
// (docs/specs/05-frontend-pwa-foundations.md). This file, and everything
// under e2e/, runs only inside the throwaway Playwright container defined in
// docker-compose.e2e.yml — never on the host, and never as part of building
// the application image.
//
// @ts-check
import { defineConfig, devices } from "@playwright/test";

export default defineConfig({
  testDir: "./specs",
  timeout: 30_000,
  fullyParallel: true,
  // No retries: a flaky E2E result must be investigated, not quietly
  // absorbed by trying again. This is a deployment gate — passing on the
  // second attempt is not the same guarantee as passing on the first.
  retries: 0,
  reporter: [["list"]],
  use: {
    baseURL: process.env.BASE_URL || "https://traefik",
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
    // The stack's HTTPS listener uses Traefik's own auto-generated
    // self-signed certificate (docker-compose.e2e.yml) — there is no CA a
    // browser would trust it against, and there does not need to be one for
    // a throwaway test run. Chromium's secure-context determination cares
    // that the scheme is https, not that the certificate is trusted; this
    // flag only suppresses the interstitial warning a real user would click
    // through.
    //
    // The plain-HTTP alternative — Chromium's
    // --unsafely-treat-insecure-origin-as-secure flag — was tried first and
    // did not work: confirmed live, with a raw Playwright script bypassing
    // this config file entirely, that window.isSecureContext stayed false
    // with that flag set, on both the default headless-shell browser and the
    // full "chromium" channel. HTTPS is what actually makes
    // navigator.serviceWorker available.
    ignoreHTTPSErrors: true,
  },
  projects: [
    {
      name: "chromium",
      use: {
        ...devices["Desktop Chrome"],
        launchOptions: {
          // Confirmed live (see the PR's E2E section): `ignoreHTTPSErrors`
          // above covers ordinary page navigation and fetches, but service
          // worker script registration runs its own certificate validation
          // that this context-level option does not reach. Without this
          // flag, register("/sw.js") failed with "SecurityError: ... An SSL
          // certificate error occurred when fetching the script." —
          // everything else on the page loaded fine over the same
          // self-signed certificate. This flag operates at the browser
          // process level, which service worker fetches do respect.
          args: ["--ignore-certificate-errors"],
        },
      },
    },
  ],
});
