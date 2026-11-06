// Package credentials keeps terminal credentials scoped to an exact server origin.
package credentials

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"gothstack.com/cli/internal/device"
)

var (
	ErrMissing     = errors.New("no credential is stored for this server")
	ErrUnavailable = errors.New("the OS keychain is not available")
	ErrKeychain    = errors.New("the OS keychain could not be accessed; unlock it and try again")
	ErrStorage     = errors.New("credential storage is unsafe or unreadable; no credential was saved")
	ErrBusy        = errors.New("another goth process is using this server's credential storage")
)

type Record struct {
	Version       int    `json:"version"`
	Origin        string `json:"origin"`
	Backend       string `json:"backend"`
	Label         string `json:"label,omitempty"`
	AccessToken   string `json:"access_token,omitempty"`
	SigningSecret string `json:"signing_secret,omitempty"`
	TokenID       string `json:"token_id,omitempty"`
	Scope         string `json:"scope,omitempty"`
	ExpiresUnix   int64  `json:"expires_unix,omitempty"`
}

func (r Record) Valid(origin string) bool {
	return r.Version == 1 && r.Origin == origin && (r.Backend == "keyring" || r.Backend == "file") && device.ValidLabel(r.Label) && r.ExpiresUnix > 0 && device.ValidGrant(device.Grant{AccessToken: r.AccessToken, SigningSecret: r.SigningSecret, TokenID: r.TokenID, Scope: r.Scope, TokenType: "Bearer", ExpiresIn: 1})
}

type Keychain interface {
	Get(context.Context, string) (string, error)
	Set(context.Context, string, string) error
}
type Store struct {
	root                       *os.Root
	lock                       *os.File
	name, origin, key, backend string
	keychain                   Keychain
	current                    *Record
}

func DefaultDirectory() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", ErrStorage
	}
	return filepath.Join(base, "gothstack", "cli"), nil
}

