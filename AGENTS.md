# bonya-sdk-go: working method

This repo follows `~/dev/Bonya2/AGENTS.md` (build only on dev0, commit early,
no upstream project names, ask the owner before adding machinery). The server
contract is Bonya2's REST API and edge (`internal/api`, `internal/edge`,
`docs/capability-tokens.md`).

SDK rules:

- Callers never see HTTP statuses. Every failure is an `*Error` whose `Kind`
  is one of the `Err*` values in `errors.go`; map new service codes there.
- One idempotency key per logical mutation, kept across retries. Never resend
  an exec or write whose outcome is unknown (`ErrOutcomeUnknown`).
- Exec, files and sessions go through `Sandbox.edgeDo` / `edgeJSON` so the
  sandbox capability is used and refreshed in one place.
- Standard library first; a new dependency needs a reason.

Build and vet on dev0:

    COPYFILE_DISABLE=1 tar --no-xattrs -C ~/dev/bonya-sdk-go --exclude .git -cf - . | ssh ahmad@100.83.232.69 "mkdir -p /var/lib/bonya/bonya2-src/sdk-go && tar -C /var/lib/bonya/bonya2-src/sdk-go -xf -"
    ssh ahmad@100.83.232.69 "cd /var/lib/bonya/bonya2-src/sdk-go && PATH=\$PATH:/usr/local/go/bin TMPDIR=/var/lib/bonya/bonya2-tmp go vet ./... && go build ./..."
