package main

import "testing"

func TestVersionWarning(t *testing.T) {
	for _, tc := range []struct {
		client, node string
		warn         bool
	}{
		{"v0.2.0", "v0.2.0", false},
		{"v0.2.0", "v0.1.1", true},
		{"dev", "v0.1.1", false},
		{"v0.2.0", "unknown (older than this client)", false},
	} {
		if got := versionWarning(tc.client, tc.node) != ""; got != tc.warn {
			t.Errorf("versionWarning(%q, %q) warns = %v, want %v", tc.client, tc.node, got, tc.warn)
		}
	}
}
