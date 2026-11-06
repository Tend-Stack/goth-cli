// Package device implements the versioned GoTHStack terminal login protocol.
package device

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const MediaType = "application/vnd.gothstack.v1+json"
const ClientID = "goth-cli"
const Scope = "cli:checks cli:evidence cli:doctor"

var (
	ErrProtocol    = errors.New("the login service returned an invalid response")
	ErrUnavailable = errors.New("the login service is unavailable; try again later")
	ErrExpired     = errors.New("the device code expired; run goth login again")
	ErrDenied      = errors.New("the connection was denied; no credential was saved")
	ErrConsumed    = errors.New("the device request is no longer available; run goth login again")
)

type Device struct {
	deadline        time.Time
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int64  `json:"expires_in"`
	Interval        int64  `json:"interval"`
}
type Grant struct {
	AccessToken   string `json:"access_token"`
	TokenType     string `json:"token_type"`
	SigningSecret string `json:"signing_secret"`
	TokenID       string `json:"token_id"`
	Scope         string `json:"scope"`
	ExpiresIn     int64  `json:"expires_in"`
}
type Client struct {
	Origin string
	http   *http.Client
	now    func() time.Time
	wait   func(context.Context, time.Duration) error
}

var hostPattern = regexp.MustCompile(`(?i)^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)*$`)
var codePattern = regexp.MustCompile(`^[A-Z2-7]{5}-[A-Z2-7]{5}$`)
var tokenIDPattern = regexp.MustCompile(`^cli_[0-7][0-9A-HJKMNP-TV-Z]{25}$`)

// Origin accepts HTTPS origins and explicit literal-loopback HTTP for local development.
// Paths, credentials, fragments, query strings and ambiguous port/host spellings are forbidden.
func Origin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Opaque != "" || u.Fragment != "" || u.RawFragment != "" || u.RawQuery != "" || u.ForceQuery || u.RawPath != "" || u.Path != "" && u.Path != "/" || u.Host == "" || len(u.Host) > 255 {
		return "", errors.New("server must be an HTTPS origin (HTTP is allowed only on a literal loopback address)")
	}
	host := strings.ToLower(u.Hostname())
	for _, r := range host {
		if r > 127 || r <= 32 {
			return "", ErrProtocol
		}
	}
	if strings.HasSuffix(host, ".") || strings.ContainsAny(u.Host, "\\%") {
		return "", ErrProtocol
	}
	if port := u.Port(); port != "" {
		p, e := strconv.Atoi(port)
		if e != nil || p < 1 || p > 65535 || strconv.Itoa(p) != port {
			return "", ErrProtocol
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return "", ErrProtocol
	}
	ip, e := netip.ParseAddr(host)
	if e != nil && !hostPattern.MatchString(host) {
		return "", ErrProtocol
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && e == nil && ip.IsLoopback()) {
		return "", errors.New("server must use HTTPS; only literal loopback addresses may use HTTP")
	}
	u.Host = strings.ToLower(u.Host)
	u.Path = ""
	if u.Scheme == "https" && u.Port() == "443" {
		u.Host = host
		if strings.Contains(host, ":") {
			u.Host = "[" + host + "]"
		}
	}
	return u.String(), nil
}
func New(origin string) (*Client, error) {
	origin, err := Origin(origin)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, TLSHandshakeTimeout: 10 * time.Second, MaxConnsPerHost: 4, MaxIdleConns: 4, IdleConnTimeout: 30 * time.Second}
	return &Client{Origin: origin, http: &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, now: time.Now, wait: func(ctx context.Context, d time.Duration) error {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}}, nil
}
func (c *Client) Close() { c.http.CloseIdleConnections() }
func ValidSecret(raw string) bool {
	b, err := base64.RawURLEncoding.DecodeString(raw)
	return err == nil && len(b) == 32 && base64.RawURLEncoding.EncodeToString(b) == raw
}
func ValidGrant(g Grant) bool {
	return strings.HasPrefix(g.AccessToken, "gs_cli_") && ValidSecret(strings.TrimPrefix(g.AccessToken, "gs_cli_")) && ValidSecret(g.SigningSecret) && tokenIDPattern.MatchString(g.TokenID) && g.TokenType == "Bearer" && g.Scope == Scope && g.ExpiresIn > 0 && g.ExpiresIn <= 90*86400
}
func ValidLabel(label string) bool {
	return label == strings.TrimSpace(label) && len(label) > 0 && len(label) <= 64 && utf8.ValidString(label) && strings.IndexFunc(label, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }) < 0
}

