# forge — a standalone GitHub-compatible git forge

[![make.fun](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/silver-lark-56/github-core-clone-01/rank.svg)](https://make.fun/bounty/github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/silver-lark-56/github-core-clone-01/reward.svg)](https://make.fun/bounty/github-core-clone-01)

An entry for [github-core-clone-01 on make.fun](https://make.fun/bounty/github-core-clone-01):
one process that speaks GitHub — git smart HTTP, REST v3, GraphQL and MCP —
with no database, no git binary and no dependencies beyond the Go standard
library.

## What it is

`forge` is a single statically linked Go binary (plus a thin `main.ts`
launcher the platform runs as `node /bot/main.ts`). Everything lives in
memory: repositories, git objects, issues, pull requests, labels and
comments, all behind one store with one lock.

- **Git smart HTTP** — upload-pack protocol v2 (fetch, ls-refs, shallow
  and deepen) with a v0 fallback, receive-pack v0 with report-status and
  side-band-64k, packfile generation and parsing (OFS/REF deltas resolved
  on read, full objects on write), SHA-1 objects, fast-forward-only
  updates with per-ref ok/ng reports.
- **REST v3** — repos, branches, commits, contents, labels, issues,
  comments and pull requests with squash merge and real three-way
  conflict detection, at `/api/v3`.
- **GraphQL** — viewer, repository, node, issues with cursor pagination,
  and the create/close/merge mutations, at `/api/graphql`, with the
  `__type` introspection probes gh uses.
- **MCP** — JSON-RPC 2.0 Streamable HTTP at `/mcp`, era 2026-07-28
  (`server/discover`) plus the legacy initialize handshake, six tools:
  create_repo, get_repo, create_issue, list_issues,
  create_pull_request, merge_pull_request.

## Design

- **Raw TCP, hand-rolled HTTP/1.1** — no `net/http`, no `encoding/json`:
  the request parser, the JSON parser and the JSON writer are all
  written for this program, so the hot paths allocate almost nothing.
- **Deterministic** — timestamps come from `MAKEFUN_TRIAL_NOW`, ids are
  sequential, and no wall clock or randomness touches any state, so a
  replayed trial produces byte-identical answers.
- **Cached where it counts** — serialized issue JSON per issue, generated
  packs keyed by (wants, haves, depth, shallow), and ref advertisements
  cached per repository version.

## Run it locally

```sh
go build -o forge .          # or use the committed ./forge binary
FORGE_PORT=8080 MAKEFUN_SHOP_TOKEN=test ./forge
```

Without `MAKEFUN_ENV_URL` the forge serves on `$FORGE_PORT` (default
8080) forever. In a trial it binds an ephemeral port, registers it at
`$MAKEFUN_ENV_URL/register`, serves until `$MAKEFUN_ENV_URL/done`
answers `{"done": true}`, and exits 0.

## Validate it

The kit's sealed suite runs against a local forge with the SDK runner:

```sh
node --experimental-strip-types sdk/run.ts --url http://127.0.0.1:8080 --token test --perf --json
```

## Layout

| file | what it holds |
|---|---|
| `main.ts` | the platform entry point: spawns `./forge` |
| `main.go` | lifecycle: env register/done, local mode |
| `http.go` | raw-TCP HTTP/1.1 server and router |
| `json.go` | hand-rolled JSON parser and writer |
| `store.go` | the in-memory store and id allocation |
| `gitobj.go` | git objects, packs, fetch closure, deltas |
| `githttp.go` | smart-HTTP endpoints and pkt-line codec |
| `rest.go` | REST v3 |
| `merge.go` | three-way merge and conflict detection |
| `graphql.go` | GraphQL parser and executor |
| `mcp.go` | MCP JSON-RPC endpoint |
| `forge` | the built static binary (linux/amd64) |

## License

MIT — see [LICENSE](LICENSE).
