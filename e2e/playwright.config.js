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
          args: [
            "--ignore-certificate-errors",
            // A synthetic camera and an auto-granted permission, for
            // docs/specs/37-in-page-camera.md's viewfinder journeys: no
            // hardware, no permission prompt. Required by that spec, which
            // also requires confirming them live rather than trusting the
            // documentation — this file has been caught once by a documented
            // flag that did not do what it said (see the
            // --unsafely-treat-insecure-origin-as-secure note above).
            //
            // Confirmed live in this image, measured rather than assumed
            // (Playwright v1.48.0-noble, Chromium 130.0.6723.31; the numbers
            // are recorded in the PR that added this):
            //   * getUserMedia({video:{facingMode:"environment"}}) resolves
            //     with no prompt — one video track, label "fake_device_0",
            //     640x480. getSettings() reports width and height but NO
            //     facingMode at all, so nothing may assert on that key here.
            //   * window.ImageCapture exists, and takePhoto() on the fake
            //     track resolves with an **image/png** Blob (3375 bytes) —
            //     which is why camera.js normalises every still to JPEG
            //     rather than relabelling, and why the E2E asserting the
            //     JPEG SOI bytes FF D8 is a real check here and not a
            //     vacuous one: the source bytes genuinely are not JPEG.
            //     Normalised through createImageBitmap -> canvas ->
            //     toBlob("image/jpeg", 0.92) it came out 640x480, 10969
            //     bytes, starting FF D8.
            //   * video.videoWidth stays 0 for ~59 ms after srcObject is
            //     set, so the Shutter's first-frame gate is a real state a
            //     test can observe, not a formality. videoWidth is a
            //     getter on HTMLVideoElement.prototype, which is what lets
            //     a journey pin it at 0 to prove the gate holds.
            //   * Every track's readyState is "ended" after stop().
            //   * enumerateDevices() reports exactly ONE videoinput. That is
            //     the measurement; it is NOT the conclusion #205 drew from it
            //     ("the flip button cannot be driven here"). This same API is
            //     as overridable as getUserMedia is two bullets up, so
            //     ingestion.spec.js drives the flip logic by reporting two.
            //     What stays device-only is whether the second stream is a
            //     genuinely different physical camera — which no amount of
            //     stubbing can answer.
            //   * window.BarcodeDetector is absent, so these flags do not
            //     start docs/specs/20-barcode-recall.md's live scan in any
            //     existing spec: supportsLiveScan() is false either way.
            //     That matters because launchOptions is browser-wide, so
            //     every spec now runs with a camera that resolves instead of
            //     one that does not. Verified rather than reasoned about: the
            //     176 specs that existed before this change all stayed green
            //     on the first run with these flags set.
            "--use-fake-device-for-media-stream",
            "--use-fake-ui-for-media-stream",
          ],
        },
      },
    },
  ],
});
