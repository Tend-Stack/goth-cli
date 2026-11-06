# goth terminal client

This Go module implements device-code login and toolchain diagnostics. Its public repository is a generated
CLI-only source export; application code, private history and deployment files
are excluded. Development remains in the authoritative GoTHStack workspace. The application's CLI
routes remain disabled in production while T5.5's remaining commands and release
acceptance are completed. Browser approval never runs learner code.

```sh
go install gothstack.com/cli/goth@latest
goth version
goth login
goth doctor
```

Requires Go 1.27.3 or later. The public vanity metadata points to
`https://github.com/Tend-Stack/goth-cli`. Use an exact **public CLI mirror commit**
instead of `latest` for reproducible installation. `.gothstack-source.json` records
the verified application source commit and per-file hashes; the two repositories
have separate histories. Official release binaries will use the
app's `YEAR.RELEASE.FIX` tags and embed `git describe`; an untagged source install
reports Go's commit-derived pseudo-version. The source mirror currently uses pseudo-versions without release tags: Go interprets 2026 as a major API version requiring a different import path.
See [Go module discovery/version rules](https://go.dev/ref/mod#vcs-find).

## Login

`goth login` prints the server, credential backend, verification URL and short
code. Open that URL yourself, sign in, check the account and terminal label, then
approve. The terminal polls at the server's interval and saves a credential only
after approval. Denial, cancellation and expiry leave existing credentials
unchanged. Never approve codes sent by someone else.

Options:

- `--server https://gothstack.com`: exact origin; HTTPS required except literal
  loopback HTTP for local development. Redirects, browser cookies and automatic
  proxy discovery are not used.
- `--label "My laptop"`: reported name, not a trusted device identity.
- `--storage auto|file`: OS keychain preferred. Automatic file fallback is allowed
  on Unix only when the keychain is definitively unavailable. An existing
  keychain binding never silently downgrades when locked or inaccessible.
- `--config-dir path`: defaults to the OS user-config directory plus
  `gothstack/cli`; the Unix directory must be owned by the current user and 0700.
- `--replace`: explicitly replace local credentials. This **does not revoke** the
  previous server credential; revoke it in `/settings/security/cli`.

Linux uses Secret Service, macOS Keychain, and Windows Credential Manager via
pinned `go-keyring`. Windows requires Credential Manager: POSIX file permissions
do not establish Windows ACL protection. Native Linux has been exercised;
native macOS/Windows interoperability is still an acceptance requirement.

Unix fallback records are private 0600 files, atomically written and scoped to
an origin hash. Keychain markers contain no secrets. Symlinks, hardlinked files,
insecure Unix permissions and malformed records fail closed. An OS-released lock
prevents concurrent processes from replacing the same origin's credential.

Device and bearer secrets are never printed. Keychain helpers use private pipes,
bounded responses and a 30-second timeout. The credential's fixed scope is
checks/evidence/doctor, with no browser or administrator authority. Local file
permissions do not protect against malware running as the same OS user.

## Toolchain diagnostics

`goth doctor` runs fixed version probes for Go, Git, Templ, Wails v3 and Java,
reads bounded Android SDK/NDK metadata, and checks Xcode on macOS. It prints
versions and gaps, then saves one current report to your private connected-terminal
settings. Go and Git are starting tools; the others are needed as lessons introduce
them. No tool installation, project build, shell script or lesson code is run.

Use `goth doctor --local` to print without accessing credentials or uploading.
Connected mode accepts the same `--server`, `--storage` and `--config-dir` options
as login. It requires an existing unexpired credential and never logs in silently.
Uploads contain only schema, platform, collection time and eight fixed tool/status/
version entries. No host name, project path, environment values or raw output is
included. The server validates independently, rate limits updates, and removes
the report when access is revoked. Reports are diagnostic claims, not attestation.

Each version process has a five-second timeout and 8 KiB output limit. Collection
has a 45-second context; SDK metadata reads have byte/entry bounds (not a guarantee
against arbitrary operating-system filesystem stalls). Installed executables run
as your OS user; this is not a sandbox for malicious tools. Native Windows process-
tree cancellation and macOS/Windows toolchain acceptance remain release gates.

## Development checks

In the full GoTHStack workspace, root `make check` includes this module's tests,
vet, pinned Staticcheck and build. From the standalone public export, run
`go test ./...`, `go vet ./...` and `go build ./goth`.
`go -C cli test -race ./...` also checks polling, strict parsing, cancellation,
redirect refusal, file safety, locking and keychain failure handling.

Opt-in native acceptance creates and deletes only its uniquely named synthetic
entry, under a random `.invalid` origin:

```sh
GOTH_CLI_NATIVE_KEYCHAIN=1 go -C cli test ./internal/credentials -run '^TestNativeKeychainOwnedRoundtrip$' -count=1 -v
```

The real terminal/browser fixture uses a disposable local server, account,
database, browser profile and private credential directory; it exercises approval,
redemption, browser revocation and native-form denial:

```sh
go -C cli build -o ../tests/results/goth-client ./goth
GOTH_CLI_BINARY=tests/results/goth-client python3 -B scripts/browser_cli_check.py /usr/bin/firefox
```

`start`, `check`, signed evidence, prebuilt release archives and full M5
security acceptance remain roadmap work. CLI signatures authenticate an account;
they are never execution attestation.
