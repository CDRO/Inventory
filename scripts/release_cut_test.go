// The `cut` job of .github/workflows/release.yml: the release tag made on
// GitHub itself, by a `workflow_dispatch`, instead of by `scripts/dev release`
// on the operator's machine (docs/specs/38-release-pipeline-and-nas-runner.md,
// "Cutting the tag from GitHub").
//
// Two kinds of test. The structural ones pin the guards a tidy-up could delete
// without anything else noticing — the job runs on a dispatch only, from main
// only, is the only job in the file with `contents: write`, and refuses before
// it creates. The behavioural ones run the step's own script, lifted out of the
// YAML, against a recording `gh` stub: the script is the one new piece of logic
// in the pipeline, and "which name does the second release of a day get" or
// "does a red e2e run still get tagged" are questions a `strings.Contains`
// cannot answer. The stubs follow dev_release_test.go's pattern: no GitHub, no
// clone, and the API calls recorded so the tests can say what was created.
package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The commit main's head is "at" in every scenario — forty hex characters,
// what GITHUB_SHA always is.
const cutHead = "2222222222222222222222222222222222222222"

// cutGhStub answers the `gh api` calls the cut script makes and records them.
//
// It is routed on the path and, for the runs endpoint, on the jq filter the
// script passes: the same endpoint is asked twice, once for successful runs
// and once for runs still in progress, and only the filter tells them apart.
// STUB_<WF>_GREEN_AFTER lets a run go green after that many in-progress
// polls, which is how the waiting path is exercised without waiting.
//
// The tag creation's arguments are written one per line to $STUB_DIR/tag-call
// — the message among them, newlines and all, which is what the classic test
// reads back.
const cutGhStub = `#!/bin/sh
printf 'gh %s\n' "$*" >> "$STUB_LOG"
runs() {
  eval "green=\${STUB_${1}_GREEN:-1}; pending=\${STUB_${1}_PENDING:-0}; after=\${STUB_${1}_GREEN_AFTER:-0}"
  polls=$(cat "$STUB_DIR/$1-polls" 2>/dev/null || echo 0)
  case "$2" in
    green)
      if [ "$after" -gt 0 ] && [ "$polls" -lt "$after" ]; then echo 0; else echo "$green"; fi ;;
    pending)
      echo $((polls + 1)) > "$STUB_DIR/$1-polls"
      echo "$pending" ;;
  esac
}
case "$*" in
  "api --paginate "*"/git/matching-refs/tags/"*)
    [ -z "${STUB_EXISTING:-}" ] || printf '%s\n' "$STUB_EXISTING"
    exit 0 ;;
  "api "*"/git/ref/tags/"*)
    [ -n "${STUB_TAG_EXISTS:-}" ] || exit 1
    echo '{"ref":"refs/tags/existing"}'; exit 0 ;;
  "api "*"/actions/workflows/test.yml/runs?"*'conclusion == "success"'*) runs TEST green ;;
  "api "*"/actions/workflows/test.yml/runs?"*'status != "completed"'*) runs TEST pending ;;
  "api "*"/actions/workflows/e2e.yml/runs?"*'conclusion == "success"'*) runs E2E green ;;
  "api "*"/actions/workflows/e2e.yml/runs?"*'status != "completed"'*) runs E2E pending ;;
  "api "*"/git/tags -f tag="*)
    printf '%s\n' "$@" > "$STUB_DIR/tag-call"
    echo "tagobjtagobjtagobjtagobjtagobjtagobjtago"; exit 0 ;;
  "api "*"/git/refs -f ref="*)
    printf '%s\n' "$@" > "$STUB_DIR/ref-call"
    echo "refs/tags/created"; exit 0 ;;
esac
exit 0
`

// cutDateStub stands in for date(1): the release day is whatever the scenario
// says, so that "today's tag" is a fixed string and the tests do not break at
// midnight.
const cutDateStub = `#!/bin/sh
case "$*" in
  "+%Y.%m.%d") echo "${STUB_TODAY:-2026.10.05}" ;;
  *) echo "2026-10-05T18:00:00Z" ;;
esac
`

