package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestVersionProjectionDropsRawDiagnostics(t *testing.T) {
	for _, tc := range []struct{ id, raw, want string }{
		{"go", "go version go1.27.3 linux/amd64\n", "1.27.3"},
		{"git", "git version 2.53.1.windows.1\n", "2.53.1.windows.1"},
		{"templ", "v0.3.1100\n", "0.3.1100"},
		{"wails3", "v3.0.0\n", "3.0.0"},
		{"java", "openjdk version \"21.0.9\" 2025-10-21\nOpenJDK Runtime Environment\n", "21.0.9"},
		{"xcode", "Xcode 26.1\nBuild version private-build\n", "26.1"},
		{"go", "failed at /home/private-person/secret-project\n", ""},
		{"templ", "v0.3.1100\x1b[31msecret\n", ""},
		{"java", "java version \"private-person\"\n", ""},
	} {
		if got := parseVersion(tc.id, tc.raw); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.id, got, tc.want)
		}
	}
}
func TestCollectUsesOnlyFixedVersionCommands(t *testing.T) {
	var calls []string
	versions := map[string]string{"go": "go version go1.27.3 linux/amd64", "git": "git version 2.53.0", "templ": "v0.3.1100", "wails3": "v3.0.0", "java": "openjdk version \"21.0.9\""}
	c := collector{os: "linux", arch: "amd64", now: func() time.Time { return time.Unix(1234, 0) }, run: func(_ context.Context, name string, args ...string) (string, string) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return versions[name], "found"
	}}
	r, err := c.collect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !r.Valid() || len(r.Tools) != 8 || r.Tools[7].Status != "unsupported" || r.Tools[5].Status != "missing" {
		t.Fatal("invalid report shape")
	}
	if !reflect.DeepEqual(calls, []string{"go version", "git --version", "templ version", "wails3 version", "java -version"}) {
		t.Fatal(calls)
	}
	raw, _ := json.Marshal(r)
	for _, forbidden := range []string{"PATH", "/home/", "hostname", "secret"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatal("private data in projection")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	calls = nil
	if _, err = c.collect(ctx); !errors.Is(err, context.Canceled) || len(calls) != 0 {
		t.Fatal("canceled collector invoked a tool")
	}
}
func TestSDKSelectionIsBoundedAndNumeric(t *testing.T) {
	root := t.TempDir()
	for version, body := range map[string]string{"9": "Pkg.Revision = 9.0.0\n", "30": "Pkg.Revision=30.0.16248370\n", "bad": "Pkg.Revision=/home/private\n", "duplicate": "Pkg.Revision=99.0.0\nPkg.Revision=100.0.0\n", "empty-duplicate": "Pkg.Revision=\nPkg.Revision=999.0.0\n"} {
		p := filepath.Join(root, "ndk", version)
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "source.properties"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	status, version := readSDK(root, "ndk", "Pkg.Revision")
	if status != "found" || version != "30.0.16248370" {
		t.Fatal(status, version)
	}
	if status, _ = readSDK(root, "platforms", "AndroidVersion.ApiLevel"); status != "missing" {
		t.Fatal(status)
	}
	for i := 0; i < 129; i++ {
		if err := os.MkdirAll(filepath.Join(root, "ndk", fmt.Sprint(i+200)), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if status, _ = readSDK(root, "ndk", "Pkg.Revision"); status != "failed" {
		t.Fatal("unbounded SDK scan", status)
	}
}
func TestProbeEnvironmentAndHelperBounds(t *testing.T) {
	env := strings.Join(probeEnvironment([]string{"PATH=/trusted", "HOME=/private", "GOTOOLCHAIN=auto", "GOFLAGS=-toolexec=evil", "JAVA_TOOL_OPTIONS=-javaagent:evil", "JDK_JAVA_OPTIONS=evil", "_JAVA_OPTIONS=evil"}), "\n")
	if strings.Contains(env, "evil") || strings.Contains(env, "GOTOOLCHAIN=auto") || !strings.Contains(env, "GOTOOLCHAIN=local") || !strings.Contains(env, "PATH=/trusted") {
		t.Fatal("unsafe probe environment")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCTOR_TEST_HELPER", "output")
	out, status := runVersion(t.Context(), t.TempDir(), exe, "-test.run=^TestProbeHelperProcess$", "--")
	if status != "found" || parseVersion("go", out) != "1.27.3" {
		t.Fatal("real process readback failed")
	}
	t.Setenv("DOCTOR_TEST_HELPER", "wait")
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	out, status = runVersion(ctx, t.TempDir(), exe, "-test.run=^TestProbeHelperProcess$", "--")
	if status != "failed" || out != "" || time.Since(start) > 3*time.Second {
		t.Fatal("unbounded probe")
	}
	t.Setenv("DOCTOR_TEST_HELPER", "overflow")
	out, status = runVersion(t.Context(), t.TempDir(), exe, "-test.run=^TestProbeHelperProcess$", "--")
	if status != "failed" || out != "" {
		t.Fatal("oversized raw output escaped")
	}
}
func TestProbeHelperProcess(t *testing.T) {
	switch os.Getenv("DOCTOR_TEST_HELPER") {
	case "output":
		fmt.Println("go version go1.27.3 linux/amd64")
		os.Exit(0)
	case "wait":
		time.Sleep(time.Minute)
		os.Exit(0)
	case "overflow":
		fmt.Print(strings.Repeat("private diagnostic", 10000))
		os.Exit(0)
	}
}
func TestNativeToolchainReport(t *testing.T) {
	if os.Getenv("GOTH_DOCTOR_NATIVE") != "1" {
		t.Skip("explicit local toolchain acceptance")
	}
	r, err := Collect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if r.OS != runtime.GOOS || !r.Valid() {
		t.Fatal("invalid native report")
	}
	raw, _ := json.MarshalIndent(r, "", "  ")
	t.Log(string(raw))
}