// Open takes an OS-released per-origin process lock. The marker fixes the chosen
// backend: a locked/failed keychain is never silently downgraded to a file.
func Open(ctx context.Context, dir, origin, mode string, keychain Keychain) (*Store, error) {
	normalized, err := device.Origin(origin)
	if err != nil || normalized != origin || keychain == nil || mode != "auto" && mode != "file" {
		return nil, ErrStorage
	}
	if mode == "file" && runtime.GOOS == "windows" {
		return nil, errors.New("the Windows OS keychain is required; a POSIX 0600 file is not an ACL guarantee")
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return nil, ErrStorage
	}
	before, err := os.Lstat(dir)
	if err != nil || !before.IsDir() || !privateInfo(before, true) {
		return nil, ErrStorage
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, ErrStorage
	}
	after, err := root.Stat(".")
	if err != nil || !os.SameFile(before, after) {
		root.Close()
		return nil, ErrStorage
	}
	sum := sha256.Sum256([]byte(origin))
	key := hex.EncodeToString(sum[:])
	s := &Store{root: root, key: key, name: key + ".json", origin: origin, keychain: keychain}
	ok := false
	defer func() {
		if !ok {
			s.Close()
		}
	}()
	lock, err := root.OpenFile(key+".lock", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if errors.Is(err, os.ErrExist) {
		lock, err = s.privateFile(key+".lock", os.O_RDWR)
	}
	if err != nil {
		return nil, ErrStorage
	}
	s.lock = lock
	info, err := lock.Stat()
	if err != nil || !privateInfo(info, false) {
		return nil, ErrStorage
	}
	if err = lockFile(lock); err != nil {
		return nil, ErrBusy
	}
	f, err := s.privateFile(s.name, os.O_RDONLY)
	if err == nil {
		raw, e := io.ReadAll(io.LimitReader(f, 8193))
		f.Close()
		if e != nil {
			return nil, ErrStorage
		}
		var disk Record
		if device.Decode(raw, &disk) != nil || disk.Version != 1 || disk.Origin != origin {
			return nil, ErrStorage
		}
		s.backend = disk.Backend
		switch disk.Backend {
		case "file":
			if runtime.GOOS == "windows" || !disk.Valid(origin) {
				return nil, ErrStorage
			}
			s.current = &disk
		case "keyring":
			if disk.Label != "" || disk.AccessToken != "" || disk.SigningSecret != "" || disk.TokenID != "" || disk.Scope != "" || disk.ExpiresUnix != 0 {
				return nil, ErrStorage
			}
			if mode == "file" {
				return nil, errors.New("this server uses the OS keychain; refusing to change storage backend")
			}
			value, e := keychain.Get(ctx, key)
			if e == nil {
				var record Record
				if device.Decode([]byte(value), &record) != nil || !record.Valid(origin) || record.Backend != "keyring" {
					return nil, ErrStorage
				}
				s.current = &record
			} else if !errors.Is(e, ErrMissing) {
				return nil, e
			}
		default:
			return nil, ErrStorage
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, ErrStorage
	} else if mode == "file" {
		s.backend = "file"
	} else {
		value, e := keychain.Get(ctx, key)
		switch {
		case e == nil:
			var record Record
			if device.Decode([]byte(value), &record) != nil || !record.Valid(origin) || record.Backend != "keyring" {
				return nil, ErrStorage
			}
			s.current = &record
			s.backend = "keyring"
		case errors.Is(e, ErrMissing):
			s.backend = "keyring"
		case errors.Is(e, ErrUnavailable):
			if runtime.GOOS == "windows" {
				return nil, ErrUnavailable
			}
			s.backend = "file"
		default:
			return nil, e
		}
	}
	if s.backend == "keyring" {
		if err = s.write(Record{Version: 1, Origin: origin, Backend: "keyring"}); err != nil {
			return nil, err
		}
	}
	ok = true
	return s, nil
}
func (s *Store) Backend() string { return s.backend }
func (s *Store) Current() (Record, bool) {
	if s.current == nil {
		return Record{}, false
	}
	return *s.current, true
}
func (s *Store) Close() {
	if s.lock != nil {
		s.lock.Close()
		s.lock = nil
	}
	if s.root != nil {
		s.root.Close()
		s.root = nil
	}
}
func (s *Store) Save(ctx context.Context, g device.Grant, label string, expiry time.Time, replace bool) error {
	r := Record{Version: 1, Origin: s.origin, Backend: s.backend, Label: label, AccessToken: g.AccessToken, SigningSecret: g.SigningSecret, TokenID: g.TokenID, Scope: g.Scope, ExpiresUnix: expiry.Unix()}
	if !device.ValidGrant(g) || !r.Valid(s.origin) || !expiry.After(time.Now()) {
		return ErrStorage
	}
	if !replace && s.current != nil && s.current.ExpiresUnix > time.Now().Unix() {
		return errors.New("a credential is already stored; refusing to overwrite active access")
	}
	if s.backend == "keyring" {
		raw, _ := json.Marshal(r)
		if len(raw) > 2048 {
			return ErrStorage
		}
		if err := s.keychain.Set(ctx, s.key, string(raw)); err != nil {
			return err
		}
		// Verify what is readable before reporting a successful local login.
		got, err := s.keychain.Get(ctx, s.key)
		if err != nil || got != string(raw) {
			return ErrKeychain
		}
	} else if err := s.write(r); err != nil {
		return err
	}
	s.current = &r
	return nil
}
func (s *Store) privateFile(name string, flags int) (*os.File, error) {
	before, err := s.root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !privateInfo(before, false) {
		return nil, ErrStorage
	}
	f, err := s.root.OpenFile(name, flags, 0)
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) || !privateInfo(after, false) {
		f.Close()
		return nil, ErrStorage
	}
	return f, nil
}
func (s *Store) write(r Record) error {
	raw, err := json.Marshal(r)
	if err != nil || len(raw) > 8192 {
		return ErrStorage
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return ErrStorage
	}
	name := ".credential-" + hex.EncodeToString(nonce[:]) + ".tmp"
	f, err := s.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return ErrStorage
	}
	defer s.root.Remove(name)
	if _, err = f.Write(append(raw, '\n')); err != nil {
		f.Close()
		return ErrStorage
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return ErrStorage
	}
	if err = f.Close(); err != nil {
		return ErrStorage
	}
	if err = s.root.Rename(name, s.name); err != nil {
		return ErrStorage
	}
	// Directory sync is needed for durable Unix rename. Windows stores only a
	// non-secret backend marker here; credentials live in Credential Manager.
	if runtime.GOOS != "windows" {
		dir, e := s.root.Open(".")
		if e != nil {
			return ErrStorage
		}
		e = dir.Sync()
		dir.Close()
		if e != nil {
			return ErrStorage
		}
	}
	return nil
}