// cutScript lifts the `run:` block of the step `id: cut` out of release.yml:
// the lines after `run: |` that are blank or indented at least as deep as the
// block, de-indented by the block's own depth. Text-based, like everything
// else in this package that reads a workflow, and loud when the step moves.
func cutScript(t *testing.T) string {
	t.Helper()
	lines := strings.Split(strings.ReplaceAll(workflowFiles(t)["release.yml"], "\r\n", "\n"), "\n")
	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "id: cut" {
			for j := i; j < len(lines) && j < i+40; j++ {
				if strings.TrimSpace(lines[j]) == "run: |" {
					start = j + 1
					break
				}
			}
			break
		}
	}
	if start < 0 {
		t.Fatal("release.yml has no step `id: cut` with a `run: |` block - this test can no longer find the script it exercises")
	}
	indent := len(lines[start]) - len(strings.TrimLeft(lines[start], " "))
	if indent == 0 {
		t.Fatalf("release.yml's cut script does not start with an indented line: %q", lines[start])
	}
	var out []string
	for _, line := range lines[start:] {
		switch {
		case strings.TrimSpace(line) == "":
			out = append(out, "")
		case len(line) >= indent && strings.TrimSpace(line[:indent]) == "":
			out = append(out, line[indent:])
		default:
			return strings.Join(out, "\n") + "\n"
		}
	}
	return strings.Join(out, "\n") + "\n"
}

type cutResult struct {
	exit   int
	stdout string
	stderr string
	calls  string
	dir    string
}

func (r cutResult) called(substr string) bool { return strings.Contains(r.calls, substr) }

// file reads one of the recordings the stubs or the script left in the
// scenario's directory; "" when it was never written.
func (r cutResult) file(name string) string {
	b, err := os.ReadFile(filepath.Join(r.dir, name))
	if err != nil {
		return ""
	}
	return string(b)
}

