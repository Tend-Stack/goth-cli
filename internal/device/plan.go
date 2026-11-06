package device

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"gothstack.com/cli/internal/plan"
)

// FetchPlan reads only a canonical lesson reference on the configured origin.
// It neither retries requests nor enrolls, writes files or runs project code.
func (c *Client) FetchPlan(ctx context.Context, bearer, course, slug string) (plan.Lesson, error) {
	raw, ok := strings.CutPrefix(bearer, "gs_cli_")
	if !ok || !ValidSecret(raw) || !plan.ValidSlug(course) || !plan.ValidSlug(slug) {
		return plan.Lesson{}, ErrProtocol
	}
	status, body, _, err := c.requestJSON(ctx, http.MethodGet, "/api/cli/lessons/"+course+"/"+slug, nil, bearer, plan.MaxLessonBytes)
	if err != nil {
		return plan.Lesson{}, err
	}
	return planResult(status, body, course, slug)
}

func (c *Client) FetchCode(ctx context.Context, bearer, code string) (plan.Lesson, error) {
	raw, ok := strings.CutPrefix(bearer, "gs_cli_")
	if !ok || !ValidSecret(raw) || !plan.ValidLessonCode(code) {
		return plan.Lesson{}, ErrProtocol
	}
	status, body, _, err := c.requestJSON(ctx, http.MethodGet, "/api/cli/lessons/"+code, nil, bearer, plan.MaxLessonBytes)
	if err != nil {
		return plan.Lesson{}, err
	}
	return planResult(status, body, "", code)
}

// StartPlan pins the exact reviewed response. It does not retry a mutation or
// modify local files; callers must handle an unconfirmed request explicitly.
func (c *Client) StartPlan(ctx context.Context, bearer string, reviewed plan.Lesson) (plan.Lesson, error) {
	raw, ok := strings.CutPrefix(bearer, "gs_cli_")
	digest, err := reviewed.Digest()
	if !ok || !ValidSecret(raw) || err != nil {
		return plan.Lesson{}, ErrProtocol
	}
	body, _ := json.Marshal(struct {
		Version int64  `json:"lesson_version"`
		SHA256  string `json:"plan_sha256"`
	}{reviewed.LessonVersion, digest})
	status, response, _, err := c.requestJSON(ctx, http.MethodPost, "/api/cli/lessons/"+reviewed.CourseSlug+"/"+reviewed.LessonSlug+"/start", body, bearer, plan.MaxLessonBytes)
	if err != nil {
		return plan.Lesson{}, err
	}
	p, err := planResult(status, response, reviewed.CourseSlug, reviewed.LessonSlug)
	if err != nil {
		return plan.Lesson{}, err
	}
	confirmed, err := p.Digest()
	if err != nil || confirmed != digest {
		return plan.Lesson{}, ErrProtocol
	}
	return p, nil
}

func planResult(status int, body []byte, course, slug string) (plan.Lesson, error) {
	if status == 200 {
		var lesson plan.Lesson
		var err error
		if course == "" {
			lesson, err = plan.DecodeCodeLesson(body, slug)
		} else {
			lesson, err = plan.DecodeLesson(body, course, slug)
		}
		if err != nil {
			return plan.Lesson{}, ErrProtocol
		}
		return lesson, nil
	}
	var failure struct {
		Error string `json:"error"`
	}
	if Decode(body, &failure) != nil {
		return plan.Lesson{}, ErrProtocol
	}
	switch {
	case status == 401 && failure.Error == "invalid_token":
		return plan.Lesson{}, errors.New("terminal access is expired or revoked; run goth login --replace")
	case status == 404 && failure.Error == "not_found":
		return plan.Lesson{}, errors.New("the lesson plan is unavailable for this account; check the lesson reference and prerequisites")
	case status == 409 && failure.Error == "plan_changed":
		return plan.Lesson{}, errors.New("the lesson plan changed; fetch and review it again before starting")
	case status == 429 && failure.Error == "rate_limited":
		return plan.Lesson{}, errors.New("lesson requests are rate limited; wait before trying again")
	case status == 503 && failure.Error == "temporarily_unavailable":
		return plan.Lesson{}, ErrUnavailable
	default:
		return plan.Lesson{}, ErrProtocol
	}
}
