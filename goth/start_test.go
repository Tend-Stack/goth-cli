package main

import (
	"bytes"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gothstack.com/cli/internal/credentials"
	"gothstack.com/cli/internal/device"
)

func TestStartInvalidReferenceBeforeCredentials(t *testing.T) {
	for _, ref := range []string{"", "05.01", "../x", "course/lesson/more", "https://elsewhere.example/lesson"} {
		var out, stderr bytes.Buffer
		if code := runStart(t.Context(), []string{ref}, &out, &stderr, nil); code != 2 {
			t.Fatal(ref, code, stderr.String())
		}
	}
}

// The app suite runs this independently built public module against its real
// loopback application handler with a disposable account and published fixture.
func TestStartActualServer(t *testing.T) {
	origin := os.Getenv("GOTHSTACK_CLI_TEST_SERVER")
	if origin == "" {
		t.Skip("application integration fixture not provided")
	}
	u, e := url.Parse(origin)
	if e != nil {
		t.Fatal(e)
	}
	ip, e := netip.ParseAddr(u.Hostname())
	if e != nil || !ip.IsLoopback() {
		t.Fatal("fixture must be literal loopback")
	}
	config := filepath.Join(t.TempDir(), "credentials")
	dir := filepath.Join(t.TempDir(), "pocketpilot")
	keychain := unavailableKeychain{}
	s, e := credentials.Open(t.Context(), config, origin, "file", keychain)
	if e != nil {
		t.Fatal(e)
	}
	grant := device.Grant{AccessToken: os.Getenv("GOTHSTACK_CLI_TEST_TOKEN"), SigningSecret: strings.Repeat("A", 43), TokenID: "cli_01ARZ3NDEKTSV4RRFFQ69G5FAV", TokenType: "Bearer", Scope: device.Scope, ExpiresIn: 3600}
	if e = s.Save(t.Context(), grant, "integration terminal", time.Now().Add(time.Hour), false); e != nil {
		s.Close()
		t.Fatal(e)
	}
	s.Close()
	for _, ref := range []string{"05.1", "cli-course/05.1"} {
		var out, stderr bytes.Buffer
		code := run(t.Context(), []string{"start", ref, "--server", origin, "--storage", "file", "--config-dir", config, "--workspace", dir}, &out, &stderr, keychain)
		if code != 0 || !strings.Contains(out.String(), "Starter files are ready") {
			t.Fatal(code, out.String(), stderr.String())
		}
	}
	b, e := os.ReadFile(filepath.Join(dir, "main.go"))
	if e != nil || string(b) != "package main\n" {
		t.Fatal("starter", string(b), e)
	}
	b, e = os.ReadFile(filepath.Join(dir, ".git", "HEAD"))
	if e != nil || string(b) != "ref: refs/heads/goth-cli-lesson-v1\n" {
		t.Fatal("branch", string(b), e)
	}
	if e = os.WriteFile(filepath.Join(dir, "main.go"), []byte("my saved work"), 0644); e != nil {
		t.Fatal(e)
	}
	var out, stderr bytes.Buffer
	code := run(t.Context(), []string{"start", "05.1", "--server", origin, "--storage", "file", "--config-dir", config, "--workspace", dir}, &out, &stderr, keychain)
	if code != 1 || !strings.Contains(stderr.String(), "conflict") {
		t.Fatal("learner conflict", code, stderr.String())
	}
	b, e = os.ReadFile(filepath.Join(dir, "main.go"))
	if e != nil || string(b) != "my saved work" {
		t.Fatal("learner edit lost")
	}
}
