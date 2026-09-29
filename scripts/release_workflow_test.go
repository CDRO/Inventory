// The release pipeline's guarantees that live in YAML rather than in code.
//
// `.github/workflows/release.yml` is the one workflow that can cause GitHub to
// run code on the NAS (docs/specs/38-release-pipeline-and-nas-runner.md, H17
// and "Security posture"). Several of its properties are load-bearing and
// would break silently: a second workflow naming the self-hosted runner, a
// dropped repository/actor guard, an `actions/checkout` added to the deploy
// job "so the script can be found". None of those fail a build, none fail the
// E2E suite, and the first evidence of any of them would be a deploy that
// migrated the wrong database or ran somebody's fork's code as root on the
// NAS.
//
// Text assertions rather than a parsed document, for the reason
// cmd/inventory/compose_test.go gives for the compose files: gopkg.in/yaml.v3
// is an indirect dependency of this module, and promoting it to a direct one
// to grep four lines is a worse trade than matching the lines. What is matched
// is therefore kept to strings a human would also grep for.
//
// This file lives in package `scripts` because that is where the repository's
// other tooling tests live and because a directory holding only test files is
// not a package `go build ./...` accepts - the same reason compose_test.go
// lives beside cmd/inventory's code.
package scripts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// workflowsDir is .github/workflows seen from this package's directory.
const workflowsDir = "../.github/workflows"

// workflowFiles reads every workflow, or skips the test when the directory is
// not there at all.
//
// The skip is for exactly one caller: the Dockerfile's builder stage runs
// `go test ./...` against the build context, and `.dockerignore` excludes
// `.github` from it — workflow files have no business in a production image.
// Everywhere the suite is actually a gate (a local `docker compose run --rm
// app go test ./...`, a reviewer's run, and CI's `test` job, all of which
// merge docker-compose.override.yml) the directory is bind-mounted and these
// assertions run. A skip that spread beyond the image build would be a test
// that protects nothing, which is why that mount carries a comment saying so.
func workflowFiles(t *testing.T) map[string]string {
	t.Helper()

	entries, err := os.ReadDir(workflowsDir)
	if os.IsNotExist(err) {
		t.Skipf("%s is not present (the image build's context excludes it) - nothing to check here", workflowsDir)
	}
	if err != nil {
		t.Fatalf("read %s: %v", workflowsDir, err)
	}
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yml") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(workflowsDir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		out[e.Name()] = string(raw)
	}
	if len(out) == 0 {
		t.Fatalf("no workflow files found in %s", workflowsDir)
	}
	return out
}

// Spec 38, "Security posture": after the repository goes private, runner
// groups are not available on a Free plan, so ANY workflow on ANY branch could
// name `runs-on: self-hosted` and land on the NAS runner. Nothing in GitHub's
// configuration prevents that; the containment is that release.yml is the only
// workflow that names it, and that a new one naming it is a review finding.
// This test is that review finding, made mechanical.
// Comment lines are excluded deliberately: test.yml's header explains the
// runner-image job it carries, and prose about the runner is not a workflow
// that can land on it.
func TestReleaseIsTheOnlyWorkflowNamingASelfHostedRunner(t *testing.T) {
	for name, body := range workflowFiles(t) {
		names := false
		for _, line := range strings.Split(body, "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			if strings.Contains(trimmed, "self-hosted") {
				names = true
			}
		}
		if name == "release.yml" {
			if !names {
				t.Error("release.yml no longer names the self-hosted runner - the deploy job cannot reach the NAS")
			}
			continue
		}
		if names {
			t.Errorf("%s names a self-hosted runner. Only release.yml may: spec 38, \"Security posture\" - "+
				"on a private repository without runner groups, this is the only thing keeping other "+
				"workflows off the NAS.", name)
		}
	}
}

// The deploy job runs the clone's own script; a checkout would produce a
// second tree whose bind mounts do not resolve to the real data, and the
// migration would be applied to the wrong database
// (docs/specs/01-architecture-and-deployment.md, "Synology NAS variant").
// There is no checkout anywhere in this workflow - the gate reads the tag
// through the API instead, which is also what keeps an annotated tag's message
// readable (actions/checkout fetches annotated tags peeled).
func TestReleaseWorkflowNeverChecksOutTheRepository(t *testing.T) {
	body := workflowFiles(t)["release.yml"]
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue // the header explains at length why there is none
		}
		if strings.Contains(trimmed, "actions/checkout") {
			t.Errorf("release.yml checks out the repository (%q). The deploy job runs the NAS clone's "+
				"own script; a checkout is a second tree that migrates the wrong database.", trimmed)
		}
	}
}

