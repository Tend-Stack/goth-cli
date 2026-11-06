package device

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gothstack.com/cli/internal/plan"
)

func TestUploadEvidenceExactReceiptAndNoRetries(t *testing.T) {
	e := plan.Evidence{Schema: 1, SubmissionID: strings.Repeat("A", 43), TokenID: "cli_test", CourseSlug: "course", LessonSlug: "lesson", LessonID: "lesson", LessonVersion: 1, PlanSHA256: strings.Repeat("a", 64), StartedUnix: 1, FinishedUnix: 1, GitDirty: true, Tools: []plan.EvidenceTool{{ID: "git", Status: "found", Version: "2.53.0"}}, Missions: []plan.EvidenceMission{{BlockID: "block", CheckID: strings.Repeat("b", 64), CheckVersion: 1, CheckSHA256: strings.Repeat("c", 64), Results: []plan.EvidenceResult{{ID: "file", Status: "pass", Reason: "matched"}}}}}
	raw, err := plan.SignEvidence(e, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	mode := "ok"
	const bearer = "gs_cli_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		b, _ := io.ReadAll(r.Body)
		if !bytes.Equal(b, raw) || r.Method != "POST" || r.URL.Path != "/api/cli/evidence" || r.Header.Get("Authorization") != "Bearer "+bearer || r.Header.Get("Cookie") != "" {
			t.Error("request boundary")
		}
		w.Header().Set("Content-Type", MediaType)
		if mode == "error" {
			w.WriteHeader(503)
			io.WriteString(w, `{"error":"temporarily_unavailable"}`)
			return
		}
		if mode == "redirect" {
			w.Header().Set("Location", serverDummyOrigin)
			w.WriteHeader(307)
			return
		}
		receipt := plan.EvidenceReceipt{SubmissionID: e.SubmissionID, ReceivedUnix: 2, Missions: []plan.MissionReceipt{{BlockID: "block", Verdict: "pass"}}}
		switch mode {
		case "wrong-id":
			receipt.SubmissionID = "wrong"
		case "wrong-block":
			receipt.Missions[0].BlockID = "wrong"
		case "wrong-verdict":
			receipt.Missions[0].Verdict = "fail"
		}
		json.NewEncoder(w).Encode(receipt)
	}))
	defer server.Close()
	client, err := New(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err = client.UploadEvidence(t.Context(), bearer, raw); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"error", "redirect", "wrong-id", "wrong-block", "wrong-verdict"} {
		mode = m
		before := calls
		if _, err = client.UploadEvidence(t.Context(), bearer, raw); err == nil || calls != before+1 {
			t.Fatal("invalid response accepted or retried", m, err, calls)
		}
	}
	before := calls
	if _, err = client.UploadEvidence(t.Context(), bearer, []byte(`{}`)); err == nil || calls != before {
		t.Fatal("invalid local evidence sent")
	}
}

const serverDummyOrigin = "http://127.0.0.1:1/elsewhere"
