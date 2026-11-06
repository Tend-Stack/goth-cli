package device

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"gothstack.com/cli/internal/plan"
)

// UploadEvidence makes one bounded mutation request. No automatic retries or
// redirects; an interrupted response is explicitly unconfirmed to the caller.
func (c *Client) UploadEvidence(ctx context.Context, bearer string, raw []byte) (plan.EvidenceReceipt, error) {
	secret, ok := strings.CutPrefix(bearer, "gs_cli_")
	_, e, err := plan.DecodeEvidence(raw)
	if !ok || !ValidSecret(secret) || err != nil {
		return plan.EvidenceReceipt{}, ErrProtocol
	}
	status, body, _, err := c.requestJSON(ctx, http.MethodPost, "/api/cli/evidence", raw, bearer, 8192)
	if err != nil {
		return plan.EvidenceReceipt{}, err
	}
	if status == 200 {
		var receipt plan.EvidenceReceipt
		if Decode(body, &receipt) != nil || receipt.SubmissionID != e.SubmissionID || receipt.ReceivedUnix < 1 || len(receipt.Missions) != len(e.Missions) {
			return plan.EvidenceReceipt{}, ErrProtocol
		}
		for i, m := range e.Missions {
			if receipt.Missions[i].BlockID != m.BlockID || receipt.Missions[i].Verdict != m.Verdict() {
				return plan.EvidenceReceipt{}, ErrProtocol
			}
		}
		return receipt, nil
	}
	var failure struct {
		Error string `json:"error"`
	}
	if Decode(body, &failure) != nil {
		return plan.EvidenceReceipt{}, ErrProtocol
	}
	switch {
	case status == 400 && failure.Error == "invalid_request":
		return plan.EvidenceReceipt{}, errors.New("the check report is invalid or too old; run goth check again")
	case status == 401 && failure.Error == "invalid_token":
		return plan.EvidenceReceipt{}, errors.New("terminal access or its signature was rejected; run goth login --replace")
	case status == 404 && failure.Error == "not_found":
		return plan.EvidenceReceipt{}, errors.New("the lesson is no longer available for this account")
	case status == 409 && failure.Error == "plan_changed":
		return plan.EvidenceReceipt{}, errors.New("the lesson plan changed or is not started; review it with goth start")
	case status == 409 && failure.Error == "submission_conflict":
		return plan.EvidenceReceipt{}, errors.New("the submission identity conflicts with an earlier report")
	case status == 429 && failure.Error == "rate_limited":
		return plan.EvidenceReceipt{}, errors.New("check submissions are rate limited; wait at least 30 seconds before trying again")
	case status == 503 && failure.Error == "temporarily_unavailable":
		return plan.EvidenceReceipt{}, ErrUnavailable
	default:
		return plan.EvidenceReceipt{}, ErrProtocol
	}
}
