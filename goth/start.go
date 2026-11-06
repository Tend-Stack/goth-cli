package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"gothstack.com/cli/internal/credentials"
	"gothstack.com/cli/internal/device"
	"gothstack.com/cli/internal/plan"
	"gothstack.com/cli/internal/workspace"
)

func runStart(ctx context.Context, args []string, out, stderr io.Writer, keychain credentials.Keychain) int {
	flags := flag.NewFlagSet("start", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	server := flags.String("server", "https://gothstack.com", "server origin")
	storage := flags.String("storage", "auto", "auto or file")
	config := flags.String("config-dir", "", "private configuration directory")
	dir := flags.String("workspace", "", "project directory; defaults to the plan's project name")
	// Accept the documented lesson-first spelling as well as flag-first syntax.
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		args = append(append([]string{}, args[1:]...), args[0])
	}
	if flags.Parse(args) != nil || flags.NArg() != 1 {
		fmt.Fprintln(stderr, "Usage: goth start <05.1|course/lesson> [--workspace path] [--server origin] [--storage auto|file] [--config-dir path]")
		return 2
	}
	ref := flags.Arg(0)
	course, slug, canonical := strings.Cut(ref, "/")
	if canonical && (!plan.ValidSlug(course) || !plan.ValidSlug(slug)) || !canonical && !plan.ValidLessonCode(ref) {
		fmt.Fprintln(stderr, "Use a curriculum code such as 05.1 or the canonical course/lesson reference.")
		return 2
	}
	client, e := device.New(*server)
	if e != nil {
		fmt.Fprintln(stderr, e)
		return 2
	}
	defer client.Close()
	if *config == "" {
		*config, e = credentials.DefaultDirectory()
		if e != nil {
			fmt.Fprintln(stderr, e)
			return 1
		}
	}
	store, e := credentials.Open(ctx, *config, client.Origin, *storage, keychain)
	if e != nil {
		fmt.Fprintln(stderr, e)
		return 1
	}
	defer store.Close()
	current, ok := store.Current()
	if !ok || current.ExpiresUnix <= time.Now().Unix() {
		fmt.Fprintln(stderr, "Run goth login first.")
		return 1
	}
	var reviewed plan.Lesson
	if canonical {
		reviewed, e = client.FetchPlan(ctx, current.AccessToken, course, slug)
	} else {
		reviewed, e = client.FetchCode(ctx, current.AccessToken, ref)
	}
	if e != nil {
		fmt.Fprintln(stderr, e)
		return 1
	}
	if *dir == "" {
		*dir = reviewed.Missions[0].Plan.Project
	}
	// Only create the selected leaf, never a chain of arbitrary missing parents.
	if e = os.Mkdir(*dir, 0755); e != nil && !os.IsExist(e) {
		fmt.Fprintln(stderr, "Could not create the workspace directory.")
		return 1
	}
	w, e := workspace.Open(ctx, *dir)
	if e != nil {
		fmt.Fprintln(stderr, e)
		return 1
	}
	defer w.Close()
	prepared, e := w.Prepare(client.Origin, reviewed)
	if e != nil {
		fmt.Fprintln(stderr, e)
		return 1
	}
	fmt.Fprintf(out, "Server: %s\nLesson: %s/%s (version %d)\nWorkspace: %q\nBranch: %s\n", client.Origin, reviewed.CourseSlug, reviewed.LessonSlug, reviewed.LessonVersion, *dir, prepared.Branch())
	confirmed, e := client.StartPlan(ctx, current.AccessToken, reviewed)
	if e != nil {
		fmt.Fprintln(stderr, e)
		fmt.Fprintln(stderr, "The server start was not confirmed. No starter files were written; retry this command to check the pinned lesson.")
		return 1
	}
	if e = prepared.Apply(ctx, confirmed); e != nil {
		fmt.Fprintln(stderr, e)
		fmt.Fprintln(stderr, "The lesson is started on your account, but local preparation did not finish. Preserve your files and retry after resolving the reported problem.")
		return 1
	}
	fmt.Fprintln(out, "Lesson started. Starter files are ready; existing learner edits were preserved. No project code ran and no completion credit was awarded.")
	return 0
}
