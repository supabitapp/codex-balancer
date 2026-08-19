# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Layout

One flat `package main` at the repo root, no subpackages. Each file owns a
subsystem (`pool.go`, `proxy.go`, `websocket.go`, `stats.go`) and its tests sit
beside it as `<file>_test.go`. Put new code in the file that owns the subject,
or in a new root-level file. Do not add directories for Go code.

`.worktrees/` holds checkouts of other branches. Never read or edit it, and
exclude it from shell greps.

## Verify

```sh
go test -race ./...
```

Seven seconds for the whole suite, so run it before every commit. `gofmt -l .`
and `go vet ./...` report nothing today; keep it that way.

## State schema

`stateMigrations` in `state_store.go` is an append-only list. Its length is the
schema version, and `PRAGMA user_version` records how far a database has come.
Editing a landed entry skips that change on every database that already passed
it. Add a new entry instead.

## Dashboard assets

`dashboard.go` embeds `web/` one file at a time in its `//go:embed` line, and
`web/dashboard.html` names the vendored scripts by pinned version, such as
`htmx-2.0.10.min.js`. Adding, renaming, or upgrading anything under `web/` means
editing both.

## Routing

@ROUTING.md

## Commits

Subject is `subsystem: imperative summary`, lowercase, no trailing period:
`routing: pin each websocket to one account`. The subsystem names the file or
the user-visible area, such as `routing`, `dashboard`, `models`, `proxy`,
`usage`, or `docs`.
