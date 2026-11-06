package workspace

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"gothstack.com/cli/internal/plan"
)

const maxCheckOutput = 64 << 10
const maxRunOutput = 256 << 10

var ErrPendingCheck = errors.New("an earlier local check left temporary files in .git/gothstack; inspect its process before removing its check-* directory")
var ErrNotStarted = errors.New("this workspace does not match the reviewed lesson; run goth start for that lesson first")
var ErrProbeTarget = errors.New("HTTP checks require an explicit http or https URL with a literal loopback address and port")

type CheckResult struct {
	BlockID    string `json:"block_id"`
	CheckID    string `json:"check_id"`
	Status     string `json:"status"`
	Reason     string `json:"reason"`
	DurationMS int64  `json:"duration_ms"`
	// Raw local diagnostics must never be included in an uploaded evidence bundle.
	Output string `json:"-"`
}
type CheckTool struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Version string `json:"version,omitempty"`
}

type CheckReport struct {
	StartedUnix  int64         `json:"started_unix"`
	FinishedUnix int64         `json:"finished_unix"`
	GitCommit    string        `json:"git_commit"`
	GitDirty     bool          `json:"git_dirty"`
	Tools        []CheckTool   `json:"tools"`
	Results      []CheckResult `json:"results"`
}

// RunChecks requires a previously started, matching workspace. It executes
// learner code as the local OS user: wall/output limits are NOT a sandbox.
// The caller must freshly authorize the plan and explicitly request execution.
func (w *Workspace) RunChecks(ctx context.Context, origin string, p plan.Lesson, probeURL string) (CheckReport, error) {
	digest, e := p.Digest()
	if e != nil {
		return CheckReport{}, plan.ErrInvalid
	}
	disk, e := w.read(statePath, maxStateBytes)
	if e != nil {
		return CheckReport{}, e
	}
	var s state
	if !disk.Exists || strictJSON(disk.Content, &s) != nil || !validState(s) || s.Origin != origin || s.Project != p.Missions[0].Plan.Project || s.Course != p.CourseSlug || s.Lesson != p.LessonSlug || s.Version != p.LessonVersion || s.Digest != digest {
		return CheckReport{}, ErrNotStarted
	}
	if e = w.checkScratchClean(); e != nil {
		return CheckReport{}, e
	}
	needsHTTP := false
	for _, m := range p.Missions {
		if m.Plan.Project != s.Project {
			return CheckReport{}, plan.ErrInvalid
		}
		for _, c := range m.Plan.Checks {
			if c.HTTP != nil {
				needsHTTP = true
			}
		}
	}
	var base *url.URL
	if needsHTTP || probeURL != "" {
		base, e = loopbackTarget(probeURL)
		if e != nil {
			return CheckReport{}, e
		}
	}
	// Cap the whole lesson even when individually valid mission budgets add up.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	report := CheckReport{StartedUnix: time.Now().Unix()}
	report.Tools = w.checkTools(ctx, p)
	remaining := maxRunOutput
	for _, m := range p.Missions {
		for _, c := range m.Plan.Checks {
			result := CheckResult{BlockID: m.BlockID, CheckID: c.ID}
			began := time.Now()
			if ctx.Err() != nil {
				result.Status = "skipped"
				result.Reason = "canceled"
			} else {
				checkCtx, stop := context.WithTimeout(ctx, time.Duration(c.TimeoutSeconds)*time.Second)
				result.Status, result.Reason, result.Output = w.checkOne(checkCtx, c, base)
				if checkCtx.Err() != nil {
					result.Status, result.Reason = "error", contextReason(checkCtx)
				}
				stop()
			}
			result.DurationMS = time.Since(began).Milliseconds()
			result.Output = terminalText(result.Output, min(remaining, maxCheckOutput))
			remaining -= len(result.Output)
			report.Results = append(report.Results, result)
		}
	}
	// Read metadata after checks; commit/dirty fields are claims about this local
	// workspace, never an attestation that these exact bytes passed execution.
	gitCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer stop()
	commit, code, e := w.git.command(gitCtx, "", "rev-parse", "--verify", "--quiet", "HEAD")
	if e != nil || code != 0 && code != 1 || code == 0 && !oidPattern.MatchString(commit) {
		return report, ErrGit
	}
	report.GitCommit = commit
	report.GitDirty = code == 1
	if code == 0 {
		status, statusCode, statusErr := w.git.command(gitCtx, "", "status", "--porcelain=v1", "--untracked-files=normal", "--ignore-submodules=none")
		if statusErr != nil || statusCode != 0 {
			return report, ErrGit
		}
		report.GitDirty = status != ""
	}
	report.FinishedUnix = time.Now().Unix()
	if ctx.Err() != nil {
		return report, ctx.Err()
	}
	return report, nil
}
func (w *Workspace) checkOne(ctx context.Context, c plan.Check, base *url.URL) (status, reason, output string) {
	switch c.Kind {
	case "file_assert":
		file, e := w.inspect(c.File.Path, plan.MaxFileBytes, c.File.Condition == "matches")
		if e != nil {
			return "error", "unsafe_file", ""
		}
		if ctx.Err() != nil {
			return "error", contextReason(ctx), ""
		}
		pass := false
		switch c.File.Condition {
		case "exists":
			pass = file.Exists
		case "absent":
			pass = !file.Exists
		case "matches":
			pass = file.Exists && regexp.MustCompile(c.File.Pattern).Match(file.Content)
		}
		if pass {
			return "pass", "matched", ""
		}
		return "fail", "assertion_failed", ""
	case "http_probe":
		return probe(ctx, base, c.HTTP)
	case "go_test":
		args := []string{"test", "-json", "-count=1", "-parallel=2", "-timeout=" + strconv.Itoa(c.TimeoutSeconds) + "s"}
		return w.checkProcess(ctx, append(args, c.GoTest.Packages...)...)
	case "templ_generate":
		// The project's tool directive selects its pinned generator. -check verifies
		// current generated content without replacing learner files (templ v0.3.1100).
		if e := w.directory(c.TemplGenerate.Path, false); e != nil {
			return "error", "unsafe_directory", ""
		}
		return w.checkProcess(ctx, "tool", "templ", "generate", "-check", "-w", "2", "-path", c.TemplGenerate.Path)
	}
	return "error", "invalid_check", ""
}
func contextReason(ctx context.Context) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "timeout"
	}
	return "canceled"
}
func loopbackTarget(raw string) (*url.URL, error) {
	u, e := url.Parse(raw)
	if e != nil || u.Scheme != "http" && u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || u.Opaque != "" || u.RawPath != "" || u.Path != "" && u.Path != "/" {
		return nil, ErrProbeTarget
	}
	ip, e := netip.ParseAddr(u.Hostname())
	if e != nil || !ip.IsLoopback() || ip.Zone() != "" {
		return nil, ErrProbeTarget
	}
	port, e := strconv.Atoi(u.Port())
	if e != nil || port < 1 || port > 65535 || strconv.Itoa(port) != u.Port() {
		return nil, ErrProbeTarget
	}
	u.Path = ""
	return u, nil
}
func probe(ctx context.Context, base *url.URL, p *plan.HTTP) (string, string, string) {
	if base == nil {
		return "error", "missing_target", ""
	}
	address := base.Host
	transport := &http.Transport{Proxy: nil, DisableCompression: true, DisableKeepAlives: true, MaxConnsPerHost: 1, MaxResponseHeaderBytes: 16 << 10, DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		if network != "tcp" || addr != address {
			return nil, ErrProbeTarget
		}
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", address)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, e := http.NewRequestWithContext(ctx, p.Method, base.String()+p.Path, strings.NewReader(p.Body))
	if e != nil {
		return "error", "invalid_request", ""
	}
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("User-Agent", "GoTHStack-CLI-Check")
	if p.ContentType != "" {
		req.Header.Set("Content-Type", p.ContentType)
	}
	if p.HXRequest {
		req.Header.Set("HX-Request", "true")
	}
	if p.HXRequestType != "" {
		req.Header.Set("HX-Request-Type", p.HXRequestType)
	}
	response, e := client.Do(req)
	if e != nil {
		if ctx.Err() != nil {
			return "error", contextReason(ctx), ""
		}
		return "error", "request_failed", ""
	}
	defer response.Body.Close()
	if response.Header.Get("Content-Encoding") != "" {
		return "error", "encoded_response", ""
	}
	body, e := io.ReadAll(io.LimitReader(response.Body, maxCheckOutput+1))
	if e != nil {
		if ctx.Err() != nil {
			return "error", contextReason(ctx), ""
		}
		return "error", "response_failed", ""
	}
	if len(body) > maxCheckOutput {
		return "error", "output_limit", ""
	}
	if response.StatusCode != p.Status {
		return "fail", "status_mismatch", ""
	}
	if p.ExpectedBody != nil && string(body) != *p.ExpectedBody {
		return "fail", "body_mismatch", ""
	}
	return "pass", "matched", ""
}
func checkEnvironment() []string {
	// Do not pass bearer/provider/database secrets from the calling environment.
	// This is exposure reduction, not protection from code reading the user's disk.
	allow := map[string]bool{}
	for _, k := range []string{"PATH", "HOME", "USERPROFILE", "SYSTEMROOT", "WINDIR", "TEMP", "TMP", "TMPDIR", "PATHEXT", "LANG", "LC_ALL", "TZ", "XDG_CACHE_HOME", "GOPATH", "GOCACHE", "GOMODCACHE", "CC", "CXX"} {
		allow[k] = true
	}
	env := []string{}
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if ok && allow[strings.ToUpper(key)] {
			env = append(env, entry)
		}
	}
	return append(env, "GOENV=off", "GOWORK=off", "GOTOOLCHAIN=local", "GOFLAGS=-mod=readonly -buildvcs=false -p=2", "GOMAXPROCS=2", "TEMPL_DEV_MODE=false")
}

