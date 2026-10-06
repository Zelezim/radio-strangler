# radio-strangler

Migrating a legacy PHP API to Go one route at a time, with zero downtime and proof at every step.

## The Strangler Fig pattern

Rewriting a live system in one go ("big bang") means a long freeze, a risky cutover and no way back.
The Strangler Fig pattern takes the opposite path: put a proxy in front of the legacy system and move
traffic to the new implementation **one route at a time**. Each route only advances when there is
evidence that the new code behaves like the old one, and any route can be rolled back instantly by
flipping its mode. Eventually the legacy system serves nothing and can be removed.

Here, a Go reverse proxy sits in front of a legacy PHP "station API" (online radio domain). The proxy
and the new Go API ship as a single binary; the PHP API runs as a separate service.

## Route modes

| Mode     | Who serves the client | What happens                                                                                       |
|----------|-----------------------|----------------------------------------------------------------------------------------------------|
| `legacy` | PHP                   | Plain pass-through to the legacy API. Starting point for every route.                              |
| `shadow` | PHP                   | The request is also replayed asynchronously against Go and both responses are compared. Users never see the Go response. |
| `canary` | PHP or Go             | N% of clients are routed to Go, selected by a deterministic FNV hash per client, so each client gets a consistent experience. |
| `go`     | Go                    | Migration done for this route. PHP no longer receives its traffic.                                 |

## Architecture

_Coming soon._

## Running locally (docker compose)

_Coming soon._

## Cloud deployment (Render + Supabase)

_Coming soon._

## Admin API

_Coming soon._

## Design decisions

_Coming soon._

## License

[MIT](LICENSE)
