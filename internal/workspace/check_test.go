package workspace

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"gothstack.com/cli/internal/plan"
)

func checkFixture(t *testing.T, checks []plan.Check, files ...plan.File) plan.Lesson {
	t.Helper()
	p := lessonFixture(t, 1, files...)
	m := &p.Missions[0]
	m.Plan.Checks = checks
	_, digest, e := m.Plan.Canonical()
	if e != nil {
		t.Fatal(e)
	}
	m.CheckSHA256 = digest
	raw, _ := json.Marshal(struct {
		Lesson  string `json:"lesson"`
		Version int64  `json:"version"`
		SHA256  string `json:"sha256"`
	}{p.LessonID, 1, digest})
	m.CheckID = hash(raw)
	if _, e = p.Digest(); e != nil {
		t.Fatal(e)
	}
	return p
}
func runChecksTest(t *testing.T, p plan.Lesson, target string) (*Workspace, CheckReport) {
	t.Helper()
	w := openTest(t, t.TempDir())
	applyTest(t, w, p)
	r, e := w.RunChecks(t.Context(), "https://gothstack.com", p, target)
	if e != nil {
		t.Fatal(e)
	}
	return w, r
}
func TestCheckRequiresMatchingStart(t *testing.T) {
	w := openTest(t, t.TempDir())
	p := lessonFixture(t, 1, plan.File{Path: "main.go", Content: "starter"})
	if _, e := w.RunChecks(t.Context(), "https://gothstack.com", p, ""); e != ErrNotStarted {
		t.Fatal(e)
	}
	applyTest(t, w, p)
	if _, e := w.RunChecks(t.Context(), "https://other.example", p, ""); e != ErrNotStarted {
		t.Fatal(e)
	}
	p = lessonFixture(t, 2, plan.File{Path: "main.go", Content: "starter"})
	if _, e := w.RunChecks(t.Context(), "https://gothstack.com", p, ""); e != ErrNotStarted {
		t.Fatal(e)
	}
}
func TestCheckFileAssertionsAndLocalDiagnostics(t *testing.T) {
	checks := []plan.Check{
		{ID: "present", Kind: "file_assert", TimeoutSeconds: 1, File: &plan.FileAssert{Path: "main.go", Condition: "exists"}},
		{ID: "missing", Kind: "file_assert", TimeoutSeconds: 1, File: &plan.FileAssert{Path: "absent.go", Condition: "absent"}},
		{ID: "regex", Kind: "file_assert", TimeoutSeconds: 1, File: &plan.FileAssert{Path: "main.go", Condition: "matches", Pattern: `(?m)^package main$`}},
		{ID: "wrong", Kind: "file_assert", TimeoutSeconds: 1, File: &plan.FileAssert{Path: "main.go", Condition: "absent"}},
	}
	_, r := runChecksTest(t, checkFixture(t, checks, plan.File{Path: "main.go", Content: "package main\n"}), "")
	for i, c := range r.Results {
		want := "pass"
		if i == 3 {
			want = "fail"
		}
		if c.Status != want {
			t.Fatal(i, c)
		}
	}
	if r.StartedUnix <= 0 || r.FinishedUnix < r.StartedUnix || r.GitCommit != "" || !r.GitDirty {
		t.Fatal("metadata", r)
	}
	r.Results[0].Output = "PRIVATE LOCAL DIAGNOSTIC"
	raw, e := json.Marshal(r)
	if e != nil || strings.Contains(string(raw), "PRIVATE") {
		t.Fatal("diagnostic upload", e)
	}
	if got := terminalText("normal\x1b[31m\x07\r\u202ehello\n", 100); got != "normal[31mhello\n" {
		t.Fatalf("terminal controls: %q", got)
	}
}
func TestCheckLoopbackTransport(t *testing.T) {
	var escaped atomic.Int32
	outside := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { escaped.Add(1) }))
	defer outside.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("ambient authorization")
		}
		switch r.URL.Path {
		case "/partial":
			if r.Method != "POST" || r.Header.Get("HX-Request") != "true" || r.Header.Get("HX-Request-Type") != "partial" {
				t.Error("typed request")
			}
			w.Header().Set("Set-Cookie", "ignored=value")
			w.WriteHeader(201)
			_, _ = w.Write([]byte("<p>ok</p>"))
		case "/redirect":
			http.Redirect(w, r, outside.URL, http.StatusFound)
		case "/headers":
			w.Header().Set("X-Large", strings.Repeat("x", 20<<10))
		case "/body":
			_, _ = w.Write([]byte("different"))
		case "/large":
			_, _ = w.Write([]byte(strings.Repeat("x", maxCheckOutput+1)))
		case "/encoded":
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = w.Write([]byte("bad"))
		case "/slow":
			<-r.Context().Done()
		default:
			w.WriteHeader(204)
		}
	}))
	defer server.Close()
	t.Setenv("HTTP_PROXY", outside.URL)
	t.Setenv("HTTPS_PROXY", outside.URL)
	expected := "<p>ok</p>"
	checks := []plan.Check{
		{ID: "partial", Kind: "http_probe", TimeoutSeconds: 2, HTTP: &plan.HTTP{Method: "POST", Path: "/partial", Status: 201, Body: "x=1", ContentType: "application/x-www-form-urlencoded", HXRequest: true, HXRequestType: "partial", ExpectedBody: &expected}},
		{ID: "redirect", Kind: "http_probe", TimeoutSeconds: 2, HTTP: &plan.HTTP{Method: "GET", Path: "/redirect", Status: 302}},
		{ID: "cookie", Kind: "http_probe", TimeoutSeconds: 2, HTTP: &plan.HTTP{Method: "GET", Path: "/empty", Status: 204}},
		{ID: "large", Kind: "http_probe", TimeoutSeconds: 2, HTTP: &plan.HTTP{Method: "GET", Path: "/large", Status: 200}},
		{ID: "encoded", Kind: "http_probe", TimeoutSeconds: 2, HTTP: &plan.HTTP{Method: "GET", Path: "/encoded", Status: 200}},
		{ID: "headers", Kind: "http_probe", TimeoutSeconds: 2, HTTP: &plan.HTTP{Method: "GET", Path: "/headers", Status: 200}},
		{ID: "status", Kind: "http_probe", TimeoutSeconds: 2, HTTP: &plan.HTTP{Method: "GET", Path: "/empty", Status: 200}},
		{ID: "body", Kind: "http_probe", TimeoutSeconds: 2, HTTP: &plan.HTTP{Method: "GET", Path: "/body", Status: 200, ExpectedBody: &expected}},
		{ID: "slow", Kind: "http_probe", TimeoutSeconds: 1, HTTP: &plan.HTTP{Method: "GET", Path: "/slow", Status: 200}},
	}
	_, r := runChecksTest(t, checkFixture(t, checks), server.URL)
	for i, want := range []string{"matched", "matched", "matched", "output_limit", "encoded_response", "request_failed", "status_mismatch", "body_mismatch", "timeout"} {
		if r.Results[i].Reason != want {
			t.Fatal(i, r.Results[i])
		}
	}
	if escaped.Load() != 0 {
		t.Fatal("redirect or proxy followed")
	}
	for _, target := range []string{"", "http://localhost:8123", "http://192.0.2.1:8123", "http://127.0.0.1", "http://127.0.0.1:0", "http://127.0.0.1:08080", "http://user@127.0.0.1:8123", "http://127.0.0.1:8123/path", "http://127.0.0.1:8123?query", "http://127.0.0.1:8123#fragment", "http://[::1%25lo]:8123"} {
		if _, e := loopbackTarget(target); e == nil {
			t.Fatal("unsafe target", target)
		}
	}
	if _, e := loopbackTarget("http://[::1]:8123"); e != nil {
		t.Fatal(e)
	}
}
func TestCheckUnsafeFilesAndCancellation(t *testing.T) {
	p := lessonFixture(t, 1, plan.File{Path: "main.go", Content: "starter"})
	w, r := runChecksTest(t, p, "")
	if r.Results[0].Status != "pass" {
		t.Fatal(r)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r, e := w.RunChecks(ctx, "https://gothstack.com", p, "")
	if e != context.Canceled || r.Results[0].Status != "skipped" {
		t.Fatal(r, e)
	}
	if runtime.GOOS != "windows" {
		if e = os.Remove(filepath.Join(w.git.dir, "main.go")); e != nil {
			t.Fatal(e)
		}
		if e = os.Symlink(filepath.Join(t.TempDir(), "outside"), filepath.Join(w.git.dir, "main.go")); e != nil {
			t.Fatal(e)
		}
		r, e = w.RunChecks(t.Context(), "https://gothstack.com", p, "")
		if e != nil || r.Results[0].Reason != "unsafe_file" {
			t.Fatal(r, e)
		}
	}
}
func TestCheckRealGoAndBoundaries(t *testing.T) {
	if !nativeChecksSupported() {
		t.Skip("native process execution is not accepted on this platform")
	}
	for _, test := range []struct {
		name, body, reason string
		seconds            int
	}{
		{"pass", `if os.Getenv("GOTH_CHECK_PRIVATE_SENTINEL")!="" {t.Fatal("secret inherited")};fmt.Println("safe output")`, "command_passed", 30},
		{"fail", `t.Fatal("deliberate failure")`, "command_failed", 30},
		{"limit", `for {fmt.Print(strings.Repeat("x",8192))}`, "output_limit", 30},
		{"timeout", `time.Sleep(30*time.Second)`, "timeout", 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("GOTH_CHECK_PRIVATE_SENTINEL", "DO_NOT_INHERIT")
			source := "package lesson\nimport (\"testing\";\"os\";\"fmt\";\"strings\";\"time\")\nvar _,_,_,_=os.Getenv,fmt.Println,strings.Repeat,time.Sleep\nfunc TestLesson(t *testing.T){" + test.body + "}\n"
			p := checkFixture(t, []plan.Check{{ID: "tests", Kind: "go_test", TimeoutSeconds: test.seconds, GoTest: &plan.GoTest{Packages: []string{"."}}}}, plan.File{Path: "go.mod", Content: "module lesson\n\ngo 1.27.3\n"}, plan.File{Path: "lesson_test.go", Content: source})
			w, r := runChecksTest(t, p, "")
			if err := w.checkScratchClean(); err != nil {
				t.Fatal("scratch survived process cleanup", err)
			}
			if len(r.Tools) != 2 || r.Tools[0].ID != "git" || r.Tools[1].ID != "go" || r.Tools[1].Status != "found" {
				t.Fatal("tool observations", r.Tools)
			}
			if r.Results[0].Reason != test.reason {
				t.Fatalf("%s: %s %s", test.name, r.Results[0].Reason, r.Results[0].Output)
			}
			if len(r.Results[0].Output) > maxCheckOutput {
				t.Fatal("output unbounded")
			}
		})
	}
}
func TestCheckRealTemplDoesNotRewrite(t *testing.T) {
	if !nativeChecksSupported() {
		t.Skip("native execution not accepted")
	}
	module := os.Getenv("GOTH_CLI_TEST_TEMPL_MODULE")
	if module == "" {
		t.Skip("provide an owned module with the pinned templ tool for native generation proof")
	}
	mod, e := os.ReadFile(filepath.Join(module, "go.mod"))
	if e != nil {
		t.Fatal(e)
	}
	sum, e := os.ReadFile(filepath.Join(module, "go.sum"))
	if e != nil {
		t.Fatal(e)
	}
	source := "package lesson\n\ntempl Greeting() {\n <p>Hello</p>\n}\n"
	p := checkFixture(t, []plan.Check{{ID: "templates", Kind: "templ_generate", TimeoutSeconds: 60, TemplGenerate: &plan.TemplGenerate{Path: "."}}}, plan.File{Path: "go.mod", Content: string(mod)}, plan.File{Path: "go.sum", Content: string(sum)}, plan.File{Path: "page.templ", Content: source})
	w, r := runChecksTest(t, p, "")
	if len(r.Tools) != 3 || r.Tools[2].ID != "templ" || r.Tools[2].Status != "found" {
		t.Fatal("pinned generator observation", r.Tools)
	}
	if r.Results[0].Reason != "command_failed" {
		t.Fatalf("missing generation: %s %s", r.Results[0].Reason, r.Results[0].Output)
	}
	generated := filepath.Join(w.git.dir, "page_templ.go")
	if _, e = os.Stat(generated); !os.IsNotExist(e) {
		t.Fatal("check wrote missing generated file", e)
	}
	// Fixture setup explicitly generates once; the check itself stays read-only.
	status, reason, output := w.checkProcess(t.Context(), "tool", "templ", "generate", "-path", ".")
	if status != "pass" {
		t.Fatal(reason, output)
	}
	before, e := os.ReadFile(generated)
	if e != nil {
		t.Fatal(e)
	}
	r, e = w.RunChecks(t.Context(), "https://gothstack.com", p, "")
	if e != nil || r.Results[0].Status != "pass" {
		t.Fatal(r, e)
	}
	if e = os.WriteFile(filepath.Join(w.git.dir, "page.templ"), []byte(strings.ReplaceAll(source, "Hello", "Changed")), 0644); e != nil {
		t.Fatal(e)
	}
	r, e = w.RunChecks(t.Context(), "https://gothstack.com", p, "")
	if e != nil || r.Results[0].Status != "fail" {
		t.Fatal(r, e)
	}
	after, e := os.ReadFile(generated)
	if e != nil || string(after) != string(before) {
		t.Fatal("check rewrote learner generated code", e)
	}
}

