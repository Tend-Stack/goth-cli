package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"gothstack.com/cli/internal/credentials"
	"gothstack.com/cli/internal/device"
	"gothstack.com/cli/internal/doctor"
)

func runDoctor(ctx context.Context, args []string, out, stderr io.Writer, keychain credentials.Keychain, collect func(context.Context) (doctor.Report, error)) int {
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	server := flags.String("server", "https://gothstack.com", "server origin")
	storage := flags.String("storage", "auto", "auto or file")
	dir := flags.String("config-dir", "", "private configuration directory")
	local := flags.Bool("local", false, "print diagnostics without accessing credentials or uploading")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "Invalid doctor options. Run goth help.")
		return 2
	}
	// Local diagnostics do not even construct a client or access the keychain.
	// Default mode saves to the account explicitly connected with goth login.
	var client *device.Client
	var bearer string
	if !*local {
		var err error
		client, err = device.New(*server)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		defer client.Close()
		if *dir == "" {
			*dir, err = credentials.DefaultDirectory()
			if err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
		}
		store, err := credentials.Open(ctx, *dir, client.Origin, *storage, keychain)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		current, ok := store.Current()
		// Keep the credential's per-origin lock through the upload so replacement
		// cannot silently race the account chosen for this operation.
		defer store.Close()
		if !ok || current.ExpiresUnix <= time.Now().Unix() {
			fmt.Fprintln(stderr, "Run goth login first, or goth doctor --local for diagnostics without uploading.")
			return 1
		}
		bearer = current.AccessToken
		fmt.Fprintln(out, "Server: "+client.Origin+"\nThis report saves tool names, versions, platform and collection time to your private connected-terminal settings. It contains no project paths or raw command output.")
	}
	report, err := collect(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(stderr, "Doctor canceled. No report was uploaded.")
			return 130
		}
		fmt.Fprintln(stderr, "Could not collect toolchain diagnostics. No report was uploaded.")
		return 1
	}
	if !report.Valid() {
		fmt.Fprintln(stderr, "Could not produce a valid toolchain report. No report was uploaded.")
		return 1
	}
	fmt.Fprintf(out, "Platform: %s/%s\n", report.OS, report.Arch)
	labels := map[string]string{"go": "Go", "git": "Git", "templ": "Templ", "wails3": "Wails v3", "java": "Java/JDK", "android_sdk": "Android SDK", "android_ndk": "Android NDK", "xcode": "Xcode"}
	for _, tool := range report.Tools {
		value := tool.Status
		if tool.Status == "found" {
			value = tool.Version
		}
		fmt.Fprintf(out, "%-14s %s\n", labels[tool.ID], value)
	}
	fmt.Fprintln(out, "Go and Git are the starting tools. Install the other tools when your lesson needs them. A reported version does not prove a project builds.")
	if *local {
		fmt.Fprintln(out, "Local report only. Nothing was uploaded.")
		return 0
	}
	received, err := client.UploadDoctor(ctx, bearer, report)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(stderr, "Upload canceled. Check connected-terminal settings before retrying.")
			return 130
		}
		fmt.Fprintln(stderr, err)
		fmt.Fprintln(stderr, "Upload was not confirmed. Check connected-terminal settings before retrying.")
		return 1
	}
	fmt.Fprintln(out, "Toolchain report saved at "+received.UTC().Format("2006-01-02 15:04 UTC")+".\nView it at "+client.Origin+"/settings/security/cli")
	return 0
}
