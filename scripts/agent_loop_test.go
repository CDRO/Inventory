// scripts/agent_loop_test.go - a thin smoke test for scripts/agent-loop.sh
// (H19, #318), run as part of `docker compose run --rm app go test ./...`.
//
// This is NOT the stub-driven coverage of every stop condition the issue's
// acceptance criteria ask for - that lives in scripts/tests/agent-loop.test.sh,
// which needs `jq` (present in deploy/agent's own image; absent from this
// package's plain dev image, golang:1-alpine - see that file's own header
// for how and where to run it). What this proves, cheaply and without jq:
// the script parses its own arguments and enforces its documented usage
// contract before it ever reaches a check that needs an external tool -
// exactly the same scope scripts/dev_test.go's TestDoctorHelpForwards keeps
// for scripts/doctor, and for the same reason.
package scripts

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

func runAgentLoop(t *testing.T, args ...string) (stdout string, exit int) {
	t.Helper()
	cmd := exec.Command("/bin/sh", append([]string{"agent-loop.sh"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("running agent-loop.sh %v: %v\n%s", args, err, out)
		}
		exit = ee.ExitCode()
	}
	return string(out), exit
}

func TestAgentLoopHelp(t *testing.T) {
	out, exit := runAgentLoop(t, "--help")
	if exit != 0 {
		t.Errorf("--help exit = %d, want 0:\n%s", exit, out)
	}
	if !strings.Contains(out, "scripts/agent-loop.sh") {
		t.Errorf("--help output does not carry the script's own usage:\n%s", out)
	}
	if !strings.Contains(out, "--queue") {
		t.Errorf("--help output does not mention --queue:\n%s", out)
	}
}

func TestAgentLoopRequiresAMode(t *testing.T) {
	out, exit := runAgentLoop(t)
	if exit != 2 {
		t.Errorf("no --wave-file/--wave or --queue: exit = %d, want 2:\n%s", exit, out)
	}
	if !strings.Contains(out, "--queue") {
		t.Errorf("usage error does not mention --queue:\n%s", out)
	}
}

func TestAgentLoopWaveWithoutWaveFileIsRefused(t *testing.T) {
	out, exit := runAgentLoop(t, "--wave", "6")
	if exit != 1 {
		t.Errorf("--wave without --wave-file: exit = %d, want 1:\n%s", exit, out)
	}
	if !strings.Contains(out, "FATAL") || !strings.Contains(out, "--wave-file") {
		t.Errorf("refusal does not name the missing --wave-file:\n%s", out)
	}
}

func TestAgentLoopUnknownArgumentIsAUsageError(t *testing.T) {
	out, exit := runAgentLoop(t, "--not-a-real-flag")
	if exit != 2 {
		t.Errorf("unknown argument: exit = %d, want 2:\n%s", exit, out)
	}
}