type checkOutput struct {
	mu      sync.Mutex
	raw     []byte
	limited bool
	cancel  context.CancelFunc
}

func (o *checkOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := len(p)
	room := maxCheckOutput - len(o.raw)
	if len(p) > room {
		o.raw = append(o.raw, p[:room]...)
		o.limited = true
		o.cancel()
	} else {
		o.raw = append(o.raw, p...)
	}
	return n, nil
}
func (w *Workspace) checkProcess(ctx context.Context, args ...string) (string, string, string) {
	if !nativeChecksSupported() {
		return "error", "platform_not_accepted", ""
	}
	exe, e := exec.LookPath("go")
	if e != nil {
		return "error", "go_missing", ""
	}
	exe, e = filepath.Abs(exe)
	if e != nil {
		return "error", "go_missing", ""
	}
	if e = w.checkScratchClean(); e != nil {
		return "error", "cleanup_pending", ""
	}
	scratch := meta + "/check-" + rand.Text()
	if e = w.root.Mkdir(scratch, 0700); e != nil {
		return "error", "scratch_unavailable", ""
	}
	processCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(processCtx, exe, args...)
	cmd.Dir = w.git.dir
	temporary := filepath.Join(w.git.dir, filepath.FromSlash(scratch))
	cmd.Env = append(checkEnvironment(), "GOTMPDIR="+temporary, "TMPDIR="+temporary, "TMP="+temporary, "TEMP="+temporary)
	cmd.WaitDelay = time.Second
	boundGit(cmd)
	output := &checkOutput{cancel: cancel}
	cmd.Stdout = output
	cmd.Stderr = output
	e = cmd.Run()
	stopCheckProcess(cmd)
	cleanupErr := w.root.RemoveAll(scratch)
	output.mu.Lock()
	defer output.mu.Unlock()
	raw := string(output.raw)
	if cleanupErr != nil {
		return "error", "cleanup_failed", "Temporary check files remain at " + scratch
	}
	if output.limited {
		return "error", "output_limit", raw
	}
	if ctx.Err() != nil {
		return "error", contextReason(ctx), raw
	}
	if e != nil {
		var exit *exec.ExitError
		if errors.As(e, &exit) {
			return "fail", "command_failed", raw
		}
		return "error", "command_unavailable", raw
	}
	return "pass", "command_passed", raw
}
func terminalText(raw string, limit int) string {
	if limit <= 0 {
		return ""
	}
	var b strings.Builder
	for len(raw) > 0 {
		r, n := utf8.DecodeRuneInString(raw)
		raw = raw[n:]
		if unicode.IsControl(r) && r != '\n' && r != '\t' || unicode.Is(unicode.Cf, r) {
			continue
		}
		if b.Len()+utf8.RuneLen(r) > limit {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Summary contains only fixed diagnostic labels, never raw process output.
func (r CheckResult) Summary() string {
	return fmt.Sprintf("%s/%s: %s (%s)", r.BlockID, r.CheckID, r.Status, r.Reason)
}

var checkVersionPattern = regexp.MustCompile(`^[0-9][A-Za-z0-9.+_-]{0,63}$`)

func (w *Workspace) checkTools(ctx context.Context, p plan.Lesson) []CheckTool {
	needed := map[string]bool{"git": true}
	for _, m := range p.Missions {
		for _, c := range m.Plan.Checks {
			if c.GoTest != nil || c.TemplGenerate != nil {
				needed["go"] = true
			}
			if c.TemplGenerate != nil {
				needed["templ"] = true
			}
		}
	}
	tools := []CheckTool{}
	for _, id := range []string{"git", "go", "templ"} {
		if !needed[id] {
			continue
		}
		tool := CheckTool{ID: id, Status: "failed"}
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		var raw string
		if id == "git" {
			out, code, e := w.git.run(probeCtx, "", "--version")
			if e == nil && code == 0 {
				raw = out
			}
		} else {
			args := []string{"version"}
			if id == "templ" {
				args = []string{"tool", "templ", "version"}
			}
			status, reason, out := w.checkProcess(probeCtx, args...)
			if status == "pass" {
				raw = out
			} else if reason == "go_missing" {
				tool.Status = "missing"
			} else if reason == "platform_not_accepted" {
				tool.Status = "unsupported"
			}
		}
		cancel()
		version := ""
		fields := strings.Fields(raw)
		if id == "git" && len(fields) >= 3 && fields[0] == "git" && fields[1] == "version" {
			version = fields[2]
		}
		if id == "go" && len(fields) >= 4 && fields[0] == "go" && fields[1] == "version" {
			version = strings.TrimPrefix(fields[2], "go")
		}
		if id == "templ" && len(fields) == 1 {
			version = strings.TrimPrefix(fields[0], "v")
		}
		if checkVersionPattern.MatchString(version) {
			tool.Status = "found"
			tool.Version = version
		}
		tools = append(tools, tool)
	}
	return tools
}

// A hard CLI crash can leave a child alive. Do not guess that its scratch is
// disposable or accumulate another tree: preserve it for process-aware review.
func (w *Workspace) checkScratchClean() error {
	dir, err := w.root.Open(meta)
	if err != nil {
		return ErrWorkspace
	}
	defer dir.Close()
	entries, err := dir.ReadDir(129)
	if err != nil && err != io.EOF || len(entries) > 128 {
		return ErrWorkspace
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "check-") {
			return ErrPendingCheck
		}
	}
	return nil
}
