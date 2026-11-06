package credentials

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/zalando/go-keyring"
	"gothstack.com/cli/internal/device"
)

const keychainService = "gothstack-cli-v1"

var keyPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type keychainRequest struct {
	Action string `json:"action"`
	Key    string `json:"key"`
	Value  string `json:"value,omitempty"`
}
type OSKeychain struct{ Executable string }

func (k OSKeychain) Get(ctx context.Context, key string) (string, error) {
	return k.call(ctx, keychainRequest{Action: "get", Key: key})
}
func (k OSKeychain) Set(ctx context.Context, key, value string) error {
	_, err := k.call(ctx, keychainRequest{Action: "set", Key: key, Value: value})
	return err
}

// Delete removes one exact entry. It is used by owned native acceptance fixtures;
// it does not revoke server access and is not exposed as a logout command.
func (k OSKeychain) Delete(ctx context.Context, key string) error {
	_, err := k.call(ctx, keychainRequest{Action: "delete", Key: key})
	return err
}

type cappedOutput struct{ bytes.Buffer }

func (b *cappedOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 8192 {
		return 0, ErrStorage
	}
	return b.Buffer.Write(p)
}
func (k OSKeychain) call(ctx context.Context, request keychainRequest) (string, error) {
	if !keyPattern.MatchString(request.Key) || len(request.Value) > 2048 {
		return "", ErrStorage
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	raw, _ := json.Marshal(request)
	cmd := exec.CommandContext(ctx, k.Executable, "__keychain")
	boundHelper(cmd)
	cmd.WaitDelay = time.Second
	cmd.Stdin = bytes.NewReader(raw)
	var out cappedOutput
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	err := cmd.Run()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			switch exit.ExitCode() {
			case 20:
				return "", ErrMissing
			case 21:
				return "", ErrUnavailable
			}
		}
		if ctx.Err() != nil {
			return "", ErrKeychain
		}
		return "", ErrKeychain
	}
	return out.String(), nil
}

// Helper runs in a bounded child process so an OS unlock prompt cannot hang the
// login forever. Secrets travel through private pipes, never process arguments.
// Exit codes are fixed; raw provider errors and credentials are never logged.
func Helper(in io.Reader, out io.Writer) int {
	raw, err := io.ReadAll(io.LimitReader(in, 8193))
	if err != nil {
		return 23
	}
	var r keychainRequest
	if device.Decode(raw, &r) != nil || !keyPattern.MatchString(r.Key) || len(r.Value) > 2048 {
		return 23
	}
	var value string
	switch r.Action {
	case "get":
		if r.Value != "" {
			return 23
		}
		value, err = keyring.Get(keychainService, r.Key)
	case "set":
		if r.Value == "" {
			return 23
		}
		err = keyring.Set(keychainService, r.Key, r.Value)
	case "delete":
		if r.Value != "" {
			return 23
		}
		err = keyring.Delete(keychainService, r.Key)
	default:
		return 23
	}
	if err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return 20
		}
		var busErr dbus.Error
		var op *net.OpError
		if errors.As(err, &busErr) && (busErr.Name == "org.freedesktop.DBus.Error.ServiceUnknown" || busErr.Name == "org.freedesktop.DBus.Error.NameHasNoOwner") {
			return 21
		}
		if errors.As(err, &op) && (errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED)) {
			return 21
		}
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) || errors.Is(err, keyring.ErrUnsupportedPlatform) {
			return 21
		}
		// The library explicitly disables D-Bus autolaunch when no session bus exists.
		if strings.Contains(err.Error(), "dbus: couldn't determine address of session bus") {
			return 21
		}
		return 23
	}
	if len(value) > 8192 {
		return 23
	}
	if _, err = io.WriteString(out, value); err != nil {
		return 23
	}
	return 0
}
