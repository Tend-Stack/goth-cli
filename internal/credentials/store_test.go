package credentials

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"gothstack.com/cli/internal/device"
)

type fakeKeychain struct {
	value          string
	getErr, setErr error
	sets           int
}

func (k *fakeKeychain) Get(context.Context, string) (string, error) {
	if k.getErr != nil {
		return "", k.getErr
	}
	if k.value == "" {
		return "", ErrMissing
	}
	return k.value, nil
}
func (k *fakeKeychain) Set(_ context.Context, _ string, value string) error {
	k.sets++
	if k.setErr != nil {
		return k.setErr
	}
	k.value = value
	return nil
}
func recordGrant() device.Grant {
	return device.Grant{AccessToken: "gs_cli_" + base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("a", 32))), SigningSecret: base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("b", 32))), TokenID: "cli_01ARZ3NDEKTSV4RRFFQ69G5FAV", TokenType: "Bearer", Scope: device.Scope, ExpiresIn: 3600}
}
func TestFileAtomicRoundtripAndLock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows requires native keychain")
	}
	dir := filepath.Join(t.TempDir(), "credentials")
	origin := "https://gothstack.example"
	k := &fakeKeychain{getErr: ErrUnavailable}
	s, err := Open(t.Context(), dir, origin, "auto", k)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.Backend() != "file" {
		t.Fatal("missing fallback")
	}
	if _, err = Open(t.Context(), dir, origin, "auto", k); !errors.Is(err, ErrBusy) {
		t.Fatal("concurrent login admitted", err)
	}
	if err = s.Save(t.Context(), recordGrant(), "terminal", time.Now().Add(time.Hour), false); err != nil {
		t.Fatal(err)
	}
	name := s.name
	s.Close()
	info, err := os.Stat(filepath.Join(dir, name))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("file permissions")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatal("temporary file retained", len(entries))
	}
	s, err = Open(t.Context(), dir, origin, "auto", &fakeKeychain{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r, ok := s.Current()
	if !ok || r.AccessToken != recordGrant().AccessToken || s.Backend() != "file" {
		t.Fatal("file binding lost")
	}
	if err = s.Save(t.Context(), recordGrant(), "replacement", time.Now().Add(time.Hour), false); err == nil {
		t.Fatal("silently replaced active access")
	}
	if err = s.Save(t.Context(), recordGrant(), "replacement", time.Now().Add(time.Hour), true); err != nil {
		t.Fatal(err)
	}
	if k.sets != 0 {
		t.Fatal("file path accessed keychain")
	}
}
func TestKeychainMarkerAndNoSilentDowngrade(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "credentials")
	k := &fakeKeychain{}
	s, err := Open(t.Context(), dir, "https://gothstack.example", "auto", k)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Save(t.Context(), recordGrant(), "terminal", time.Now().Add(time.Hour), false); err != nil {
		t.Fatal(err)
	}
	name := s.name
	s.Close()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), recordGrant().AccessToken) || strings.Contains(string(data), recordGrant().SigningSecret) {
		t.Fatal("keychain secret copied to disk")
	}
	k.getErr = ErrUnavailable
	if _, err = Open(t.Context(), dir, "https://gothstack.example", "auto", k); !errors.Is(err, ErrUnavailable) {
		t.Fatal("failed keychain silently downgraded", err)
	}
	if _, err = Open(t.Context(), dir, "https://gothstack.example", "file", k); err == nil {
		t.Fatal("explicit mode changed existing backend")
	}
	k.getErr = nil
	s, err = Open(t.Context(), dir, "https://gothstack.example", "auto", k)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if r, ok := s.Current(); !ok || !r.Valid("https://gothstack.example") {
		t.Fatal("keychain roundtrip")
	}
}
func TestStorageRejectsUnsafePathsAndRecords(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix file protections")
	}
	for _, kind := range []string{"directory-mode", "directory-link", "file-mode", "file-link", "hard-link", "duplicate", "wrong-origin"} {
		t.Run(kind, func(t *testing.T) {
			base := t.TempDir()
			dir := filepath.Join(base, "credentials")
			k := &fakeKeychain{getErr: ErrUnavailable}
			s, err := Open(t.Context(), dir, "https://gothstack.example", "auto", k)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Save(t.Context(), recordGrant(), "terminal", time.Now().Add(time.Hour), false); err != nil {
				t.Fatal(err)
			}
			name := filepath.Join(dir, s.name)
			s.Close()
			switch kind {
			case "directory-mode":
				err = os.Chmod(dir, 0755)
			case "directory-link":
				err = os.Rename(dir, dir+"-real")
				if err == nil {
					err = os.Symlink(dir+"-real", dir)
				}
			case "file-mode":
				err = os.Chmod(name, 0644)
			case "file-link":
				err = os.Rename(name, name+"-real")
				if err == nil {
					err = os.Symlink(name+"-real", name)
				}
			case "hard-link":
				err = os.Link(name, name+"-other")
			case "duplicate":
				err = os.WriteFile(name, []byte(`{"version":1,"version":1}`), 0600)
			case "wrong-origin":
				data, e := os.ReadFile(name)
				if e != nil {
					t.Fatal(e)
				}
				err = os.WriteFile(name, []byte(strings.ReplaceAll(string(data), "gothstack.example", "attacker.example")), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if s, err = Open(t.Context(), dir, "https://gothstack.example", "auto", k); err == nil {
				s.Close()
				t.Fatal("unsafe storage admitted")
			}
		})
	}
}
func TestFailedKeychainSaveKeepsMarkerWithoutFileFallback(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "credentials")
	k := &fakeKeychain{setErr: ErrKeychain}
	s, err := Open(t.Context(), dir, "https://gothstack.example", "auto", k)
	if err != nil {
		t.Fatal(err)
	}
	name := s.name
	if err = s.Save(t.Context(), recordGrant(), "terminal", time.Now().Add(time.Hour), false); !errors.Is(err, ErrKeychain) {
		t.Fatal(err)
	}
	s.Close()
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "access_token") {
		t.Fatal("fallback after failed secret write")
	}
}
