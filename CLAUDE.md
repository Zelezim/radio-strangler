# radio-strangler

Portfolio project: migrating a legacy PHP API to Go route by route using the Strangler Fig pattern.
A Go reverse proxy sits in front of a legacy PHP "station API" (online radio domain). Each route is in one mode:
legacy (PHP serves), shadow (PHP serves, request replayed async on Go and compared), canary (N% of clients on Go,
deterministic FNV hash per client), go (Go serves). Proxy and new Go API ship as ONE binary; PHP is a separate service.
Same images run on-prem (docker compose) and in the cloud (Render + Supabase Postgres).

## Rules
- Go 1.22+, standard library only, plus github.com/lib/pq. No ORM, no frameworks, no other deps without asking.
- Plain SQL. Migrations are embedded .sql files applied at startup, guarded by pg_advisory_lock.
- Layout: cmd/server, internal/<package>, legacy-php/, scripts/. Small packages with interfaces at the consumer side.
- log/slog JSON logs, context everywhere, explicit timeouts, graceful shutdown.
- Every change: gofmt, go vet, go test -race ./... must pass before commit.
- Code, comments, README and commit messages in English. Conventional commits.
- Comments explain WHY (design decisions), not what.
- NEVER commit .env, connection strings, tokens or keys. Review git diff --staged before every commit.
- Supabase: use the SESSION pooler connection string (port 5432), not the transaction pooler.
- The legacy JSON contract is the source of truth; the Go API must match it semantically.
