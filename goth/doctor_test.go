package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gothstack.com/cli/internal/doctor"
)

func TestDoctorLocalDoesNotAccessCredentials(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "never-created")
	var out, stderr bytes.Buffer
	collect := func(context.Context) (doctor.Report, error) {
		r := doctor.Report{Schema: 1, OS: "linux", Arch: "amd64", CollectedUnix: time.Now().Unix()}
		for _, id := range []string{"go", "git", "templ", "wails3", "java", "android_sdk", "android_ndk"} {
			r.Tools = append(r.Tools, doctor.Tool{ID: id, Status: "missing"})
		}
		r.Tools = append(r.Tools, doctor.Tool{ID: "xcode", Status: "unsupported"})
		return r, nil
	}
	// A nil keychain would make a credential access fail. Local mode must avoid it.
	code := runDoctor(t.Context(), []string{"--local", "--config-dir", dir}, &out, &stderr, nil, collect)
	if code != 0 || stderr.Len() != 0 || !strings.Contains(out.String(), "Nothing was uploaded") || !strings.Contains(out.String(), "Go             missing") {
		t.Fatal(code, out.String(), stderr.String())
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("local mode touched credential directory")
	}
	out.Reset()
	stderr.Reset()
	code = runDoctor(t.Context(), []string{"--local"}, &out, &stderr, nil, func(context.Context) (doctor.Report, error) {
		return doctor.Report{}, errors.New("/home/private secret diagnostic")
	})
	if code != 1 || strings.Contains(stderr.String(), "secret") || strings.Contains(stderr.String(), "/home") {
		t.Fatal("raw collector error exposed")
	}
	out.Reset()
	stderr.Reset()
	code = runDoctor(t.Context(), []string{"--local"}, &out, &stderr, nil, func(context.Context) (doctor.Report, error) { return doctor.Report{}, context.Canceled })
	if code != 130 {
		t.Fatal("cancellation", code)
	}
}
func TestDoctorLoginRequiredBeforeCollection(t *testing.T) {
	var out, stderr bytes.Buffer
	called := false
	code := runDoctor(t.Context(), []string{"--config-dir", filepath.Join(t.TempDir(), "credentials")}, &out, &stderr, unavailableKeychain{}, func(context.Context) (doctor.Report, error) { called = true; return doctor.Report{}, nil })
	if code != 1 || called {
		t.Fatal("collected without credential", code)
	}
}
