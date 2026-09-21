## Test Review — VERDICT: APPROVE

**Round:** 1  ·  **Spec:** docs/specs/17-expiry-notifications.md  ·  **Issue:** #93
**Suite:** `COMPOSE_PROJECT_NAME=inventory-w2-notifications docker compose run --rm app go test ./...` → exit 1 overall. The only failure is the pre-existing `internal/vision` `TestLiveAnalyzeProductHonoursContract` (real Gemini API returned 429, issue #113 — CI skips it with `GEMINI_API_KEY` unset). Every PR-touched package reports `ok`: `internal/expiry` (0.009s), `internal/notify` (0.043s), `internal/store` (13.6s), `internal/httpapi` (1.76s).

### Verified against the specific failure scenarios in the task

- **Stored token returned by a read / embedded in `last_result`:** `internal/httpapi/notifications_test.go:106` (`TestGetNotificationSettingsNeverReturnsTheToken`, asserts on raw response bytes, not just the decoded struct) and `:124` (`TestGetNotificationSettingsLastResultCarriesNoToken`). Backed by `internal/notify/deliver_test.go:193` and `:211` (`TestDeliveryFailureSummaryNeverCarriesTheToken`, `TestUnreachableTargetSummarisesWithoutTheURL`) which assert the curated `Summary()` excludes the token/URL while `Error()` (logged only) still has it — a real regression test, not a tautology.
- **Redirect followed:** `internal/notify/deliver_test.go:169` (`TestDeliverTreatsARedirectAsFailureAndDoesNotFollowIt`) stands up a second `httptest.Server` and asserts `elsewhere.requests` is empty — this fails loudly if `CheckRedirect` regresses to the Go default.
- **Storage notified twice in a day / while disabled / with no settings row:** `internal/store/notifications_test.go:124` (`TestClaimDueNotificationsClaimsOncePerDay`, real DB, second same-hour claim returns nothing, next day's claim does) and `:165` (`TestClaimDueNotificationsSkipsDisabledAndOtherHours`). At the service layer, `internal/notify/service_test.go:151` (`TestRunDueSendsNothingWhenNothingIsDue`) and the disabled-row 409 at `internal/httpapi/notifications_test.go:246` (`TestNotificationTestRefusesWhileDisabled`, asserting `f.notifier.calls == 0`, not just the status code).
- **Digest including another storage's rows / a consumed-to-zero batch / an image reference:** `internal/store/notifications_test.go:232` (`TestExpiringBatchesBeforeIsScopedAndResolvesPaths`) builds a neighbour storage with an identically-dated batch and asserts `len(items) == 1`, excluding both the neighbour and the zero-quantity batch by real SQL, not a fake. `internal/notify/digest_test.go:66` (`TestMessageCarriesTheSpecifiedFieldsAndNothingElse`) scans the rendered message for `http://`, `.jpg`, `.png`, `image`, `/api/` and asserts none appear — and the wire types (`DigestItem`, `payloadItem`) structurally have no image field at all.
- **`include_soon = false` still producing a non-empty digest from soon-only items:** `internal/notify/digest_test.go:55` (`TestBuildHonoursIncludeSoon`) builds a digest from soon-only items with the flag off and asserts `digest.Empty()` — this is exactly the bug scenario, not just a happy-path check. Mirrored at the service layer by `internal/notify/service_test.go:126` (`TestRunDueRespectsIncludeSoon`), which also asserts nothing left the process (`target.requests` empty).

### Coverage vs acceptance criteria (issue #93)

- [x] No row / `enabled = FALSE` → scheduler does nothing, no code path sends — `store` claim tests + `service` `TestRunDueSendsNothingWhenNothingIsDue` + `httpapi` 409 on the test button.
- [x] At most once per day, at the configured hour, only when a bucket is non-empty — `TestClaimDueNotificationsClaimsOncePerDay`, `TestRunDueSendsNothingWhenEveryBucketIsEmpty`.
- [x] Buckets match spec 08's classification, one definition — `internal/expiry/classify_test.go` walks every boundary of the spec's table plus a real cross-timezone case (`TestClassifyComparesCalendarDaysAcrossZones`), and `notify.Build` calls `expiry.Classify` directly rather than reimplementing the windows.
- [x] Messages carry only name/quantity/location/date of the configured storage, never images/other storages/credentials — digest + store tests above, plus `TestDeliverCarriesNoCredentialsOfItsOwn` (no `Cookie`, no `Authorization` without a token, no `X-Api-Key`).
- [x] Test endpoint reports inline and writes `last_result` — `TestNotificationTestSendsAndReportsInline`, `TestNotificationTestReportsAFailureInline`.
- [x] Stored token never returned by any read — see above.
- [x] Redirects treated as failure, not followed — see above.
- [x] Disabling / deleting the storage stops delivery immediately — `TestClaimDueNotificationsSkipsDisabledAndOtherHours`, `TestDeletingAStorageRemovesItsNotificationSettings` (real `ON DELETE CASCADE` against the schema).

Storage scoping (404-not-403) is checked on all three routes at once against the real router/gate chain, not a handler called directly: `internal/httpapi/notifications_test.go:271` (`TestNotificationRoutesAreStorageScoped`) drives `newAPIFixture`, which wires `NewRouter` with real `RequireStorageMember` middleware (confirmed via `internal/httpapi/locations_test.go`'s fixture, which every httpapi test in this package shares) — asserts identical `not_found` code and no `"forbidden"` substring for both an unknown id and one the caller isn't a member of, and that neither `SaveNotificationSettings` nor `SendTest` was ever called.

### Should fix (non-blocking)

- `internal/store/notifications_test.go:81` (`TestSaveNotificationSettingsRejectsNonsense`) only exercises the upper `send_hour` boundary (`24`); a broken `< 0` check (e.g. accidentally `<= 0`) would not be caught. Low risk since the same two-sided comparison is duplicated verbatim in `internal/httpapi/notifications.go:181`, but a single negative-hour case at either layer would close it.

### Not verified

- Spec 08 states the dashboard's urgency classification is computed **client-side**; I could not find any existing JS implementation of it in `web/static/js` to check parity against, so I cannot independently confirm the Go `expiry.Classify` windows agree with a dashboard that, as far as I can find in this repo, doesn't yet compute urgency client-side. This is not something the current PR's tests could be expected to cover in JS either, and the Go side is now the single, spec-table-tested implementation.
- The hourly scheduler goroutine (`cmd/inventory/main.go`'s `runExpiryNotifications`/`nextTopOfHour`) has no dedicated unit test, but its logic is thin plumbing around `Service.RunDue`, which is thoroughly tested; not run as a live 24h scheduler here.
