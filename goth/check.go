package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"gothstack.com/cli/internal/credentials"
	"gothstack.com/cli/internal/device"
	"gothstack.com/cli/internal/plan"
	"gothstack.com/cli/internal/workspace"
)

func runCheck(ctx context.Context, args []string, out, stderr io.Writer, keychain credentials.Keychain) int {
	flags := flag.NewFlagSet("check", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	server := flags.String("server", "https://gothstack.com", "server origin")
	storage := flags.String("storage", "auto", "auto or file")
	config := flags.String("config-dir", "", "private configuration directory")
	dir := flags.String("workspace", ".", "previously started workspace")
	probe := flags.String("probe-url", "", "explicit literal-loopback URL and port for HTTP checks")
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		args = append(append([]string{}, args[1:]...), args[0])
	}
	if flags.Parse(args) != nil || flags.NArg() > 1 {
		fmt.Fprintln(stderr, "Usage: goth check [05.1|course/lesson] [--workspace path] [--probe-url http://127.0.0.1:8080] [--server origin] [--storage auto|file] [--config-dir path]")
		return 2
	}
	ref := flags.Arg(0)
	requestedCourse, requestedSlug, canonical := strings.Cut(ref, "/")
	if ref != "" && (canonical && (!plan.ValidSlug(requestedCourse) || !plan.ValidSlug(requestedSlug)) || !canonical && !plan.ValidLessonCode(ref)) {
		fmt.Fprintln(stderr, "Use a curriculum code such as 05.1 or the canonical course/lesson reference.")
		return 2
	}
	client, err := device.New(*server)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	defer client.Close()
	if *config == "" {
		*config, err = credentials.DefaultDirectory()
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	store, err := credentials.Open(ctx, *config, client.Origin, *storage, keychain)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer store.Close()
	credential, ok := store.Current()
	if !ok || credential.ExpiresUnix <= time.Now().Unix() {
		fmt.Fprintln(stderr, "Run goth login first.")
		return 1
	}
	w, err := workspace.OpenStarted(ctx, *dir)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer w.Close()
	course, slug, err := w.LessonReference(client.Origin)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	var reviewed plan.Lesson
	if ref != "" && !canonical {
		reviewed, err = client.FetchCode(ctx, credential.AccessToken, ref)
	} else {
		if canonical && (requestedCourse != course || requestedSlug != slug) {
			fmt.Fprintln(stderr, workspace.ErrNotStarted)
			return 1
		}
		reviewed, err = client.FetchPlan(ctx, credential.AccessToken, course, slug)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if reviewed.CourseSlug != course || reviewed.LessonSlug != slug {
		fmt.Fprintln(stderr, workspace.ErrNotStarted)
		return 1
	}
	fmt.Fprintf(out, "Lesson: %s/%s (version %d)\nRunning local checks as your OS user. Project code can access your files and network.\n", course, slug, reviewed.LessonVersion)
	report, err := w.RunChecks(ctx, client.Origin, reviewed, *probe)
	for _, r := range report.Results {
		fmt.Fprintf(out, "%s/%s: %s (%s)\n", r.BlockID, r.CheckID, r.Status, r.Reason)
		if r.Output != "" {
			fmt.Fprintln(out, r.Output)
		}
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		fmt.Fprintln(stderr, "The local run did not finish; no evidence was uploaded.")
		return 1
	}
	// Cancellation before signing/upload must not become a completed submission.
	if ctx.Err() != nil {
		fmt.Fprintln(stderr, "Canceled; no evidence was uploaded.")
		return 1
	}
	nonce := make([]byte, 32)
	if _, err = rand.Read(nonce); err != nil {
		fmt.Fprintln(stderr, "Could not create a submission identity.")
		return 1
	}
	e, err := report.Evidence(reviewed, credential.TokenID, base64.RawURLEncoding.EncodeToString(nonce))
	if err != nil {
		fmt.Fprintln(stderr, "Could not validate the local check report.")
		return 1
	}
	key, err := base64.RawURLEncoding.Strict().DecodeString(credential.SigningSecret)
	if err != nil {
		fmt.Fprintln(stderr, "Invalid terminal signing key; run goth login --replace.")
		return 1
	}
	raw, err := plan.SignEvidence(e, key)
	clear(key)
	if err != nil {
		fmt.Fprintln(stderr, "Could not sign the local check report.")
		return 1
	}
	receipt, err := client.UploadEvidence(ctx, credential.AccessToken, raw)
	if err != nil {
		fmt.Fprintln(stderr, err)
		fmt.Fprintln(stderr, "Evidence upload was not confirmed. No automatic retry was made; check the lesson before running again.")
		return 1
	}
	fmt.Fprintf(out, "Evidence saved: %s\nOnly typed results, tool versions, timing and Git metadata were uploaded. Evidence upload does not complete the lesson or award XP.\n", receipt.SubmissionID)
	for _, m := range receipt.Missions {
		if m.Verdict != "pass" {
			return 1
		}
	}
	return 0
}
