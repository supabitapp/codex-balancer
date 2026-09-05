# Development and verification

Use the Go version declared in `go.mod` (Go 1.26). From an isolated checkout or
worktree, run:

```sh
./scripts/verify
```

This runs `go vet`, the full test suite with the race detector and cache disabled,
and a temporary binary build and version check. It returns on the first failure
and removes its build directory. It does not read your account database or deploy.
The first run needs network access to download Go dependencies; the race detector
requires a supported platform and C toolchain.

The HTTP and WebSocket tests use local `httptest` servers and temporary SQLite
stores. They exercise proxy routing, account changes, authentication, usage
attribution, and dashboard rendering without real account credentials. Repeat a
suspected scheduling failure with, for example:

```sh
go test -race ./internal/app -run '^TestWebSocketSocketsChooseIndependentAccounts$' -count=50
```

For manual server debugging, build a binary into a private temporary directory,
pass a new `-state` path, choose an unused loopback `-addr`, and use `-no-tui` and
`-log-file ''` to keep logs on stderr. An empty test pool can use `-no-auth`;
never reuse your normal database for destructive account tests. `-poll 0` disables
usage polling. The automated suite is the credential-free verification path;
browser interaction, real upstream compatibility, and Linux systemd/socket
activation need their own environment checks.

## Resource sampling benchmark

```sh
go test ./internal/app -run '^$' -bench '^BenchmarkResourceMonitorUnavailable$' -benchmem -count=5
```

The benchmark repeatedly samples within one cache interval using a missing file.
It measures the failed-read/cache path, including filesystem error allocations.
It does not measure successful Linux sampling, dashboard throughput, or proxy
latency. Run before and after sequentially on the same machine without other
builds running. To reproduce the baseline, create a temporary checkout of
`4875950`, copy the current `internal/app/resources_test.go` into it, and run the
same benchmark command (the `-run '^$'` skips the new regression test).

Measured on 5 September 2026 with Go 1.26.5, darwin/arm64, Apple M4 Pro,
five sequential runs per revision:

| Revision | Median ns/op | B/op | allocs/op |
| --- | ---: | ---: | ---: |
| `4875950` + benchmark | 950.2 | 96 | 2 |
| Failed attempts cached | 32.95 | 0 | 0 |

The median for repeated calls inside the interval fell about 96.5%. Each retry
still performs a read; the practical benefit is avoiding repeated failed syscalls
on unsupported hosts or transient read failures, not an overall server speedup.

## Deployment boundary

`deploy/deploy-codex-balancer` fetches and installs `origin/main` by default and
restarts the systemd service. Do not run it as a verification command. No checked-in
GitHub deployment workflow exists; an external scheduler or host-side trigger
could still invoke that script. Confirm that host configuration before merging
work when production deployment is prohibited.
