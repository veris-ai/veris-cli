package main

import (
	"strings"
	"testing"

	"github.com/veris-ai/veris-proxy/internal/discovery"
)

// --promote-on-success is a flag on this run's own lifecycle. Pointed at a
// sandbox the run did not create, it would freeze and scrub a world somebody
// else is using.
func TestPromoteOnSuccessNeedsAnEnvironment(t *testing.T) {
	err := cmdRun([]string{
		"--image", "busybox", "--sandbox", "sbx_theirs",
		"--promote-on-success", "--", "true",
	})
	if err == nil || !strings.Contains(err.Error(), "--environment") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "veris-proxy promote --sandbox") {
		t.Errorf("the refusal must name the command that does promote an "+
			"existing sandbox: %v", err)
	}
}

func TestPromoteNeedsASandboxID(t *testing.T) {
	t.Setenv(discovery.EnvSandboxID, "")
	err := cmdPromote(nil)
	if err == nil || !strings.Contains(err.Error(), "--sandbox") {
		t.Fatalf("err = %v", err)
	}
}

func TestPromoteRefusesAnUnknownClockRestore(t *testing.T) {
	err := cmdPromote([]string{"--sandbox", "sbx_9", "--clock-restore", "later"})
	if err == nil || !strings.Contains(err.Error(), "rebase or frozen") {
		t.Fatalf("err = %v", err)
	}
}

// The scrub is the surprising half of a promote, so it is printed rather than
// left to be discovered by a later run missing its deliveries.
func TestPromotionOutputNamesWhatWasScrubbed(t *testing.T) {
	var out strings.Builder
	printPromotion(&out, &discovery.PromoteResult{
		EnvironmentID: "env_1", SandboxID: "sbx_9",
		ClockRestore: "rebase", SizeBytes: 4 << 20,
		CuratorClockRestored: true,
		Scrubbed:             map[string][]string{"stripe": {"deliveries", "requests"}},
	})
	for _, want := range []string{"sbx_9", "env_1", "4 MB", "scrubbed", "deliveries"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output must name %q:\n%s", want, out.String())
		}
	}
}
