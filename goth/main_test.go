package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"gothstack.com/cli/internal/credentials"
)

type unavailableKeychain struct{}

func (unavailableKeychain) Get(context.Context, string) (string, error) {
	return "", credentials.ErrUnavailable
}
func (unavailableKeychain) Set(context.Context, string, string) error {
	return credentials.ErrUnavailable
}

func TestLoginPreflightAndSafeErrors(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/vnd.gothstack.v1+json")
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"error":"secret-server-diagnostic"}`))
	}))
	defer server.Close()
	for _, tc := range []struct {
		args    []string
		code    int
		network bool
	}{
		{[]string{"help"}, 0, false},
		{[]string{"version"}, 0, false},
		{[]string{"bad-command"}, 2, false},
		{[]string{"login", "--label", "\x1b[31m"}, 2, false},
		{[]string{"login", "--server", "https://user:secret-password@host"}, 2, false},
		{[]string{"login", "--unknown", "secret-option"}, 2, false},
		{[]string{"login", "--server", server.URL, "--storage", "bad"}, 1, false},
		{[]string{"login", "--server", server.URL, "--storage", "file"}, 1, runtime.GOOS != "windows"},
	} {
		before := requests.Load()
		var out, stderr bytes.Buffer
		args := append([]string{}, tc.args...)
		if len(args) > 0 && args[0] == "login" {
			args = append(args, "--config-dir", filepath.Join(t.TempDir(), "credentials"))
		}
		code := run(t.Context(), args, &out, &stderr, unavailableKeychain{})
		if code != tc.code || (requests.Load() > before) != tc.network {
			t.Fatalf("code=%d requests=%d", code, requests.Load()-before)
		}
		for _, secret := range []string{"secret-server-diagnostic", "secret-password", "secret-option", "\x1b"} {
			if strings.Contains(out.String()+stderr.String(), secret) {
				t.Fatal("untrusted input leaked into output")
			}
		}
	}
}
