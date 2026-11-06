// Package plan validates every downloaded lesson plan before it can influence
// local files or processes. A digest binds content; it is not execution evidence.
package plan

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"regexp"
	"unicode/utf8"
)

const MaxLessonBytes = 2 << 20

var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var slugPattern = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9_.-]{0,127}$`)

type LocalMission struct {
	BlockID string `json:"block_id"`
	Mission
	Plan Plan `json:"plan"`
}
type Lesson struct {
	LessonID      string         `json:"lesson_id"`
	LessonVersion int64          `json:"lesson_version"`
	CourseSlug    string         `json:"course_slug"`
	LessonSlug    string         `json:"lesson_slug"`
	Missions      []LocalMission `json:"missions"`
}

// Digest validates and binds the entire reviewed projection, including starter
// files, instructions and lesson version, for an explicit start request.
func (p Lesson) Digest() (string, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return "", ErrInvalid
	}
	if _, err = DecodeLesson(raw, p.CourseSlug, p.LessonSlug); err != nil {
		return "", ErrInvalid
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func ValidSlug(s string) bool { return slugPattern.MatchString(s) }

// DecodeLesson rejects ambiguous JSON, wrong request identity, altered source or
// check definitions and cross-lesson references before returning usable data.
func DecodeLesson(raw []byte, course, slug string) (Lesson, error) {
	var out Lesson
	if len(raw) == 0 || len(raw) > MaxLessonBytes || !utf8.Valid(raw) || !ValidSlug(course) || !ValidSlug(slug) {
		return out, ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if scan(d, 0, lessonField) != nil {
		return out, ErrInvalid
	}
	if _, err := d.Token(); err != io.EOF {
		return out, ErrInvalid
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&out) != nil || out.CourseSlug != course || out.LessonSlug != slug || !idPattern.MatchString(out.LessonID) || out.LessonVersion < 1 || len(out.Missions) < 1 || len(out.Missions) > 16 {
		return Lesson{}, ErrInvalid
	}
	seen := map[string]bool{}
	for _, m := range out.Missions {
		if !idPattern.MatchString(m.BlockID) || seen[m.BlockID] || m.Validate() != nil {
			return Lesson{}, ErrInvalid
		}
		seen[m.BlockID] = true
		_, digest, err := m.Plan.Canonical()
		if err != nil || digest != m.CheckSHA256 || definitionID(out.LessonID, m.CheckVersion, digest) != m.CheckID {
			return Lesson{}, ErrInvalid
		}
	}
	canonical, err := json.Marshal(out)
	if err != nil || len(canonical) > MaxLessonBytes {
		return Lesson{}, ErrInvalid
	}
	return out, nil
}

func lessonField(s string) bool {
	switch s {
	case "lesson_id", "lesson_version", "course_slug", "lesson_slug", "missions", "block_id", "plan":
		return true
	}
	return field(s)
}

func definitionID(lesson string, version int64, digest string) string {
	raw, _ := json.Marshal(struct {
		Lesson  string `json:"lesson"`
		Version int64  `json:"version"`
		SHA256  string `json:"sha256"`
	}{lesson, version, digest})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