// jobBlock returns the body of one top-level job of a workflow - the lines
// from `  <name>:` up to the next job at the same two-space indentation.
//
// Scoping matters for every assertion about the deploy job: the
// repository/actor guard is deliberately spelled out TWICE in release.yml,
// once on `gate` and once on `deploy`, and a whole-file `strings.Contains`
// therefore cannot tell "both guards present" from "the one on deploy was
// deleted". The copy on `deploy` is the one that decides whether GitHub runs
// code as root on the NAS.
func jobBlock(t *testing.T, body, name string) string {
	t.Helper()
	// A Windows checkout hands these files back with CRLF: `.gitattributes`
	// normalises *.go, not *.yml. Splitting on "\n" would leave a trailing
	// "\r" on every line and no job header would ever match.
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	start := -1
	for i, line := range lines {
		if line == "  "+name+":" {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("release.yml has no top-level job %q - this test can no longer find what it guards", name)
	}
	for i := start + 1; i < len(lines); i++ {
		line := lines[i]
		// The next top-level job: two spaces, then a key, and nothing else.
		if strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") &&
			strings.HasSuffix(strings.TrimSpace(line), ":") && !strings.HasPrefix(strings.TrimSpace(line), "#") {
			return strings.Join(lines[start:i], "\n")
		}
	}
	return strings.Join(lines[start:], "\n")
}

// significantLines strips blank lines and #-comment lines from a workflow
// body - the same filter TestReleaseIsTheOnlyWorkflowNamingASelfHostedRunner
// and TestReleaseWorkflowNeverChecksOutTheRepository already apply inline,
// pulled out so every guard-string assertion below can share it. Without it,
// a `strings.Contains` against the raw body cannot tell a live guard from a
// comment that merely mentions it: release.yml:249 spells out `!cancelled()`
// while explaining the very guard three lines below, so deleting the real
// line left the explanatory comment standing in for it and the assertion
// green (issue #408).
func significantLines(body string) string {
	// A Windows checkout hands these files back with CRLF; jobBlock's own
	// comment explains why that has to be normalised before splitting.
	var kept []string
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// The guards spec 38's H17 criteria name, each of which is a line somebody
// could reasonably delete while tidying and nothing else would notice.
//
// Each is asserted against the job that actually carries it rather than
// against the whole file, so that deleting it from `deploy` fails this test
// even when an identical line survives elsewhere in the workflow.
func TestReleaseDeployJobKeepsItsGuards(t *testing.T) {
	body := significantLines(workflowFiles(t)["release.yml"])
	deploy := jobBlock(t, body, "deploy")

	// The trigger is the workflow's, not the deploy job's.
	//
	// `v[0-9]*` rather than `v*`, and the difference is load-bearing now that
	// the runner can actually fetch: a tag matching this filter deploys to the
	// NAS for real, so `v-test` or `vendor-pin` must not match. Widening it
	// back would not fail anything else in this repository.
	if !strings.Contains(body, `tags: ["v[0-9]*"]`) {
		t.Error(`release.yml's tag filter is no longer tags: ["v[0-9]*"] - a tag push is the only ` +
			"trigger, and narrowing it to names starting with a digit is what stops a stray " +
			"tag like v-test from deploying to the NAS")
	}

	for _, want := range []struct {
		text string
		why  string
	}{
		{"runs-on: [self-hosted, nas]", "the deploy job runs on the labelled NAS runner"},
		{"environment: production", "every deploy leaves a deployment record and can be given a required reviewer"},
		{"group: nas-deploy", "two tags pushed minutes apart queue instead of interleaving (decision D4)"},
		{"cancel-in-progress: false", "cancelling a deploy mid-migration must never be automatic"},
		{"github.repository == 'CDRO/Inventory'", "a fork's copy of this file must not act on this NAS"},
		{"github.actor == 'CDRO'", "a fork's copy of this file must not act on this NAS"},
		{"--ref \"$TAG\" --auto --backup", "the exact command spec 38 says the deploy job runs"},
		{"DEPLOY_MODE: ${{ needs.gate.outputs.deploy_mode }}", "the tag message's classic override reaches the script"},
	} {
		if !strings.Contains(deploy, want.text) {
			t.Errorf("release.yml's `deploy` job no longer contains %q - %s", want.text, want.why)
		}
	}
}

// The `if:` on `deploy` is the entire mechanism that makes
// `needs: [gate, test, e2e]` stop a red run while still letting a SKIPPED
// reusable call through. `!cancelled()` deliberately overrides GitHub's
// default of skipping a job whose `needs` did not all succeed, which is what
// makes the three `.result` clauses load-bearing rather than decorative:
// delete them - a plausible tidy-up under the belief that `needs:` alone
// enforces success - and a tag whose `test` job went red deploys to the NAS.
// Nothing else in this repository would notice.
func TestReleaseDeployRunsOnlyWhenTheGateAndBothCallsPassed(t *testing.T) {
	body := significantLines(workflowFiles(t)["release.yml"])
	for _, want := range []struct {
		text string
		why  string
	}{
		{"needs: [gate, test, e2e]", "the deploy job waits for the gate AND for both reusable calls"},
		{"!cancelled()", "a cancelled run must not fall through to the NAS"},
		{"needs.gate.result == 'success'", "a failed gate stops the release"},
		{"needs.test.result == 'success' || needs.test.result == 'skipped'", "a red `test` call stops the release; a skipped one does not"},
		{"needs.e2e.result == 'success' || needs.e2e.result == 'skipped'", "a red `e2e` call stops the release; a skipped one does not"},
		{"if: needs.gate.outputs.need_test == 'true'", "`test` runs exactly when the gate found no green run for the commit"},
		{"if: needs.gate.outputs.need_e2e == 'true'", "`e2e` runs exactly when the gate found no green run for the commit"},
	} {
		if !strings.Contains(body, want.text) {
			t.Errorf("release.yml no longer contains %q - %s", want.text, want.why)
		}
	}
}

// Decision D3, the consuming half. `scripts/dev_release_test.go` covers the
// producing half - that `scripts/dev release --classic` writes the exact line
// into the tag message - but the line only forces anything if the workflow
// reads it the way the decision says, and both halves of that are a one-token
// edit away from being silently wrong:
//
//   - the message must be read from the git DATABASE, because
//     `actions/checkout` fetches annotated tags peeled and a checkout-based
//     read would permanently and invisibly disable `--classic`;
//   - the match must be `grep -qx` (whole line). Weakened to `grep -q`, the
//     sentence "not deploy: classic yet" in a release note forces a stack
//     restart - the exact failure the decision spells the rule out to avoid.
func TestReleaseGateReadsTheTagMessageAsSpecifiedByD3(t *testing.T) {
	body := significantLines(workflowFiles(t)["release.yml"])
	for _, want := range []struct {
		text string
		why  string
	}{
		{"git/ref/tags/$TAG", "the tag ref is resolved through the API, not from a checkout"},
		{"git/tags/$obj", "the annotated tag OBJECT is read, which is where the message lives"},
		{"grep -qx 'deploy: classic'", "the override is an exact LINE, never a substring"},
		{"tr -d '\\r'", "a carriage return in the message does not hide the keyword"},
	} {
		if !strings.Contains(body, want.text) {
			t.Errorf("release.yml no longer contains %q - %s", want.text, want.why)
		}
	}
}

// H17: "the job asserts that GET /healthz reports the tag as its version". A
// deploy is not done because the script exited 0 - it is done when the NAS
// serves the tag. Flip this comparison, or drop its `exit 1`, and a release
// that left the previous version serving reports green.
func TestReleaseAssertsHealthzReportsTheTag(t *testing.T) {
	body := significantLines(workflowFiles(t)["release.yml"])
	for _, want := range []struct {
		text string
		why  string
	}{
		{`if [ "$version" != "$GITHUB_REF_NAME" ]; then`, "the version served is compared against the tag that triggered the run"},
		{`"$version" != "$GITHUB_REF_NAME"`, "the comparison is inequality-then-fail, not equality-then-pass"},
		{"/healthz", "the assertion probes the health endpoint"},
	} {
		if !strings.Contains(body, want.text) {
			t.Errorf("release.yml no longer contains %q - %s", want.text, want.why)
		}
	}
	// The `exit 1` has to be inside that branch: an assertion that reports the
	// mismatch and exits 0 is not an assertion.
	_, after, found := strings.Cut(body, `if [ "$version" != "$GITHUB_REF_NAME" ]; then`)
	if !found {
		return // already reported above
	}
	branch, _, closed := strings.Cut(after, "\n          fi")
	if !closed {
		t.Fatalf("release.yml's /healthz comparison has no closing `fi` at the expected indentation - "+
			"this test can no longer tell what is inside the branch:\n%s", after)
	}
	if !strings.Contains(branch, "exit 1") {
		t.Errorf("release.yml reports a /healthz version mismatch but does not fail the job:\n%s", branch)
	}
}

// The gate's whole point is "for that exact SHA", never "the newest run on the
// branch" - the trap test.yml's own header documents. `head_sha=` is the
// query parameter that makes it exact.
func TestReleaseGateLooksRunsUpByTheTagsCommit(t *testing.T) {
	body := significantLines(workflowFiles(t)["release.yml"])
	if !strings.Contains(body, "runs?head_sha=$SHA") {
		t.Error("release.yml's gate no longer filters the run lookup by head_sha - " +
			"a release could be gated by a green run of a different commit")
	}
	if !strings.Contains(body, "actions: read") {
		t.Error("release.yml no longer requests `actions: read` - the gate's run lookup would 403 " +
			"on a repository whose default workflow permission is read-only contents")
	}
}

// The `tags:` glob cannot express the release shape — GitHub's filter patterns
// are not regular expressions — so `v2` and `v1.4.0` still reach the gate. The
// gate rejecting them is the second half of that guard, and it has to happen
// before anything is resolved or looked up: once the deploy job starts, the NAS
// has already been touched. Deleting this case statement would leave a workflow
// that still looks correct and deploys `v2`.
func TestReleaseGateRefusesATagThatIsNotAReleaseTag(t *testing.T) {
	gate := jobBlock(t, significantLines(workflowFiles(t)["release.yml"]), "gate")
	for _, want := range []struct {
		text string
		why  string
	}{
		{"v[0-9][0-9][0-9][0-9].[0-9][0-9].[0-9][0-9])", "the plain vYYYY.MM.DD shape is accepted"},
		{"v[0-9][0-9][0-9][0-9].[0-9][0-9].[0-9][0-9].*)", "the same-day counter vYYYY.MM.DD.n is accepted"},
		{"''|*[!0-9]*) ok=0", "and only when that counter is all digits - the check that keeps this " +
			"gate from being looser than scripts/dev.d/release, which would let the tool cut a tag " +
			"its own pipeline then refuses"},
		{"is not a release tag", "anything else is refused with a message naming what to do about it"},
	} {
		if !strings.Contains(gate, want.text) {
			t.Errorf("release.yml's `gate` job no longer contains %q - %s. Without it a tag like "+
				"v2 or v-test reaches the deploy job and is released to the NAS.", want.text, want.why)
		}
	}
	// The refusal has to precede the lookups, or it refuses a tag the job has
	// already acted on.
	refusal := strings.Index(gate, "is not a release tag")
	lookup := strings.Index(gate, "git/ref/tags/")
	if refusal < 0 || lookup < 0 || refusal > lookup {
		t.Errorf("release.yml's gate resolves the tag before checking its shape "+
			"(refusal at %d, lookup at %d) - the check must come first, so that a bad tag "+
			"costs a failed gate rather than a deploy", refusal, lookup)
	}
}

// The other half of the gate's decision, and the half that fails OPEN when it
// breaks: filtering by `head_sha` asks about the right commit, but only
// `.conclusion == "success"` asks whether that commit was actually green.
// Widen it to `.conclusion != null`, or to `.status == "completed"` - both
// plausible while chasing a flaky lookup - and a commit whose `test` run went
// RED counts as a green run, the reusable calls are skipped as unnecessary,
// and `deploy` puts it on the NAS. The `-gt 0` is what turns the count into
// the decision.
func TestReleaseGateCountsOnlySuccessfulRuns(t *testing.T) {
	gate := jobBlock(t, significantLines(workflowFiles(t)["release.yml"]), "gate")
	for _, want := range []struct {
		text string
		why  string
	}{
		{`select(.conclusion == "success")`, "only a SUCCESSFUL run may satisfy the gate; a red or cancelled run must not"},
		{`-gt 0`, "the count of successful runs is what decides whether the reusable call is skipped"},
	} {
		if !strings.Contains(gate, want.text) {
			t.Errorf("release.yml's `gate` job no longer contains %q - %s. A commit whose test or e2e "+
				"run failed could then be read as green and deployed to the NAS.", want.text, want.why)
		}
	}
}

// The two calls and the two callees have to agree, and a `workflow_call`
// trigger removed from either workflow turns every release into a run that
// fails at the `uses:` rather than into a slower one.
func TestTestAndE2EAreCallableByTheReleaseGate(t *testing.T) {
	files := workflowFiles(t)
	release := significantLines(files["release.yml"])

	for _, wf := range []string{"test.yml", "e2e.yml"} {
		if !strings.Contains(release, "uses: ./.github/workflows/"+wf) {
			t.Errorf("release.yml no longer calls %s as a reusable workflow", wf)
		}
		raw, ok := files[wf]
		if !ok {
			t.Fatalf("%s does not exist", wf)
		}
		body := significantLines(raw)
		if !strings.Contains(body, "workflow_call:") {
			t.Errorf("%s no longer declares `workflow_call`, so release.yml's gate cannot run it "+
				"on a tag whose commit has no green run", wf)
		}
	}
}
