package device

import (
	"context"
	"errors"
	"strings"
	"time"

	"gothstack.com/cli/internal/doctor"
)

// UploadDoctor uses only the explicitly configured origin and the fixed doctor
// endpoint. It never retries a mutation or follows redirects with a bearer.
func (c *Client) UploadDoctor(ctx context.Context, bearer string, report doctor.Report) (time.Time, error) {
	raw, ok := strings.CutPrefix(bearer, "gs_cli_")
	if !ok || !ValidSecret(raw) || !report.Valid() {
		return time.Time{}, ErrProtocol
	}
	status, body, _, err := c.requestBearer(ctx, "/api/cli/doctor", report, bearer)
	if err != nil {
		return time.Time{}, err
	}
	if status == 200 {
		var receipt struct {
			ReceivedUnix int64 `json:"received_unix"`
		}
		if Decode(body, &receipt) != nil || receipt.ReceivedUnix < c.now().Unix()-300 || receipt.ReceivedUnix > c.now().Unix()+300 {
			return time.Time{}, ErrProtocol
		}
		return time.Unix(receipt.ReceivedUnix, 0), nil
	}
	var failure struct {
		Error string `json:"error"`
	}
	if Decode(body, &failure) != nil {
		return time.Time{}, ErrProtocol
	}
	switch {
	case status == 401 && failure.Error == "invalid_token":
		return time.Time{}, errors.New("terminal access is expired or revoked; run goth login --replace")
	case status == 429 && failure.Error == "rate_limited":
		return time.Time{}, errors.New("toolchain upload is rate limited; wait before running goth doctor again")
	case status == 400 && failure.Error == "invalid_request":
		return time.Time{}, errors.New("the server rejected the toolchain report; check your system clock and CLI version")
	case status == 503 && failure.Error == "temporarily_unavailable":
		return time.Time{}, errors.New("toolchain upload is unavailable; try again later")
	default:
		return time.Time{}, ErrProtocol
	}
}