// runCut runs the lifted script under sh with the stubs on PATH and the
// variables the step would have from the runner, overridden by env. The poll
// interval is zero and the attempts few, so a scenario that waits does not.
func runCut(t *testing.T, env map[string]string) cutResult {
	t.Helper()

	dir := t.TempDir()
	script := filepath.Join(dir, "cut.sh")
	if err := os.WriteFile(script, []byte(cutScript(t)), 0o755); err != nil {
		t.Fatal(err)
	}
	stubs := filepath.Join(dir, "stubs")
	if err := os.MkdirAll(stubs, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"gh": cutGhStub, "date": cutDateStub} {
		if err := os.WriteFile(filepath.Join(stubs, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	vars := map[string]string{
		"PATH":                stubs + string(os.PathListSeparator) + os.Getenv("PATH"),
		"STUB_LOG":            filepath.Join(dir, "calls.log"),
		"STUB_DIR":            dir,
		"GITHUB_REF":          "refs/heads/main",
		"GITHUB_REF_NAME":     "main",
		"GITHUB_SHA":          cutHead,
		"GITHUB_REPOSITORY":   "CDRO/Inventory",
		"GITHUB_OUTPUT":       filepath.Join(dir, "output"),
		"GITHUB_STEP_SUMMARY": filepath.Join(dir, "summary"),
		"INPUT_TAG":           "",
		"INPUT_CLASSIC":       "false",
		"RELEASE_TZ":          "Europe/Zurich",
		"POLL_ATTEMPTS":       "3",
		"POLL_SECONDS":        "0",
	}
	for k, v := range env {
		vars[k] = v
	}
	cmd := exec.Command("sh", script)
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if _, override := vars[k]; !override && !strings.HasPrefix(k, "STUB_") && !strings.HasPrefix(k, "GITHUB_") && !strings.HasPrefix(k, "INPUT_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	for k, v := range vars {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	exit := 0
	if ee, ok := err.(*exec.ExitError); ok {
		exit = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run cut script: %v", err)
	}
	calls, _ := os.ReadFile(vars["STUB_LOG"])
	return cutResult{exit: exit, stdout: stdout.String(), stderr: stderr.String(), calls: string(calls), dir: dir}
}

// tagged reports what the script created: the tag name it wrote to
// $GITHUB_OUTPUT, and the arguments of the tag-object and ref creations.
func tagged(t *testing.T, r cutResult) (output, tagCall, refCall string) {
	t.Helper()
	return r.file("output"), r.file("tag-call"), r.file("ref-call")
}

// The name, when none is given: today's date, and the smallest free counter
// from 1 when the day already has a release - the same rule
// scripts/wellen-orchestrator.ps1's Get-NextReleaseTagName applies, so a
// release cut from GitHub and one cut by the orchestrator never disagree about
// what the second release of a day is called.
func TestReleaseCutNamesTheTagFromTodayAndTheExistingTags(t *testing.T) {
	for _, tc := range []struct {
		name     string
		existing string
		want     string
	}{
		{"first of the day", "", "v2026.10.05"},
		{"second of the day", "refs/tags/v2026.10.05", "v2026.10.05.1"},
		{"third of the day", "refs/tags/v2026.10.05\nrefs/tags/v2026.10.05.1", "v2026.10.05.2"},
		{"a gap is filled, not skipped", "refs/tags/v2026.10.05\nrefs/tags/v2026.10.05.2", "v2026.10.05.1"},
		{"a .n without the plain tag still yields the plain tag", "refs/tags/v2026.10.05.1", "v2026.10.05"},
		{"the prefix match's other days do not count", "refs/tags/v2026.10.050\nrefs/tags/v2026.10.05x", "v2026.10.05"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runCut(t, map[string]string{"STUB_EXISTING": tc.existing})
			if r.exit != 0 {
				t.Fatalf("exit %d, want 0\nstdout:\n%s\nstderr:\n%s", r.exit, r.stdout, r.stderr)
			}
			output, tagCall, refCall := tagged(t, r)
			if !strings.Contains(output, "tag="+tc.want+"\n") {
				t.Errorf("GITHUB_OUTPUT = %q, want tag=%s", output, tc.want)
			}
			if !strings.Contains(tagCall, "tag="+tc.want+"\n") {
				t.Errorf("tag object created with %q, want tag=%s", tagCall, tc.want)
			}
			if !strings.Contains(refCall, "ref=refs/tags/"+tc.want+"\n") {
				t.Errorf("ref created with %q, want ref=refs/tags/%s", refCall, tc.want)
			}
		})
	}
}

// What is created, and how: an annotated tag - a tag OBJECT on main's head
// commit, then the ref naming that object - which is what `git tag -a` and
// `git push` produce, and which is what the gate reads the message from. The
// default message is scripts/dev release's, and without --classic it carries
// no override line.
func TestReleaseCutCreatesAnAnnotatedTagOnMainsHead(t *testing.T) {
	r := runCut(t, map[string]string{"INPUT_TAG": "v2026.10.05"})
	if r.exit != 0 {
		t.Fatalf("exit %d, want 0\nstdout:\n%s\nstderr:\n%s", r.exit, r.stdout, r.stderr)
	}
	_, tagCall, refCall := tagged(t, r)
	for _, want := range []string{"tag=v2026.10.05\n", "object=" + cutHead + "\n", "type=commit\n", "message=Release v2026.10.05\n"} {
		if !strings.Contains(tagCall, want) {
			t.Errorf("tag object created with:\n%s\nwant it to contain %q", tagCall, want)
		}
	}
	if strings.Contains(tagCall, "deploy: classic") {
		t.Errorf("the tag message carries the classic override without --classic:\n%s", tagCall)
	}
	if !strings.Contains(refCall, "sha=tagobjtagobjtagobjtagobjtagobjtagobjtago\n") {
		t.Errorf("the ref does not point at the tag object the API returned:\n%s", refCall)
	}
	// The order is object then ref: a ref to a tag object that does not exist
	// yet is a 422, and the recording shows which came first.
	if obj, ref := strings.Index(r.calls, "/git/tags -f"), strings.Index(r.calls, "/git/refs -f"); obj < 0 || ref < 0 || obj > ref {
		t.Errorf("tag object and ref were created in the wrong order:\n%s", r.calls)
	}
}

// Decision D3's producing half, on this path: `classic: true` puts the exact
// line `deploy: classic` into the message, on a line of its own, which is the
// only form the gate's `grep -qx` reads as the override.
func TestReleaseCutWritesTheClassicOverrideAsALineOfItsOwn(t *testing.T) {
	r := runCut(t, map[string]string{"INPUT_TAG": "v2026.10.05", "INPUT_CLASSIC": "true"})
	if r.exit != 0 {
		t.Fatalf("exit %d, want 0\nstdout:\n%s\nstderr:\n%s", r.exit, r.stdout, r.stderr)
	}
	_, tagCall, _ := tagged(t, r)
	_, after, found := strings.Cut(tagCall, "message=")
	if !found {
		t.Fatalf("no message in the tag creation:\n%s", tagCall)
	}
	message, _, _ := strings.Cut(after, "\nobject=")
	if !strings.HasPrefix(message, "Release v2026.10.05\n") {
		t.Errorf("message = %q, want it to start with the default annotation", message)
	}
	exact := false
	for _, line := range strings.Split(message, "\n") {
		if line == "deploy: classic" {
			exact = true
		}
	}
	if !exact {
		t.Errorf("message = %q, want the line `deploy: classic` on its own", message)
	}
}

// The refusals, each before anything is created - and the ones that come
// before the run lookup also before anything is looked up, in the script's
// order, which is scripts/dev.d/release's order.
func TestReleaseCutRefusesBeforeItCreates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		env    map[string]string
		reason string
		// whether the run lookup may have happened before the refusal
		lookedUp bool
	}{
		{"a dispatch off main", map[string]string{"GITHUB_REF": "refs/heads/feature/x", "GITHUB_REF_NAME": "feature/x"}, "a release is cut from main", false},
		{"a name that is not a release", map[string]string{"INPUT_TAG": "v2"}, "is not a release tag name", false},
		{"a counter that is not all digits", map[string]string{"INPUT_TAG": "v2026.10.05.1rc"}, "is not a release tag name", false},
		{"a tag that already exists", map[string]string{"INPUT_TAG": "v2026.10.05", "STUB_TAG_EXISTS": "1"}, "already exists", false},
		{"no test run at all", map[string]string{"STUB_TEST_GREEN": "0"}, "no successful test.yml run", true},
		{"a red e2e run and no green one", map[string]string{"STUB_E2E_GREEN": "0"}, "no successful e2e.yml run", true},
		{"an e2e run that stays in progress", map[string]string{"STUB_E2E_GREEN": "0", "STUB_E2E_PENDING": "1"}, "still in progress after", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runCut(t, tc.env)
			if r.exit == 0 {
				t.Fatalf("exit 0, want a refusal\nstdout:\n%s", r.stdout)
			}
			if !strings.Contains(r.stdout, "::error::") || !strings.Contains(r.stdout, tc.reason) {
				t.Errorf("stdout does not carry the refusal %q as a workflow error:\n%s\nstderr:\n%s", tc.reason, r.stdout, r.stderr)
			}
			if r.called("/git/tags -f") || r.called("/git/refs -f") {
				t.Errorf("a refused release still created something:\n%s", r.calls)
			}
			if !tc.lookedUp && r.called("/actions/workflows/") {
				t.Errorf("the run lookup happened before a refusal that should precede it:\n%s", r.calls)
			}
		})
	}
}

