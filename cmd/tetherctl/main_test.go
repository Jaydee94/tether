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

func TestListRecordedSessionsNonexistentDir(t *testing.T) {
	_, err := listRecordedSessions("/nonexistent/dir/12345")
	if err == nil {
		t.Fatal("expected error for nonexistent directory")
	}
}

func TestSplitLines(t *testing.T) {
	cases := []struct {
		input string
		want  int
	}{
		{"line1\nline2\nline3\n", 3},
		{"line1\nline2", 2},
		{"", 0},
		{"\n", 0},
		{"single", 1},
	}
	for _, tc := range cases {
		got := splitLines([]byte(tc.input))
		if len(got) != tc.want {
			t.Fatalf("splitLines(%q): expected %d lines, got %d", tc.input, tc.want, len(got))
		}
	}
}

func TestContainsString(t *testing.T) {
	items := []string{"foo", "bar", "baz"}
	if !containsString(items, "foo") {
		t.Error("expected containsString to find 'foo'")
	}
	if !containsString(items, "baz") {
		t.Error("expected containsString to find 'baz'")
	}
	if containsString(items, "qux") {
		t.Error("expected containsString to not find 'qux'")
	}
	if containsString(nil, "foo") {
		t.Error("expected containsString to return false for nil slice")
	}
}

func TestPlaybackCastValid(t *testing.T) {
	// Minimal asciinema v2 .cast with a header and one output event (no sleep delay).
	cast := `{"version":2,"title":"test session","timestamp":1700000000}
[0.1,"o","hello"]
[0.2,"o"," world"]
`
	err := playbackCast([]byte(cast))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPlaybackCastEmpty(t *testing.T) {
	err := playbackCast([]byte(""))
	if err == nil {
		t.Fatal("expected error for empty recording")
	}
	if !strings.Contains(err.Error(), "empty recording") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestPlaybackCastInvalidHeader(t *testing.T) {
	err := playbackCast([]byte("not-json\n"))
	if err == nil {
		t.Fatal("expected error for invalid header")
	}
	if !strings.Contains(err.Error(), "parsing cast header") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestPlaybackCastSkipsInputEvents(t *testing.T) {
	// "i" (input) events should be skipped silently.
	cast := `{"version":2,"title":"test","timestamp":1700000000}
[0.1,"i","keystroke"]
[0.2,"o","output"]
`
	if err := playbackCast([]byte(cast)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCurrentUserFromKubeconfig(t *testing.T) {
	dir := t.TempDir()
	kubeconfigPath := filepath.Join(dir, "config")
	kubeYAML := `apiVersion: v1
kind: Config
current-context: test-ctx
contexts:
- context:
    cluster: test-cluster
    user: alice
  name: test-ctx
clusters:
- cluster:
    server: https://localhost:6443
  name: test-cluster
users:
- name: alice
  user:
    token: test-token
`
	if err := os.WriteFile(kubeconfigPath, []byte(kubeYAML), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}

	got := currentUser(kubeconfigPath)
	if got != "alice" {
		t.Fatalf("expected 'alice', got %q", got)
	}
}

func TestCurrentUserFallbackEnv(t *testing.T) {
	t.Setenv("USER", "bob")
	// Pass a nonexistent kubeconfig so we fall back to envUser.
	got := currentUser("/nonexistent/kubeconfig")
	if got != "bob" {
		t.Fatalf("expected 'bob', got %q", got)
	}
}

func TestCurrentUserFallbackUnknown(t *testing.T) {
	// Unset USER so envUser returns "unknown".
	t.Setenv("USER", "")
	got := currentUser("/nonexistent/kubeconfig")
	if got != "unknown" {
		t.Fatalf("expected 'unknown', got %q", got)
	}
}

func TestDefaultKubeconfigEnvVar(t *testing.T) {
	t.Setenv("KUBECONFIG", "/custom/kubeconfig")
	got := defaultKubeconfig()
	if got != "/custom/kubeconfig" {
		t.Fatalf("expected '/custom/kubeconfig', got %q", got)
	}
}

func TestDefaultKubeconfigDefault(t *testing.T) {
	t.Setenv("KUBECONFIG", "")
	got := defaultKubeconfig()
	if !strings.HasSuffix(got, filepath.Join(".kube", "config")) {
		t.Fatalf("expected default kube config path, got %q", got)
	}
}

func TestDefaultTokenNamespaceEnvVar(t *testing.T) {
	t.Setenv("TETHER_NAMESPACE", "my-namespace")
	got := defaultTokenNamespace()
	if got != "my-namespace" {
		t.Fatalf("expected 'my-namespace', got %q", got)
	}
}

func TestDefaultTokenNamespaceDefault(t *testing.T) {
	t.Setenv("TETHER_NAMESPACE", "")
	got := defaultTokenNamespace()
	if got != "tether-system" {
		t.Fatalf("expected 'tether-system', got %q", got)
	}
}

func TestPlaybackNotFoundErrorNoDir(t *testing.T) {
	err := playbackNotFoundError("my-lease", "/nonexistent/audit")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "no recording found") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestPlaybackNotFoundErrorWithSessions(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "other-session.cast"), []byte{}, 0o644)

	err := playbackNotFoundError("my-lease", dir)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "other-session") {
		t.Fatalf("expected hint with session names, got: %v", err)
	}
}

func TestPlaybackNotFoundErrorWithDevSession(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "dev-session.cast"), []byte{}, 0o644)

	err := playbackNotFoundError("my-lease", dir)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "dev-session") {
		t.Fatalf("expected dev-session hint, got: %v", err)
	}
}

func TestPlaybackNotFoundErrorEmptyDir(t *testing.T) {
	dir := t.TempDir()
	err := playbackNotFoundError("my-lease", dir)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "no .cast files present") {
		t.Fatalf("expected no .cast files hint, got: %v", err)
	}
}