func TestCheckAggregateOutputBound(t *testing.T) {
	if !nativeChecksSupported() {
		t.Skip("native execution not accepted")
	}
	checks := []plan.Check{}
	for i := range 5 {
		checks = append(checks, plan.Check{ID: "flood" + strconv.Itoa(i), Kind: "go_test", TimeoutSeconds: 30, GoTest: &plan.GoTest{Packages: []string{"."}}})
	}
	p := checkFixture(t, checks, plan.File{Path: "go.mod", Content: "module lesson\n\ngo 1.27.3\n"}, plan.File{Path: "lesson_test.go", Content: `package lesson
import("testing";"fmt";"strings")
func TestFlood(t *testing.T){for{fmt.Print(strings.Repeat("x",8192))}}
`})
	_, r := runChecksTest(t, p, "")
	total := 0
	for _, c := range r.Results {
		if c.Reason != "output_limit" || len(c.Output) > maxCheckOutput {
			t.Fatal(c.Reason, len(c.Output))
		}
		total += len(c.Output)
	}
	if total > maxRunOutput || len(r.Results[4].Output) != 0 {
		t.Fatal("aggregate output unbounded", total)
	}
}

func TestCheckPreservesInterruptedScratch(t *testing.T) {
	dir := t.TempDir()
	w := openTest(t, dir)
	p := lessonFixture(t, 1, plan.File{Path: "main.go", Content: "starter"})
	applyTest(t, w, p)
	scratch := filepath.Join(dir, meta, "check-owned-fixture")
	if err := os.Mkdir(scratch, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scratch, "retained"), []byte("prior process may still own this"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := w.RunChecks(t.Context(), "https://gothstack.com", p, ""); err != ErrPendingCheck {
		t.Fatal(err)
	}
	readTest(t, scratch, "retained", "prior process may still own this")
	w.Close()
	if next, err := Open(t.Context(), dir); err != ErrPendingCheck {
		if next != nil {
			next.Close()
		}
		t.Fatal("start could overlap pending native process", err)
	}
}

func TestCheckExistenceDoesNotReadLargeFiles(t *testing.T) {
	p := checkFixture(t, []plan.Check{
		{ID: "present", Kind: "file_assert", TimeoutSeconds: 1, File: &plan.FileAssert{Path: "asset.bin", Condition: "exists"}},
		{ID: "content", Kind: "file_assert", TimeoutSeconds: 1, File: &plan.FileAssert{Path: "asset.bin", Condition: "matches", Pattern: "x"}},
	})
	w := openTest(t, t.TempDir())
	applyTest(t, w, p)
	f, err := os.Create(filepath.Join(w.git.dir, "asset.bin"))
	if err != nil {
		t.Fatal(err)
	}
	err = f.Truncate(plan.MaxFileBytes + 1)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	r, err := w.RunChecks(t.Context(), "https://gothstack.com", p, "")
	if err != nil || r.Results[0].Status != "pass" || r.Results[1].Reason != "unsafe_file" {
		t.Fatal(r, err)
	}
}

func TestCheckOpenDoesNotInitializeUnrelatedDirectory(t *testing.T) {
	dir := t.TempDir()
	if w, e := OpenStarted(t.Context(), dir); e == nil {
		w.Close()
		t.Fatal("unstarted directory opened")
	}
	if _, e := os.Stat(filepath.Join(dir, ".git")); !os.IsNotExist(e) {
		t.Fatal("check initialized a repository", e)
	}
}