// A release dispatched right after a merge finds main's own `test` and `e2e`
// runs still going. The script waits for them rather than refusing - and then
// tags, because the commit went green - and it gives up after POLL_ATTEMPTS
// polls rather than hanging the job.
func TestReleaseCutWaitsForARunStillInProgress(t *testing.T) {
	r := runCut(t, map[string]string{
		"STUB_E2E_PENDING":     "1",
		"STUB_E2E_GREEN_AFTER": "2",
		"POLL_ATTEMPTS":        "5",
	})
	if r.exit != 0 {
		t.Fatalf("exit %d, want 0\nstdout:\n%s\nstderr:\n%s", r.exit, r.stdout, r.stderr)
	}
	polls, _ := strconv.Atoi(strings.TrimSpace(r.file("E2E-polls")))
	if polls < 2 {
		t.Errorf("e2e was polled %d time(s) while in progress, want at least 2", polls)
	}
	if !strings.Contains(r.stdout, "still in progress - waiting") {
		t.Errorf("the wait is not reported in the log:\n%s", r.stdout)
	}
	if !r.called("/git/refs -f ref=refs/tags/v2026.10.05 ") {
		t.Errorf("the tag was not created once the run went green:\n%s", r.calls)
	}
}

// --- The structure around the script ------------------------------------------

