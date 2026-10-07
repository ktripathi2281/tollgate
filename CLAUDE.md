# Tollgate

Tollgate is an OpenAI-compatible LLM gateway in Go. Clients point any OpenAI SDK at it. It authenticates the request, enforces per-key rate limits and spend budgets, routes the call to a provider with retries and fallback, streams the response back, and records the cost. What sets it apart is the money path: integer micro-USD, hold-and-settle budgets, idempotency keys and a reconcile command, built the way payment systems are.

The spec is `docs/BRIEF.md`. It is long: before writing code, read the sections of the brief that the current milestone (below) depends on, plus section 24 (revisions).

## Environment

- Go and all tooling run inside WSL Ubuntu, not on Windows. From the Windows side, run commands as:
  `MSYS_NO_PATHCONV=1 wsl -d Ubuntu --cd /mnt/d/Projects/tollgate --exec bash -lc '<command>'`
- git runs from Windows. `.gitattributes` forces LF line endings.

## Make targets

- `make build`: static binary in `bin/`
- `make test`: `go test -race ./...`
- `make lint`: golangci-lint (also checks gofmt and goimports)
- `make fmt`: format the code
- `make run`: build, then serve with `config.yaml`
- `make migrate`, `make bench`, `make smoke`: added in M4, M7 and M3

## Rules

1. One milestone at a time (brief section 16). Stop at the end of each and wait for a go-ahead.
2. At every stop, write `notes/milestone-N.md` (gitignored): what was built, how to run it, the Go concepts used with file paths, how Go's approach differs from Java and Spring, and five interview questions with short answers. Update `docs/PROGRESS.md`.
3. Plain, idiomatic Go: small interfaces defined where they're used, wrapped errors, `context.Context` first, composition, no DI framework, no reflection, generics only where they clearly simplify. Mutexes for shared state, channels for handing off work. When two designs are close, pick the easier one to explain.
4. Record every non-obvious choice in `docs/DECISIONS.md`: context, decision, alternatives rejected.
5. Ask before adding a dependency that isn't listed in brief section 13.
6. Push back when the brief is wrong, and propose a fix before building. Read the provider's current API reference before writing an adapter.
7. Never ask for, print or commit secrets. Provider keys live in `.env`.
8. Small commits, one logical change each, conventional commit messages. Ask before every push.
9. Exercises: unless told "skip exercises", leave one small function per milestone for the maintainer (brief section 2) and list it below.
10. Write about the system, never about who built it.

## Progress

@docs/PROGRESS.md
