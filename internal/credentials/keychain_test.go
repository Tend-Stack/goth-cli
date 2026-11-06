package credentials

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == "__keychain" {
		if os.Getenv("GOTH_TEST_KEYCHAIN_HELPER") == "wait" {
			time.Sleep(time.Minute)
			os.Exit(23)
		}
		os.Exit(Helper(os.Stdin, os.Stdout))
	}
	os.Exit(m.Run())
}

func TestKeychainHelperCancellationIsBounded(t *testing.T) {
	t.Setenv("GOTH_TEST_KEYCHAIN_HELPER", "wait")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	value, err := (OSKeychain{Executable: exe}).Get(ctx, strings.Repeat("a", 64))
	if !errors.Is(err, ErrKeychain) || value != "" || time.Since(started) > 3*time.Second {
		t.Fatal("unbounded or unsafe helper cancellation")
	}
}

func TestHelperRejectsMalformedRequestsWithoutOutput(t *testing.T) {
	for _, raw := range []string{`{}`, `{"action":"get","key":"invalid"}`, `{"action":"get","action":"set"}`, `{"action":"unknown","key":"` + strings.Repeat("a", 64) + `"}`, strings.Repeat("x", 8193)} {
		var out bytes.Buffer
		if code := Helper(strings.NewReader(raw), &out); code != 23 || out.Len() != 0 {
			t.Fatal("invalid helper input admitted")
		}
	}
}

func TestNativeKeychainOwnedRoundtrip(t *testing.T) {
	if os.Getenv("GOTH_CLI_NATIVE_KEYCHAIN") != "1" {
		t.Skip("explicit native OS credential-store acceptance only")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal("locating test executable")
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		t.Fatal("fixture entropy")
	}
	origin := "https://fixture-" + hex.EncodeToString(nonce[:]) + ".invalid"
	k := OSKeychain{Executable: exe}
	dir := filepath.Join(t.TempDir(), "credentials")
	s, err := Open(t.Context(), dir, origin, "auto", k)
	if err != nil {
		t.Fatal("native keychain preflight failed:", err)
	}
	defer s.Close()
	if s.Backend() != "keyring" {
		t.Fatal("native keychain unavailable; fallback is not native acceptance")
	}
	if _, ok := s.Current(); ok {
		t.Fatal("fixture collision; refusing to touch existing credential")
	}
	key := s.key
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
		defer cancel()
		if err := k.Delete(ctx, key); err != nil && !errors.Is(err, ErrMissing) {
			t.Error("owned fixture cleanup failed:", err)
		}
		if _, err := k.Get(ctx, key); !errors.Is(err, ErrMissing) {
			t.Error("owned fixture was not removed")
		}
	}()
	if err = s.Save(t.Context(), recordGrant(), "owned acceptance fixture", time.Now().Add(time.Hour), false); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(t.Context(), dir, origin, "auto", k)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if r, ok := s.Current(); !ok || r.AccessToken != recordGrant().AccessToken || r.SigningSecret != recordGrant().SigningSecret {
		t.Fatal("native readback mismatch")
	}
	raw, err := os.ReadFile(filepath.Join(dir, s.name))
	if err != nil || bytes.Contains(raw, []byte(recordGrant().AccessToken)) || bytes.Contains(raw, []byte(recordGrant().SigningSecret)) {
		t.Fatal("native keychain secret leaked to marker")
	}
}