// The guards on the job itself. `cut` is the one job in the pipeline that
// writes to the repository, and each of these is a line somebody could delete
// while tidying: the event check (without it a tag PUSH would try to cut a
// second tag), the repository/actor guard, the per-job write scope, and the
// refusals' order.
func TestReleaseCutJobKeepsItsGuards(t *testing.T) {
	raw := workflowFiles(t)["release.yml"]
	body := significantLines(raw)
	cut := jobBlock(t, body, "cut")

	if !strings.Contains(body, "workflow_dispatch:") {
		t.Error("release.yml no longer declares `workflow_dispatch` - the tag can no longer be cut from GitHub")
	}
	for _, want := range []struct {
		text string
		why  string
	}{
		{"github.event_name == 'workflow_dispatch'", "the job runs on a dispatch only; a tag push must not cut a second tag"},
		{"github.repository == 'CDRO/Inventory'", "a fork's copy of this file must not write to this repository"},
		{"github.actor == 'CDRO'", "a fork's copy of this file must not write to this repository"},
		{"contents: write", "creating a ref is a write to the git database"},
		{`[ "$GITHUB_REF" = refs/heads/main ]`, "a release is cut from main only"},
		{"runs?head_sha=$SHA", "green runs are looked up for the exact commit, never the newest on the branch"},
		{`select(.conclusion == "success")`, "only a SUCCESSFUL run counts, never a completed red one"},
		{`select(.status != "completed")`, "a run still in progress is waited for rather than read as missing"},
		{"git/matching-refs/tags/$base", "the same-day counter is derived from the tags that exist"},
		{"-f type=commit", "the tag object points at the commit, not at another tag"},
		{`-f ref="refs/tags/$TAG"`, "the ref created is a tag, never a branch"},
		{"v[0-9][0-9][0-9][0-9].[0-9][0-9].[0-9][0-9])", "the plain vYYYY.MM.DD shape is accepted"},
		{"''|*[!0-9]*) ok=0", "and the counter only when it is all digits - the gate's own check, so this job never cuts a tag the gate then refuses"},
	} {
		if !strings.Contains(cut, want.text) {
			t.Errorf("release.yml's `cut` job no longer contains %q - %s", want.text, want.why)
		}
	}

	// The write scope is this job's and nobody else's: once in the whole file,
	// and inside `cut`. The workflow-level block stays read-only.
	if n := strings.Count(body, "contents: write"); n != 1 {
		t.Errorf("`contents: write` appears %d time(s) in release.yml, want exactly once, on the cut job "+
			"(spec 38, going-private checklist: any write scope granted per job)", n)
	}
	head, _, _ := strings.Cut(body, "\njobs:")
	if !strings.Contains(head, "contents: read") {
		t.Error("release.yml's workflow-level permissions no longer say `contents: read` - every job but `cut` inherits them")
	}

	// Refuse, then look up, then create - in that order.
	refusal := strings.Index(cut, "is not a release tag name")
	lookup := strings.Index(cut, "/actions/workflows/")
	create := strings.Index(cut, `"$api/git/tags"`)
	if refusal < 0 || lookup < 0 || create < 0 || !(refusal < lookup && lookup < create) {
		t.Errorf("release.yml's cut job is not shape check (%d) -> run lookup (%d) -> tag creation (%d); "+
			"a refusal has to come before anything is looked up, and the lookup before anything exists", refusal, lookup, create)
	}
}

// How the tag reaches the rest of the run. On a tag push `github.ref_name` IS
// the tag; on a dispatch it is `main`, and the gate's shape check would refuse
// it - so the gate takes the name from `cut` on that path, and the deploy job
// takes it from the gate on both. Point either back at `github.ref_name` and a
// dispatched release fails at the gate (the benign failure) or deploys and then
// asserts that /healthz reports "main" (the confusing one).
func TestReleaseGateAndDeployTakeTheTagFromTheCutJob(t *testing.T) {
	body := significantLines(workflowFiles(t)["release.yml"])
	gate := jobBlock(t, body, "gate")
	deploy := jobBlock(t, body, "deploy")

	for _, want := range []struct {
		text string
		why  string
	}{
		{"needs: cut", "the gate waits for the tag to exist"},
		{"needs.cut.result == 'success' || needs.cut.result == 'skipped'", "a tag push (cut skipped) and a dispatch (cut succeeded) both reach the gate; a refused cut does not"},
		{"RELEASE_TAG: ${{ github.event_name == 'workflow_dispatch' && needs.cut.outputs.tag || github.ref_name }}", "the tag is cut's on a dispatch and the pushed ref's name otherwise"},
		{`TAG="$RELEASE_TAG"`, "the shape check and the resolution read that name"},
		{`echo "tag=$TAG"`, "and hand it on as a job output"},
	} {
		if !strings.Contains(gate, want.text) {
			t.Errorf("release.yml's `gate` job no longer contains %q - %s", want.text, want.why)
		}
	}
	for _, want := range []struct {
		text string
		why  string
	}{
		{"TAG: ${{ needs.gate.outputs.tag }}", "the deploy job deploys and asserts the tag the gate resolved"},
	} {
		if !strings.Contains(deploy, want.text) {
			t.Errorf("release.yml's `deploy` job no longer contains %q - %s", want.text, want.why)
		}
	}
	if strings.Contains(deploy, "GITHUB_REF_NAME") || strings.Contains(deploy, "github.ref_name") {
		t.Error("release.yml's `deploy` job reads github.ref_name again - on a dispatched release that is `main`, not the tag")
	}
}
