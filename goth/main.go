// Command goth connects a terminal to a GoTHStack learning account.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"syscall"
	"time"

	"gothstack.com/cli/internal/credentials"
	"gothstack.com/cli/internal/device"
	"gothstack.com/cli/internal/doctor"
)

// Release builds set this to git describe --tags --long --dirty.
var version = "development"

func main() {
	if len(os.Args) == 2 && os.Args[1] == "__keychain" {
		os.Exit(credentials.Helper(os.Stdin, os.Stdout))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Could not locate the credential helper.")
		stop()
		os.Exit(1)
	}
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr, credentials.OSKeychain{Executable: exe})
	stop()
	os.Exit(code)
}
func run(ctx context.Context, args []string, out, stderr io.Writer, keychain credentials.Keychain) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		fmt.Fprintln(out, "Usage: goth login [--server https://gothstack.com] [--label name] [--storage auto|file] [--config-dir path] [--replace]\n       goth doctor [--local] [--server https://gothstack.com] [--storage auto|file] [--config-dir path]\n       goth start <05.1|course/lesson> [--workspace path] [--server origin] [--storage auto|file] [--config-dir path]\n       goth version\n\nConnect a terminal by reviewing its code in your browser. No browser opens automatically.\n--replace overwrites local credentials; it does not revoke earlier server access.")
		return 0
	}
	if args[0] == "version" && len(args) == 1 {
		v := version
		if v == "development" {
			if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
				v = info.Main.Version
			}
		}
		fmt.Fprintln(out, "goth "+v)
		return 0
	}
	if args[0] == "start" {
		return runStart(ctx, args[1:], out, stderr, keychain)
	}
	if args[0] == "doctor" {
		return runDoctor(ctx, args[1:], out, stderr, keychain, doctor.Collect)
	}
	if args[0] != "login" {
		fmt.Fprintln(stderr, "Unknown command. Run goth help.")
		return 2
	}
	flags := flag.NewFlagSet("login", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	server := flags.String("server", "https://gothstack.com", "server origin")
	label := flags.String("label", runtime.GOOS+" terminal", "reported terminal name")
	storage := flags.String("storage", "auto", "auto or file")
	dir := flags.String("config-dir", "", "private configuration directory")
	replace := flags.Bool("replace", false, "replace local credentials without revoking old server access")
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "Invalid login options. Run goth help.")
		return 2
	}
	client, err := device.New(*server)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	defer client.Close()
	if !device.ValidLabel(*label) {
		fmt.Fprintln(stderr, "The terminal label must be 1–64 UTF-8 bytes without control characters.")
		return 2
	}
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
	defer store.Close()
	if current, ok := store.Current(); ok && current.ExpiresUnix > time.Now().Unix() {
		if !*replace {
			fmt.Fprintln(out, "A local credential is already stored for "+client.Origin+". Server access is checked when it is used.\nUse --replace to connect again; replacement does not revoke the old credential.")
			return 0
		}
		fmt.Fprintln(out, "Replacing the local credential does not revoke earlier server access. Manage it at "+client.Origin+"/settings/security/cli")
	}
	backend := "OS keychain"
	if store.Backend() == "file" {
		backend = "private file (0600)"
	}
	fmt.Fprintln(out, "Server: "+client.Origin+"\nCredential storage: "+backend)
	d, err := client.Start(ctx, *label)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintln(out, "Open "+d.VerificationURI+"\nEnter code: "+d.UserCode+"\nReview the account and permissions, then approve this terminal. Waiting up to 10 minutes…")
	g, expiry, err := client.Await(ctx, d)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(stderr, "Login canceled. No local credential was saved.")
			return 130
		}
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err = store.Save(ctx, g, *label, expiry, *replace); err != nil {
		fmt.Fprintln(stderr, err)
		fmt.Fprintln(stderr, "Server access may have been issued. Revoke this terminal at "+client.Origin+"/settings/security/cli")
		return 1
	}
	fmt.Fprintln(out, "Terminal connected. Credentials saved in "+backend+".\nAccess expires: "+expiry.UTC().Format("2006-01-02 15:04 UTC"))
	return 0
}
