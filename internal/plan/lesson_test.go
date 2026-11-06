package plan

import (
	"encoding/json"
	"strings"
	"testing"
)

// Fixed canonical hashes are calculated independently of the client validator.
const sampleLesson = `{"lesson_id":"lesson","lesson_version":1,"course_slug":"course","lesson_slug":"05.1","missions":[{"block_id":"mission","check_id":"1d87c6f9edfe7180e5c6c8bd8b6c9d05452b92c314133608c04a046ab6417eeb","check_version":1,"check_sha256":"5d67aa13812c4386ef3625a6c71f92a76998135e6d7973acfa2f111696eb0f20","instructions_md":"Build locally.","evidence":["Source file"],"plan":{"schema":1,"project":"pocketpilot","files":[{"path":"main.go","content":"package main\n"}],"checks":[{"id":"source","kind":"file_assert","timeout_seconds":1,"file_assert":{"path":"main.go","condition":"exists"}}]}}]}`

func TestLessonIdentityAndDigestBinding(t *testing.T) {
	p, err := DecodeLesson([]byte(sampleLesson), "course", "05.1")
	if err != nil || p.LessonID != "lesson" || p.Missions[0].Plan.Files[0].Content != "package main\n" {
		t.Fatal(p, err)
	}
	for name, body := range map[string]string{
		"course":            strings.Replace(sampleLesson, `"course_slug":"course"`, `"course_slug":"other"`, 1),
		"lesson":            strings.Replace(sampleLesson, `"lesson_slug":"05.1"`, `"lesson_slug":"05.2"`, 1),
		"lesson-id":         strings.Replace(sampleLesson, `"lesson_id":"lesson"`, `"lesson_id":"other"`, 1),
		"version":           strings.Replace(sampleLesson, `"check_version":1`, `"check_version":2`, 1),
		"starter":           strings.Replace(sampleLesson, `package main`, `package changed`, 1),
		"check":             strings.Replace(sampleLesson, `"condition":"exists"`, `"condition":"absent"`, 1),
		"duplicate":         strings.Replace(sampleLesson, `"lesson_version":1`, `"lesson_version":1,"lesson_version":1`, 1),
		"escaped-duplicate": strings.Replace(sampleLesson, `"lesson_version":1`, `"lesson_version":1,"lesson_vers\u0069on":1`, 1),
		"alias":             strings.Replace(sampleLesson, `"check_id"`, `"Check_id"`, 1),
		"null":              strings.Replace(sampleLesson, `"lesson_version":1`, `"lesson_version":null`, 1),
		"null-evidence":     strings.Replace(sampleLesson, `"Source file"`, `null`, 1),
		"negative-version":  strings.Replace(sampleLesson, `"lesson_version":1`, `"lesson_version":-1`, 1),
		"unknown":           strings.Replace(sampleLesson, `"lesson_id":`, `"command":"sh","lesson_id":`, 1),
		"nested-wrong-key":  strings.Replace(sampleLesson, `"condition":"exists"`, `"condition":"exists","lesson_id":"lesson"`, 1),
		"path":              strings.Replace(sampleLesson, `"main.go"`, `"../main.go"`, 1),
		"trailing":          sampleLesson + `{}`,
		"oversized":         strings.Repeat(" ", MaxLessonBytes) + sampleLesson,
		"invalid-utf8":      sampleLesson + string([]byte{255}),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeLesson([]byte(body), "course", "05.1"); err == nil {
				t.Fatal("unsafe response accepted")
			}
		})
	}
	for _, count := range []int{0, 2, 17} {
		value := p
		value.Missions = nil
		for range count {
			value.Missions = append(value.Missions, p.Missions[0])
		}
		raw, _ := json.Marshal(value)
		if _, err := DecodeLesson(raw, "course", "05.1"); err == nil {
			t.Fatal("invalid mission count or duplicate accepted", count)
		}
	}
	for _, slug := range []string{"", ".", "..", "../outside", "x?user=y", "x#f", "x/y", "%2e", "x\n"} {
		if ValidSlug(slug) {
			t.Fatal("unsafe route", slug)
		}
	}
}

func FuzzLessonPlan(f *testing.F) {
	f.Add([]byte(sampleLesson))
	f.Add([]byte(`{}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		p, err := DecodeLesson(raw, "course", "05.1")
		if err != nil {
			return
		}
		again, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := DecodeLesson(again, "course", "05.1"); err != nil {
			t.Fatal("unstable accepted plan", err)
		}
	})
}

func TestCodeResponseMustBindRequestedCode(t *testing.T) {
	raw := strings.Replace(sampleLesson, `"lesson_slug":"05.1"`, `"lesson_slug":"canonical-lesson","lesson_code":"05.1"`, 1)
	p, err := DecodeCodeLesson([]byte(raw), "05.1")
	if err != nil || p.LessonSlug != "canonical-lesson" {
		t.Fatal(p, err)
	}
	if _, err := DecodeCodeLesson([]byte(raw), "05.2"); err == nil {
		t.Fatal("wrong code accepted")
	}
	if _, err := DecodeCodeLesson([]byte(sampleLesson), "05.1"); err == nil {
		t.Fatal("missing code accepted")
	}
	for _, code := range []string{"5.1", "05.0", "05.01", "../05.1", "05.1?x=y", "05.1/other"} {
		if ValidLessonCode(code) {
			t.Fatal("invalid code", code)
		}
	}
}
