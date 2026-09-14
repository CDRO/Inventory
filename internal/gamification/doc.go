// Package gamification implements the scoring, progress tracking, quests and
// UI described in docs/specs/50-gamification-overview.md through
// docs/specs/52-gamification-quests-and-ui.md.
//
// Specs numbered 50 and above are a later phase: the inventory system in
// specs 00-11 must stay complete, correct, and shippable with this entire
// package unimplemented (see the phase marker in
// docs/specs/50-gamification-overview.md). At the point this package was
// created (spec 50), nothing outside it referenced it yet; boundary_test.go
// checks that automatically rather than leaving it as an unenforced
// convention.
//
// Specs 51 and 52 added the deliberate integration points that convention
// allows: progress, quest and achievement API routes mounted on the existing
// router, and XP/quest-progress increments recorded in the same transaction
// as the inventory or product write that earned them. Adding a further one
// means updating boundary_test.go's allowlist to name it specifically —
// never deleting the check — so an undeclared new dependency elsewhere still
// gets caught.
package gamification
