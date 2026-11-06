package device

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func secret(b byte) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat(string(b), 32)))
}
func grant() Grant {
	return Grant{AccessToken: "gs_cli_" + secret('a'), SigningSecret: secret('b'), TokenID: "cli_01ARZ3NDEKTSV4RRFFQ69G5FAV", TokenType: "Bearer", Scope: Scope, ExpiresIn: 90 * 86400}
}
func fixture(t *testing.T, handler http.HandlerFunc) (*Client, *[]time.Duration) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	c, err := New(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	now := time.Now()
	delays := []time.Duration{}
	c.now = func() time.Time { return now }
	c.wait = func(ctx context.Context, d time.Duration) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		delays = append(delays, d)
		now = now.Add(d)
		return nil
	}
	return c, &delays
}
func response(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", MediaType)
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}
func TestOriginAndStrictResponse(t *testing.T) {
	for _, origin := range []string{"https://gothstack.com", "https://EXAMPLE.com:443/", "http://127.0.0.1:1234", "http://[::1]:1234"} {
		if _, err := Origin(origin); err != nil {
			t.Fatal(origin, err)
		}
	}
	for _, origin := range []string{"http://example.com", "http://localhost", "https://user:secret@example.com", "https://example.com/path", "https://example.com?code=x", "https://example.com#x", "https://example.com:", "https://example.com:00080", "https://example.com.", "https://example.com%2fsecret", "https://-invalid.test", "https://evil`id`.test", "https://é.test"} {
		if _, err := Origin(origin); err == nil {
			t.Fatal("accepted origin", origin)
		}
	}
	for _, body := range []string{`{"error":"a","error":"b"}`, `{"error":"a","ERROR":"b"}`, `{"error":"a","\u0065rror":"b"}`, `{"other":"a"}`, `{"error":"a"}{}`, `[]`, `{"error":"` + string([]byte{255}) + `"}`, strings.Repeat("x", 8193)} {
		var v struct {
			Error string `json:"error"`
		}
		if Decode([]byte(body), &v) == nil {
			t.Fatal("accepted ambiguous response")
		}
	}
}
func TestLoginBackoffAndBoundGrant(t *testing.T) {
	polls := 0
	var c *Client
	c, delays := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.Header.Get("Accept") != MediaType || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" || r.URL.RawQuery != "" {
			t.Error("invalid protocol")
		}
		var fields map[string]string
		if json.NewDecoder(r.Body).Decode(&fields) != nil || fields["client_id"] != ClientID {
			t.Error("bad request")
		}
		if strings.HasSuffix(r.URL.Path, "start") {
			response(w, 200, Device{DeviceCode: secret('d'), UserCode: "ABCDE-FGHIJ", VerificationURI: c.Origin + "/cli/device", ExpiresIn: 600, Interval: 5})
			return
		}
		if fields["device_code"] != secret('d') {
			t.Error("wrong device secret")
		}
		polls++
		switch polls {
		case 1:
			response(w, 400, map[string]string{"error": "authorization_pending"})
		case 2:
			response(w, 400, map[string]string{"error": "slow_down"})
		case 3:
			w.Header().Set("Retry-After", "20")
			response(w, 429, map[string]string{"error": "rate_limited"})
		case 4:
			w.Header().Set("Retry-After", "1")
			response(w, 503, map[string]string{"error": "temporarily_unavailable"})
		default:
			response(w, 200, grant())
		}
	})
	d, err := c.Start(t.Context(), "my terminal")
	if err != nil {
		t.Fatal(err)
	}
	g, expires, err := c.Await(t.Context(), d)
	if err != nil || g.AccessToken != grant().AccessToken || !expires.After(c.now()) {
		t.Fatal("grant", err)
	}
	want := []time.Duration{5 * time.Second, 5 * time.Second, 10 * time.Second, 20 * time.Second, 10 * time.Second}
	if len(*delays) != len(want) {
		t.Fatal(*delays)
	}
	for i, v := range want {
		if (*delays)[i] != v {
			t.Fatal(*delays)
		}
	}
	other, _ := New("https://other.example")
	defer other.Close()
	if _, _, err = other.Await(t.Context(), d); !errors.Is(err, ErrProtocol) {
		t.Fatal("device moved to another origin")
	}
}
func TestLoginExpiryCancellationAndTerminalStates(t *testing.T) {
	for _, tc := range []struct {
		code string
		want error
	}{{"access_denied", ErrDenied}, {"expired_token", ErrExpired}, {"invalid_grant", ErrConsumed}, {"unexpected private server text", ErrProtocol}} {
		t.Run(tc.code, func(t *testing.T) {
			c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) { response(w, 400, map[string]string{"error": tc.code}) })
			d := Device{DeviceCode: secret('d'), VerificationURI: c.Origin + "/cli/device", ExpiresIn: 600, Interval: 5, deadline: c.now().Add(600 * time.Second)}
			if _, _, err := c.Await(t.Context(), d); !errors.Is(err, tc.want) {
				t.Fatal(err)
			}
		})
	}
	calls := 0
	c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Retry-After", "900")
		response(w, 429, map[string]string{"error": "rate_limited"})
	})
	d := Device{DeviceCode: secret('d'), VerificationURI: c.Origin + "/cli/device", ExpiresIn: 600, Interval: 5, deadline: c.now().Add(10 * time.Second)}
	if _, _, err := c.Await(t.Context(), d); !errors.Is(err, ErrExpired) || calls != 1 {
		t.Fatal("expiry exceeded", err, calls)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	d.deadline = c.now().Add(time.Minute)
	if _, _, err := c.Await(ctx, d); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
func TestLoginRejectsRedirectAndUntrustedProjections(t *testing.T) {
	leaked := false
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { leaked = true }))
	defer other.Close()
	for _, kind := range []string{"redirect", "verification", "grant", "oversize", "html", "duplicate", "retry"} {
		t.Run(kind, func(t *testing.T) {
			c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				switch kind {
				case "redirect":
					http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
				case "verification":
					response(w, 200, Device{DeviceCode: secret('d'), UserCode: "ABCDE-FGHIJ", VerificationURI: "https://attacker.example/cli/device", ExpiresIn: 600, Interval: 5})
				case "grant":
					g := grant()
					g.Scope = "admin"
					response(w, 200, g)
				case "oversize":
					w.Header().Set("Content-Type", MediaType)
					io.WriteString(w, strings.Repeat("s", 9000))
				case "html":
					w.Header().Set("Content-Type", "text/html")
					io.WriteString(w, "private server error")
				case "duplicate":
					w.Header().Set("Content-Type", MediaType)
					io.WriteString(w, `{"error":"slow_down","error":"private"}`)
				case "retry":
					w.Header().Set("Retry-After", "999999999")
					response(w, 429, map[string]string{"error": "rate_limited"})
				}
			})
			var err error
			if kind == "verification" {
				_, err = c.Start(t.Context(), "terminal")
			} else {
				_, _, err = c.Await(t.Context(), Device{DeviceCode: secret('d'), VerificationURI: c.Origin + "/cli/device", ExpiresIn: 600, Interval: 5, deadline: c.now().Add(time.Minute)})
			}
			if !errors.Is(err, ErrProtocol) {
				t.Fatal(kind, err)
			}
			if strings.Contains(err.Error(), "private") {
				t.Fatal("server content leaked")
			}
		})
	}
	if leaked {
		t.Fatal("redirect forwarded device credential")
	}
}
