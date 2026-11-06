//go:build linux || darwin

package workspace

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gothstack.com/cli/internal/plan"
)

// Exercise an actual go -> test binary -> background child tree. The child has
// its own output descriptors: closing the parent's pipes alone cannot pass this
// test. Native ps works on both supported Unix hosts; cross-compilation is not
// process-cleanup acceptance.
func TestCheckReapsBackgroundChild(t *testing.T) {
	for _, mode := range []struct {
		name, body, status, reason string
		seconds                    int
	}{
		{"success", "", "pass", "command_passed", 30},
		{"failure", `t.Fatal("owned failure")`, "fail", "command_failed", 30},
		{"cancel", `time.Sleep(45*time.Second)`, "error", "canceled", 30},
		{"output", `for {fmt.Print(strings.Repeat("x",8192))}`, "error", "output_limit", 30},
		{"deadline", `time.Sleep(45*time.Second)`, "error", "timeout", 10},
	} {
		t.Run(mode.name, func(t *testing.T) {
			token := "goth-owned-" + rand.Text()
			source := fmt.Sprintf(`package lesson
import("testing";"os";"os/exec";"strconv";"time";"fmt";"strings")
var _,_=fmt.Print,strings.Repeat
func TestChild(t *testing.T){
 if os.Getenv("OWNED_CHILD")=="1" {
  if err:=os.WriteFile("child.ready",[]byte(%q),0600);err!=nil{t.Fatal(err)}
  time.Sleep(45*time.Second);return
 }
 cmd:=exec.Command(os.Args[0],"-test.run=^TestChild$","--",%q)
 cmd.Env=append(os.Environ(),"OWNED_CHILD=1")
 if err:=cmd.Start();err!=nil{t.Fatal(err)}
 deadline:=time.Now().Add(5*time.Second)
 for {if raw,err:=os.ReadFile("child.ready");err==nil&&string(raw)==%q{break};if time.Now().After(deadline){t.Fatal("child not ready")};time.Sleep(10*time.Millisecond)}
 if err:=os.WriteFile("child.pid",[]byte(strconv.Itoa(cmd.Process.Pid)),0600);err!=nil{t.Fatal(err)}
 %s
}
`, token, token, token, mode.body)
			p := checkFixture(t, []plan.Check{{ID: "child", Kind: "go_test", TimeoutSeconds: mode.seconds, GoTest: &plan.GoTest{Packages: []string{"."}}}}, plan.File{Path: "go.mod", Content: "module lesson\n\ngo 1.27.3\n"}, plan.File{Path: "lesson_test.go", Content: source})
			w := openTest(t, t.TempDir())
			applyTest(t, w, p)
			pidPath := filepath.Join(w.git.dir, "child.pid")
			ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
			defer cancel()
			done := make(chan struct{})
			if mode.name == "cancel" {
				go func() {
					defer close(done)
					tick := time.NewTicker(10 * time.Millisecond)
					defer tick.Stop()
					for {
						select {
						case <-ctx.Done():
							return
						case <-tick.C:
							if _, err := os.Stat(pidPath); err == nil {
								cancel()
								return
							}
						}
					}
				}()
			} else {
				close(done)
			}
			report, err := w.RunChecks(ctx, "https://gothstack.com", p, "")
			cancel()
			<-done
			// Read the owned child even on a failed assertion, and wait for its bounded
			// lifetime before temporary workspace teardown. Never kill a reused PID.
			raw, pidErr := os.ReadFile(pidPath)
			if pidErr != nil {
				t.Fatal("fixture did not start a child", pidErr, err, report)
			}
			pid, pidErr := strconv.Atoi(string(raw))
			if pidErr != nil || pid <= 1 {
				t.Fatal("invalid fixture PID")
			}
			t.Cleanup(func() {
				deadline := time.Now().Add(50 * time.Second)
				for ownedChildRunning(t, pid, token) && time.Now().Before(deadline) {
					time.Sleep(100 * time.Millisecond)
				}
				if ownedChildRunning(t, pid, token) {
					t.Error("owned fixture exceeded its bounded lifetime")
				}
			})
			if mode.name == "cancel" {
				if !errors.Is(err, context.Canceled) {
					t.Fatal("cancellation", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if len(report.Results) != 1 || report.Results[0].Status != mode.status || report.Results[0].Reason != mode.reason {
				t.Fatal("outcome", report.Results)
			}
			deadline := time.Now().Add(2 * time.Second)
			for ownedChildRunning(t, pid, token) && time.Now().Before(deadline) {
				time.Sleep(25 * time.Millisecond)
			}
			if ownedChildRunning(t, pid, token) {
				t.Fatal("background child survived check cleanup")
			}
			if err := w.checkScratchClean(); err != nil {
				t.Fatal("scratch survived cleanup", err)
			}
		})
	}
}

func ownedChildRunning(t *testing.T, pid int, token string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	raw, err := exec.CommandContext(ctx, "ps", "-ww", "-p", strconv.Itoa(pid), "-o", "stat=", "-o", "args=").Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 && strings.TrimSpace(string(raw)) == "" {
			return false
		}
		t.Fatal("could not inspect owned fixture process", err)
	}
	fields := strings.Fields(string(raw))
	if len(fields) == 0 {
		return false
	}
	if strings.HasPrefix(fields[0], "Z") {
		return false
	}
	// An unrelated process reusing the number is not our fixture and must not be
	// signalled. This marker is deliberately public, random fixture identity.
	for _, arg := range fields[1:] {
		if arg == token {
			return true
		}
	}
	return false
}

// A positive live-child control prevents a broken/truncated process observer
// from turning every cleanup check into a false pass.
func TestOwnedProcessObservation(t *testing.T) {
	if os.Getenv("GOTH_TEST_PROCESS_OBSERVER") == "1" {
		time.Sleep(45 * time.Second)
		return
	}
	token := "goth-owned-" + rand.Text()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestOwnedProcessObservation$", "--", token)
	cmd.Env = append(os.Environ(), "GOTH_TEST_PROCESS_OBSERVER=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		if cmd.ProcessState == nil {
			_ = cmd.Wait()
		}
	}()
	if !ownedChildRunning(t, cmd.Process.Pid, token) {
		t.Fatal("live fixture not observed")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if ownedChildRunning(t, cmd.Process.Pid, token) {
		t.Fatal("exited fixture still observed")
	}
}
