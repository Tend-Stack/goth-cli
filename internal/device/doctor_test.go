package device

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gothstack.com/cli/internal/doctor"
)

func doctorSample() doctor.Report {
	r := doctor.Report{Schema: 1, OS: "linux", Arch: "amd64", CollectedUnix: time.Now().Unix()}
	for _, id := range []string{"go", "git", "templ", "wails3", "java", "android_sdk", "android_ndk"} {
		r.Tools = append(r.Tools, doctor.Tool{ID: id, Status: "missing"})
	}
	r.Tools = append(r.Tools, doctor.Tool{ID: "xcode", Status: "unsupported"})
	return r
}
func TestDoctorUploadFixedOriginReceiptAndSafeErrors(t *testing.T) {
	const bearer = "gs_cli_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	var calls atomic.Int32
	var endpoint atomic.Value
	endpoint.Store(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/cli/doctor" || r.Method != "POST" || r.Header.Get("Authorization") != "Bearer "+bearer || r.Header.Get("Cookie") != "" || r.Header.Get("Accept") != MediaType {
			t.Error("request boundary")
		}
		var report doctor.Report
		if json.NewDecoder(r.Body).Decode(&report) != nil || !report.Valid() {
			t.Error("report")
		}
		w.Header().Set("Content-Type", MediaType)
		fmt.Fprintf(w, `{"received_unix":%d}`, time.Now().Unix())
	}))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); endpoint.Load().(http.HandlerFunc)(w, r) }))
	defer server.Close()
	c, err := New(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err = c.UploadDoctor(t.Context(), bearer, doctorSample()); err != nil {
		t.Fatal(err)
	}
	if _, err = c.UploadDoctor(t.Context(), "injected\r\nsecret", doctorSample()); err == nil || calls.Load() != 1 {
		t.Fatal("invalid bearer reached network")
	}
	for _, tc := range []struct {
		status int
		body   string
		retry  string
	}{
		{200, `{"received_unix":1}`, ""},
		{200, fmt.Sprintf(`{"received_unix":%d,"received_unix":%d}`, time.Now().Unix(), time.Now().Unix()), ""},
		{200, `{"received_unix":null}`, ""},
		{401, `{"error":"invalid_token"}`, ""},
		{429, `{"error":"rate_limited"}`, "30"},
		{503, `{"error":"temporarily_unavailable"}`, "5"},
		{400, `{"error":"invalid_request"}`, ""},
		{503, `{"error":"secret-server-diagnostic"}`, "5"},
		{200, strings.Repeat("x", 8193), ""},
	} {
		endpoint.Store(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", MediaType)
			w.Header().Set("Retry-After", tc.retry)
			w.WriteHeader(tc.status)
			fmt.Fprint(w, tc.body)
		}))
		before := calls.Load()
		_, err = c.UploadDoctor(t.Context(), bearer, doctorSample())
		if err == nil || strings.Contains(err.Error(), "secret-server-diagnostic") || calls.Load() != before+1 {
			t.Fatal("failure leaked or mutation retried", err)
		}
	}
	var redirected atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
	defer other.Close()
	endpoint.Store(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	if _, err = c.UploadDoctor(t.Context(), bearer, doctorSample()); err == nil || redirected.Load() != 0 {
		t.Fatal("followed bearer redirect")
	}
}