// Decode rejects oversized, duplicate, unknown, invalid-UTF8 and trailing JSON.
// Error text never includes server-supplied content or a credential.
func Decode(raw []byte, out any) error {
	if len(raw) > 8192 || !utf8.Valid(raw) {
		return ErrProtocol
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return ErrProtocol
	}
	typ := reflect.TypeOf(out)
	if typ == nil || typ.Kind() != reflect.Pointer || typ.Elem().Kind() != reflect.Struct {
		return ErrProtocol
	}
	allowed := map[string]bool{}
	for i := 0; i < typ.Elem().NumField(); i++ {
		field := typ.Elem().Field(i)
		if field.IsExported() {
			name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
			if name != "" && name != "-" {
				allowed[name] = true
			}
		}
	}
	keys := map[string]bool{}
	for d.More() {
		token, err = d.Token()
		if err != nil {
			return ErrProtocol
		}
		key, ok := token.(string)
		if !ok || !allowed[key] || keys[key] || len(keys) >= 16 {
			return ErrProtocol
		}
		keys[key] = true
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return ErrProtocol
		}
	}
	if token, err = d.Token(); err != nil || token != json.Delim('}') {
		return ErrProtocol
	}
	if _, err = d.Token(); err != io.EOF {
		return ErrProtocol
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(out) != nil {
		return ErrProtocol
	}
	return nil
}
func (c *Client) request(ctx context.Context, path string, payload any) (int, []byte, int, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, 0, ErrProtocol
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Origin+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, 0, ErrProtocol
	}
	r.Header.Set("Accept", MediaType)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("User-Agent", "goth-cli")
	response, err := c.http.Do(r)
	if err != nil {
		if ctx.Err() != nil {
			return 0, nil, 0, ctx.Err()
		}
		return 0, nil, 0, ErrUnavailable
	}
	defer response.Body.Close()
	media, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || media != MediaType {
		return 0, nil, 0, ErrProtocol
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 8193))
	if err != nil || len(raw) > 8192 {
		return 0, nil, 0, ErrProtocol
	}
	retry := 0
	if response.StatusCode == 429 || response.StatusCode == 503 {
		retry, err = strconv.Atoi(response.Header.Get("Retry-After"))
		if err != nil || retry < 1 || retry > 900 {
			return 0, nil, 0, ErrProtocol
		}
	}
	return response.StatusCode, raw, retry, nil
}
func (c *Client) Start(ctx context.Context, label string) (Device, error) {
	var d Device
	if !ValidLabel(label) {
		return d, errors.New("terminal label must be 1–64 UTF-8 bytes without control characters")
	}
	sent := c.now()
	status, body, _, err := c.request(ctx, "/api/cli/device/start", map[string]string{"client_id": ClientID, "label": label})
	if err != nil {
		return d, err
	}
	if status != 200 {
		return d, ErrUnavailable
	}
	if Decode(body, &d) != nil || !ValidSecret(d.DeviceCode) || !codePattern.MatchString(d.UserCode) || d.VerificationURI != c.Origin+"/cli/device" || d.ExpiresIn != 600 || d.Interval != 5 {
		return Device{}, ErrProtocol
	}
	d.deadline = sent.Add(time.Duration(d.ExpiresIn) * time.Second)
	return d, nil
}

// Await respects persistent slow-down, rate/availability backoff, cancellation
// and the original expiry. Only the requesting terminal receives the grant.
func (c *Client) Await(ctx context.Context, d Device) (Grant, time.Time, error) {
	if !ValidSecret(d.DeviceCode) || d.ExpiresIn != 600 || d.Interval != 5 || d.deadline.IsZero() || d.VerificationURI != c.Origin+"/cli/device" {
		return Grant{}, time.Time{}, ErrProtocol
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(d.ExpiresIn)*time.Second)
	defer cancel()
	deadline := d.deadline
	interval := time.Duration(d.Interval) * time.Second
	delay := interval
	for {
		remaining := deadline.Sub(c.now())
		if remaining <= 0 {
			return Grant{}, time.Time{}, ErrExpired
		}
		if err := c.wait(ctx, min(delay, remaining)); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return Grant{}, time.Time{}, ErrExpired
			}
			return Grant{}, time.Time{}, err
		}
		if !c.now().Before(deadline) {
			return Grant{}, time.Time{}, ErrExpired
		}
		sent := c.now()
		status, body, retry, err := c.request(ctx, "/api/cli/device/poll", map[string]string{"client_id": ClientID, "device_code": d.DeviceCode})
		if err != nil {
			return Grant{}, time.Time{}, err
		}
		if status == 200 {
			var g Grant
			if Decode(body, &g) != nil || !ValidGrant(g) {
				return Grant{}, time.Time{}, ErrProtocol
			}
			return g, sent.Add(time.Duration(g.ExpiresIn) * time.Second), nil
		}
		var failure struct {
			Error string `json:"error"`
		}
		if Decode(body, &failure) != nil {
			return Grant{}, time.Time{}, ErrProtocol
		}
		delay = interval
		switch {
		case status == 400 && failure.Error == "authorization_pending":
		case status == 400 && failure.Error == "slow_down":
			interval = min(600*time.Second, interval+5*time.Second)
			delay = interval
		case status == 400 && failure.Error == "access_denied":
			return Grant{}, time.Time{}, ErrDenied
		case status == 400 && failure.Error == "expired_token":
			return Grant{}, time.Time{}, ErrExpired
		case status == 400 && failure.Error == "invalid_grant":
			return Grant{}, time.Time{}, ErrConsumed
		case status == 429 && failure.Error == "rate_limited", status == 503 && failure.Error == "temporarily_unavailable":
			delay = max(interval, time.Duration(retry)*time.Second)
		default:
			return Grant{}, time.Time{}, ErrProtocol
		}
	}
}
