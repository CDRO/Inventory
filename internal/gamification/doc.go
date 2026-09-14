// Package gamification implements the scoring, progress tracking, and
// (later) quests and UI described in docs/specs/50-gamification-overview.md
// through docs/specs/52-gamification-quests-and-ui.md.
//
// Specs numbered 50 and above are a later phase: the inventory system in
// specs 00-11 must stay complete, correct, and shippable with this entire
// package unimplemented (see the phase marker in
// docs/specs/50-gamification-overview.md). At the point this package was
// created (spec 50), nothing outside it references it yet; boundary_test.go
// checks that automatically rather than leaving it as an unenforced
// convention.
//
// Spec 51 is expected to add deliberate integration points — progress API
// routes mounted on the existing router, an XP increment recorded in the
// same transaction as an inventory write. When it does, boundary_test.go's
// allowlist should be updated to name those specific, reviewed points rather
// than deleted, so an undeclared new dependency elsewhere still gets caught.
package gamification
