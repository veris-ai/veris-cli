package main

import (
	"strings"
	"testing"
)

func TestPatchBundledCAsNeedsAnImage(t *testing.T) {
	// Without --image there is no container filesystem to scan, and silently
	// accepting the flag would leave the SDK bundles it promises to patch
	// untouched.
	err := cmdRun([]string{"--patch-bundled-cas", "--", "true"})
	if err == nil || !strings.Contains(err.Error(), "--image") {
		t.Fatalf("--patch-bundled-cas without --image must refuse naming --image, got %v", err)
	}
}
