package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRelativeFromFuture(t *testing.T) {
	now := time.Date(2026, 3, 30, 12, 0, 0, 0, time.UTC)
	target := now.Add(30 * time.Minute)

	got := relativeFrom(now, target)
	want := "in 30m0s"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestRelativeFromPast(t *testing.T) {
	now := time.Date(2026, 3, 30, 12, 0, 0, 0, time.UTC)
	target := now.Add(-5 * time.Minute)

	got := relativeFrom(now, target)
	want := "5m0s ago"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestRelativeFromNow(t *testing.T) {
	now := time.Date(2026, 3, 30, 12, 0, 0, 0, time.UTC)

	got := relativeFrom(now, now)
	want := "now"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestAutoLeaseNameSanitizesInvalidUser(t *testing.T) {
	now := time.Unix(1774878689, 0)
	got := autoLeaseName("tether-LEASE_NAME", now)
	want := "tether-lease-name-1774878689"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestAutoLeaseNameFallbackForEmpty(t *testing.T) {
	now := time.Unix(1774878689, 0)
	got := autoLeaseName("$$$", now)
	want := "user-1774878689"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestAutoLeaseNameTruncatesLongUser(t *testing.T) {
	now := time.Unix(1774878689, 0)
	got := autoLeaseName(strings.Repeat("a", 400), now)

	if !strings.HasSuffix(got, "-1774878689") {
		t.Fatalf("expected timestamp suffix in %q", got)
	}
	if len(got) > 252 {
		t.Fatalf("expected lease name length <= 252, got %d", len(got))
	}
}

func TestIsPlaceholderLeaseName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{name: "LEASE_NAME", want: true},
		{name: "<lease-name>", want: true},
		{name: "your-lease-name", want: true},
		{name: "demo-kind-tether-dev-setup", want: false},
	}

	for _, tc := range cases {
		got := isPlaceholderLeaseName(tc.name)
		if got != tc.want {
			t.Fatalf("isPlaceholderLeaseName(%q): expected %v, got %v", tc.name, tc.want, got)
		}
	}
}

func TestListRecordedSessions(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "dev-session.cast"), []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write cast: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("ignore"), 0o644); err != nil {
		t.Fatalf("write txt: %v", err)
	}

	sessions, err := listRecordedSessions(dir)
	if err != nil {
		t.Fatalf("listRecordedSessions: %v", err)
	}
	if len(sessions) != 1 || sessions[0] != "dev-session" {
		t.Fatalf("unexpected sessions: %#v", sessions)
	}
}

func TestFormatPlaybackOutputTableJSON(t *testing.T) {
	input := `{"kind":"Table","columnDefinitions":[{"name":"Name"},{"name":"Status"},{"name":"Age"}],"rows":[{"cells":["default","Active","1h"]},{"cells":["kube-system","Active","1h"]}]}`
	got := formatPlaybackOutput(input)

	if strings.Contains(got, "\"kind\":\"Table\"") {
		t.Fatalf("expected table text output, got raw json: %q", got)
	}
	if !strings.Contains(got, "Name") || !strings.Contains(got, "kube-system") {
		t.Fatalf("expected rendered table content, got %q", got)
	}
}

func TestFormatPlaybackOutputPassThrough(t *testing.T) {
	input := "$ kubectl get namespaces\n"
	got := formatPlaybackOutput(input)
	if got != input {
		t.Fatalf("expected passthrough output %q, got %q", input, got)
	}
}
