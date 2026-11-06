package main

import (
	"bytes"
	"testing"
)

func TestCheckInvalidOptionsBeforeCredentials(t *testing.T) {
	for _, args := range [][]string{{"lesson"}, {"05.01"}, {"course/lesson/extra"}, {"../lesson"}, {"--unknown"}, {"--server", "http://example.test"}} {
		var out, stderr bytes.Buffer
		if code := runCheck(t.Context(), args, &out, &stderr, nil); code != 2 {
			t.Fatal(code, stderr.String())
		}
	}
}
