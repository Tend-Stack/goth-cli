// Package doctor collects a bounded, path-free toolchain preflight report.
// A reported version is self-reported diagnostic information, not attestation.
package doctor

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Tool struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Version string `json:"version,omitempty"`
}
type Report struct {
	Schema        int    `json:"schema"`
	OS            string `json:"os"`
	Arch          string `json:"arch"`
	CollectedUnix int64  `json:"collected_unix"`
	Tools         []Tool `json:"tools"`
}

var toolIDs = []string{"go", "git", "templ", "wails3", "java", "android_sdk", "android_ndk", "xcode"}
var numericVersionPattern = regexp.MustCompile(`^[0-9]{1,6}(?:\.[0-9]{1,9}){0,3}$`)
var versionPattern = regexp.MustCompile(`^[0-9][0-9A-Za-z.+_-]{0,63}$`)
var captures = map[string]*regexp.Regexp{
	"go":     regexp.MustCompile(`(?m)^go version go([0-9][0-9A-Za-z.+_-]{0,63}) `),
	"git":    regexp.MustCompile(`(?m)^git version ([0-9][0-9A-Za-z.+_-]{0,63})(?:[\r\n ]|$)`),
	"templ":  regexp.MustCompile(`(?m)^v?([0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z._-]+)?)[\r\n ]*$`),
	"wails3": regexp.MustCompile(`(?m)^v?([0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z._-]+)?)[\r\n ]*$`),
	"java":   regexp.MustCompile(`(?m)^(?:openjdk|java) version "([0-9][0-9A-Za-z.+_-]{0,63})"`),
	"xcode":  regexp.MustCompile(`(?m)^Xcode ([0-9]+(?:\.[0-9]+){1,2})[\r\n ]*$`),
}

// Valid validates the report shape. The server must independently validate it,
// bind it to a currently authorized credential, and use its own receipt time.
func (r Report) Valid() bool {
	if r.Schema != 1 || r.CollectedUnix <= 0 || !contains([]string{"linux", "darwin", "windows"}, r.OS) || !contains([]string{"amd64", "arm64"}, r.Arch) || len(r.Tools) != len(toolIDs) {
		return false
	}
	for i, t := range r.Tools {
		if t.ID != toolIDs[i] || !contains([]string{"found", "missing", "failed", "unsupported"}, t.Status) {
			return false
		}
		if t.Status == "found" {
			if !versionPattern.MatchString(t.Version) {
				return false
			}
		} else if t.Version != "" {
			return false
		}
		if t.Status == "unsupported" && (t.ID != "xcode" || r.OS == "darwin") {
			return false
		}
		if t.ID == "xcode" && r.OS != "darwin" && t.Status != "unsupported" {
			return false
		}
	}
	return true
}
func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
func parseVersion(id, raw string) string {
	p := captures[id]
	if p == nil {
		return ""
	}
	values := p.FindStringSubmatch(raw)
	if len(values) != 2 || !versionPattern.MatchString(values[1]) {
		return ""
	}
	return values[1]
}

type collector struct {
	os, arch, sdk string
	run           func(context.Context, string, ...string) (string, string)
	now           func() time.Time
}

// Collect only invokes fixed version commands. It never runs a project build,
// Wails setup/doctor, package installation, shell, or downloaded check definition.
func Collect(ctx context.Context) (Report, error) {
	c := collector{os: runtime.GOOS, arch: runtime.GOARCH, now: time.Now}
	if !contains([]string{"linux", "darwin", "windows"}, c.os) || !contains([]string{"amd64", "arm64"}, c.arch) {
		return Report{}, errors.New("this platform is not supported by goth doctor")
	}
	dir, err := os.MkdirTemp("", "goth-doctor-")
	if err != nil {
		return Report{}, errors.New("could not create a private probe directory")
	}
	defer os.RemoveAll(dir)
	c.run = func(ctx context.Context, name string, args ...string) (string, string) {
		return runVersion(ctx, dir, name, args...)
	}
	c.sdk = sdkDirectory()
	return c.collect(ctx)
}
func (c collector) collect(ctx context.Context) (Report, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	report := Report{Schema: 1, OS: c.os, Arch: c.arch, CollectedUnix: c.now().Unix()}
	for _, id := range toolIDs {
		if ctx.Err() != nil {
			return Report{}, ctx.Err()
		}
		tool := Tool{ID: id}
		switch id {
		case "android_sdk":
			tool.Status, tool.Version = readSDK(c.sdk, "platforms", "AndroidVersion.ApiLevel")
		case "android_ndk":
			tool.Status, tool.Version = readSDK(c.sdk, "ndk", "Pkg.Revision")
		case "xcode":
			if c.os != "darwin" {
				tool.Status = "unsupported"
			} else {
				tool = c.command(ctx, id, "/usr/bin/xcodebuild", "-version")
			}
		case "go":
			tool = c.command(ctx, id, "go", "version")
		case "git":
			tool = c.command(ctx, id, "git", "--version")
		case "templ":
			tool = c.command(ctx, id, "templ", "version")
		case "wails3":
			tool = c.command(ctx, id, "wails3", "version")
		case "java":
			tool = c.command(ctx, id, "java", "-version")
		}
		report.Tools = append(report.Tools, tool)
	}
	if ctx.Err() != nil {
		return Report{}, ctx.Err()
	}
	if !report.Valid() {
		return Report{}, errors.New("could not produce a valid toolchain report")
	}
	return report, nil
}
func (c collector) command(ctx context.Context, id, name string, args ...string) Tool {
	raw, status := c.run(ctx, name, args...)
	t := Tool{ID: id, Status: status}
	if status == "found" {
		t.Version = parseVersion(id, raw)
		if t.Version == "" {
			t.Status = "failed"
		}
	}
	return t
}

