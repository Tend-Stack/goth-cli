package device

import (
	"context"
	"errors"
	"fmt"
	"gothstack.com/cli/internal/plan"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

const planResponse = `{"lesson_id":"lesson","lesson_version":1,"course_slug":"course","lesson_slug":"05.1","missions":[{"block_id":"mission","check_id":"1d87c6f9edfe7180e5c6c8bd8b6c9d05452b92c314133608c04a046ab6417eeb","check_version":1,"check_sha256":"5d67aa13812c4386ef3625a6c71f92a76998135e6d7973acfa2f111696eb0f20","instructions_md":"Build locally.","evidence":["Source file"],"plan":{"schema":1,"project":"pocketpilot","files":[{"path":"main.go","content":"package main\n"}],"checks":[{"id":"source","kind":"file_assert","timeout_seconds":1,"file_assert":{"path":"main.go","condition":"exists"}}]}}]}`

func TestFetchPlanOriginValidationLimitsAndCancellation(t *testing.T) {
	const bearer = "gs_cli_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	var calls atomic.Int32
	var handler atomic.Value
	handler.Store(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method != "GET" || r.URL.Path != "/api/cli/lessons/course/05.1" || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer "+bearer || r.Header.Get("Accept") != MediaType || r.Header.Get("Accept-Encoding") != "identity" || r.Header.Get("Content-Type") != "" || len(body) != 0 || r.Header.Get("Cookie") != "" {
			t.Error("request boundary")
		}
		w.Header().Set("Content-Type", MediaType)
		fmt.Fprint(w, planResponse)
	}))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); handler.Load().(http.HandlerFunc)(w, r) }))
	defer server.Close()
	c, err := New(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	p, err := c.FetchPlan(t.Context(), bearer, "course", "05.1")
	if err != nil || len(p.Missions) != 1 {
		t.Fatal(p, err)
	}
	for _, ref := range []string{"../other", "bad?x=y", "bad%2fpath", ""} {
		if _, err := c.FetchPlan(t.Context(), bearer, "course", ref); err == nil {
			t.Fatal("unsafe ref")
		}
	}
	if _, err := c.FetchPlan(t.Context(), "secret\r\nX-Injected: bad", "course", "05.1"); err == nil || calls.Load() != 1 {
		t.Fatal("invalid request reached network")
	}
	for _, tc := range []struct {
		status                int
		body, retry, encoding string
	}{
		{401, `{"error":"invalid_token"}`, "", ""},
		{404, `{"error":"not_found"}`, "", ""},
		{429, `{"error":"rate_limited"}`, "30", ""},
		{503, `{"error":"temporarily_unavailable"}`, "5", ""},
		{503, `{"error":"PRIVATE DIAGNOSTIC"}`, "5", ""},
		{200, strings.Replace(planResponse, "package main", "package replaced", 1), "", ""},
		{200, strings.Repeat("x", plan.MaxLessonBytes+1), "", ""},
		{200, planResponse, "", "gzip"},
	} {
		handler.Store(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", MediaType)
			if tc.retry != "" {
				w.Header().Set("Retry-After", tc.retry)
			}
			if tc.encoding != "" {
				w.Header().Set("Content-Encoding", tc.encoding)
			}
			w.WriteHeader(tc.status)
			fmt.Fprint(w, tc.body)
		}))
		before := calls.Load()
		_, err := c.FetchPlan(t.Context(), bearer, "course", "05.1")
		if err == nil || strings.Contains(err.Error(), "PRIVATE DIAGNOSTIC") || calls.Load() != before+1 {
			t.Fatal("unsafe error or automatic retry", err)
		}
	}
	var redirected atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
	defer other.Close()
	handler.Store(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	if _, err := c.FetchPlan(t.Context(), bearer, "course", "05.1"); err == nil || redirected.Load() != 0 {
		t.Fatal("followed redirect")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := c.FetchPlan(ctx, bearer, "course", "05.1"); !errors.Is(err, context.Canceled) {
		t.Fatal("lost cancellation", err)
	}
}

// The application integration test provides an owned loopback server and an
// ephemeral test credential. Ordinary public-module tests need no app checkout.
func TestPlanActualServer(t *testing.T) {
	origin := os.Getenv("GOTHSTACK_CLI_TEST_SERVER")
	if origin == "" {
		t.Skip("application integration fixture not provided")
	}
	u, err := url.Parse(origin)
	if err != nil {
		t.Fatal("fixture origin")
	}
	ip, err := netip.ParseAddr(u.Hostname())
	if err != nil || !ip.IsLoopback() {
		t.Fatal("fixture must be literal loopback")
	}
	c, err := New(origin)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	token := os.Getenv("GOTHSTACK_CLI_TEST_TOKEN")
	p, err := c.FetchPlan(t.Context(), token, "cli-course", "05.1")
	if err != nil {
		t.Fatal(err)
	}
	if p.LessonID != "cli-lesson" || len(p.Missions) != 1 {
		t.Fatal("unexpected fixture")
	}
	before, err := p.Digest()
	if err != nil {
		t.Fatal(err)
	}
	started, err := c.StartPlan(t.Context(), token, p)
	if err != nil {
		t.Fatal(err)
	}
	after, err := started.Digest()
	if err != nil || after != before {
		t.Fatal("start changed reviewed plan")
	}
	if _, err = c.StartPlan(t.Context(), token, p); err != nil {
		t.Fatal("idempotent start", err)
	}
}

func TestStartPlanNoRetryAndExactConfirmation(t *testing.T) {
	const token = "gs_cli_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	p, err := plan.DecodeLesson([]byte(planResponse), "course", "05.1")
	if err != nil {
		t.Fatal(err)
	}
	digest, _ := p.Digest()
	var calls atomic.Int32
	var status atomic.Int32
	status.Store(200)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if r.Method != "POST" || r.URL.Path != "/api/cli/lessons/course/05.1/start" || r.Header.Get("Authorization") != "Bearer "+token || !strings.Contains(string(body), digest) {
			t.Error("start boundary")
		}
		w.Header().Set("Content-Type", MediaType)
		code := int(status.Load())
		w.WriteHeader(code)
		if code == 200 {
			fmt.Fprint(w, planResponse)
		} else if code == 409 {
			fmt.Fprint(w, `{"error":"plan_changed"}`)
		} else {
			fmt.Fprint(w, `{"error":"invalid_token"}`)
		}
	}))
	defer server.Close()
	c, _ := New(server.URL)
	defer c.Close()
	if _, err = c.StartPlan(t.Context(), token, p); err != nil {
		t.Fatal(err)
	}
	for _, code := range []int32{409, 401} {
		status.Store(code)
		before := calls.Load()
		if _, err = c.StartPlan(t.Context(), token, p); err == nil || calls.Load() != before+1 {
			t.Fatal("mutation retried", err)
		}
	}
	p.Missions[0].Plan.Project = "tampered"
	before := calls.Load()
	if _, err = c.StartPlan(t.Context(), token, p); err == nil || calls.Load() != before {
		t.Fatal("invalid reviewed plan reached server")
	}
}