type boundedOutput struct{ data []byte }

func (b *boundedOutput) Write(p []byte) (int, error) {
	if len(b.data)+len(p) > 8192 {
		return 0, errors.New("version output exceeded limit")
	}
	b.data = append(b.data, p...)
	return len(p), nil
}
func runVersion(ctx context.Context, dir, name string, args ...string) (string, string) {
	path, err := exec.LookPath(name)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			return "", "missing"
		}
		return "", "failed"
	}
	if !filepath.IsAbs(path) {
		return "", "failed"
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = dir
	cmd.Env = probeEnvironment(os.Environ())
	cmd.WaitDelay = time.Second
	boundProcess(cmd)
	var out boundedOutput
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err = cmd.Run(); err != nil {
		return "", "failed"
	}
	return string(out.data), "found"
}
func probeEnvironment(env []string) []string {
	out := make([]string, 0, len(env)+8)
	for _, pair := range env {
		key, _, _ := strings.Cut(pair, "=")
		key = strings.ToUpper(key)
		if strings.HasPrefix(key, "GO") || strings.HasPrefix(key, "JAVA") || key == "JDK_JAVA_OPTIONS" || key == "_JAVA_OPTIONS" || key == "NO_COLOR" || key == "TERM" || key == "WAILS_MCP_CHILD" {
			continue
		}
		out = append(out, pair)
	}
	return append(out, "GOTOOLCHAIN=local", "GOWORK=off", "GOENV=off", "GOFLAGS=", "GOPROXY=off", "GOSUMDB=off", "GOTELEMETRY=off", "NO_COLOR=1", "TERM=dumb", "WAILS_MCP_CHILD=1")
}
func sdkDirectory() string {
	for _, key := range []string{"ANDROID_HOME", "ANDROID_SDK_ROOT"} {
		if p := os.Getenv(key); p != "" {
			if filepath.IsAbs(p) {
				return p
			}
			return ""
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Android", "sdk")
	case "windows":
		if p := os.Getenv("LOCALAPPDATA"); filepath.IsAbs(p) {
			return filepath.Join(p, "Android", "Sdk")
		}
		return ""
	default:
		return filepath.Join(home, "Android", "Sdk")
	}
}
func readSDK(path, subdir, key string) (string, string) {
	if path == "" {
		return "missing", ""
	}
	root, err := os.OpenRoot(path)
	if errors.Is(err, os.ErrNotExist) {
		return "missing", ""
	}
	if err != nil {
		return "failed", ""
	}
	defer root.Close()
	dir, err := root.Open(subdir)
	if errors.Is(err, os.ErrNotExist) {
		return "missing", ""
	}
	if err != nil {
		return "failed", ""
	}
	defer dir.Close()
	entries, err := dir.ReadDir(129)
	if err != nil && !errors.Is(err, io.EOF) || len(entries) > 128 {
		return "failed", ""
	}
	versions := []string{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := filepath.Join(subdir, entry.Name(), "source.properties")
		before, err := root.Lstat(name)
		if err != nil || !before.Mode().IsRegular() {
			continue
		}
		f, err := root.OpenFile(name, propertyFlags(), 0)
		if err != nil {
			continue
		}
		info, e := f.Stat()
		if e != nil || !info.Mode().IsRegular() || !os.SameFile(before, info) || info.Size() > 4096 {
			f.Close()
			continue
		}
		raw, e := io.ReadAll(io.LimitReader(f, 4097))
		f.Close()
		if e != nil || len(raw) > 4096 {
			continue
		}
		value := ""
		seen := false
		for _, line := range strings.Split(string(raw), "\n") {
			k, v, ok := strings.Cut(line, "=")
			if ok && strings.TrimSpace(k) == key {
				if seen {
					value = ""
					break
				}
				seen = true
				value = strings.TrimSpace(v)
			}
		}
		if numericVersionPattern.MatchString(value) {
			versions = append(versions, value)
		}
	}
	if len(versions) == 0 {
		if len(entries) > 0 {
			return "failed", ""
		}
		return "missing", ""
	}
	sort.Slice(versions, func(i, j int) bool { return greaterVersion(versions[i], versions[j]) })
	return "found", versions[0]
}
func greaterVersion(a, b string) bool {
	aa, bb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < max(len(aa), len(bb)); i++ {
		x, y := int64(0), int64(0)
		if i < len(aa) {
			x, _ = strconv.ParseInt(aa[i], 10, 64)
		}
		if i < len(bb) {
			y, _ = strconv.ParseInt(bb[i], 10, 64)
		}
		if x != y {
			return x > y
		}
	}
	return false
}
