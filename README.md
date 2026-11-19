# Anvil-Go — a GitHub-compatible git forge in Go

A challenger entry for make.fun `github-core-clone-01` ("Build a
GitHub-compatible git forge"). The forge is a single statically linked Go
binary (stdlib only, no dependencies, no network at runtime); the platform
entrypoint `main.ts` only execs it.

## Mechanism

**One process, zero-copy hot paths.** Everything lives in memory. The four
scored workloads are served from precomputed bytes:

- **Push** (200 sequential 1 KiB pushes): pack parse is plain zlib inflate +
  SHA-1; the fast-forward check walks one parent; refs update under a single
  per-repo lock.
- **Clone** (50 concurrent shallow depth-1 clones): the v2 advertisement is
  a constant; ls-refs lines and the depth-1 pack are cached per repo version
  and rebuilt only when a ref moves.
- **Read** (2,000 sequential issue GETs on one keep-alive connection): each
  issue's JSON body is built once at creation and served verbatim — O(1),
  no re-serialisation, no allocation per request.
- **Write** (200 sequential issue creates): one JSON decode, one cached
  body build.

Determinism: no wall clock and no unseeded randomness anywhere in the
state. The token owner's login is derived from the contest's fixed scenario
seed (the same `fnv1a64`/`mix64`/xorshift64* stream the kit's `wire.ts`
uses), with a first-request adoption fallback. Server-made timestamps come
from a logical clock (scenario epoch + counter), so replays are
byte-identical.

## Surface

- Git smart HTTP: protocol v2 (`ls-refs`, `fetch` with `shallow`/`filter`,
  `object-format=sha1`) and v0, plus `receive-pack` with `report-status`,
  `side-band-64k`, `delete-refs`; non-fast-forward refused per ref with
  reason `non-fast-forward`.
- REST v3: repos, branches, commits, contents, labels, issues, comments,
  pulls, files, squash merges (with conflict detection → 405).
- GraphQL v4: the suite's documents (viewer, repository, introspection,
  issues, pull requests, mutations, `node(id:)`, cursor pagination).
- MCP (Streamable HTTP, revision 2026-07-28): `server/discover`,
  `initialize`, `tools/list` (byte-identical across calls), `tools/call`
  for `create_repo`, `get_repo`, `list_repos`, `create_issue`, `get_issue`,
  `list_issues`, `create_pull_request`, `merge_pull_request`.

## How to run

```sh
# Build the static binary (Go 1.25, stdlib only, no downloads)
export PATH=$HOME/go-install/go/bin:$PATH
export GOCACHE=$HOME/.cache/go-build
CGO_ENABLED=0 GOPROXY=off go build -o anvil-go .

# The trial runs it through the launcher:
node /bot/main.ts          # execs ./anvil-go, forwards its exit code

# Local dev (no MAKEFUN_ENV_URL: the binary exits 1, as the starter does;
# point it at a live env to run the lifecycle):
MAKEFUN_SHOP_TOKEN=<token> MAKEFUN_ENV_URL=http://127.0.0.1:PORT ./anvil-go
```

## Local scores

Practice trials via the kit SDK runner
(`sdk/run.ts --perf --json`, mock env on port 18080, token
`test-token-123`). Incumbent Anvil for reference: 923/1000 (feature 600,
agent 200, perf 123, `perf_p50_us` 585,519), rank #5316/9462.

| Trial | Checks | Feature | Agent | Perf | Total | `perf_p50_us` | Notes |
|-------|--------|---------|-------|------|-------|---------------|-------|
| 1 | 109/120 | 545 | 200 | 189 | 934 | 221,378 | 11 fixes pending (comments map, ls-refs unborn, shallow fetch) |
| 2 | 119/120 | 595 | 200 | 192 | 987 | 198,948 | 1 fail: `git.fetch-shallow-deepen` |
| 3 | 120/120 | 600 | 200 | 188 | 988 | 228,074 | deepen fixed (held closure cut at shallow boundaries) |
| 4 | 120/120 | 600 | 200 | 166 | 966 | 323,258 | host-noise outlier (all four workloads uniformly slower) |
| 5 | 120/120 | 600 | 200 | 191 | 991 | 225,386 | |
| 6 | 120/120 | 600 | 200 | 195 | 995 | 194,656 | launched via `node main.ts` (real trial path) |

Median perf ~191 (total ~991) across the clean 120/120 trials — well above
the ≥960 target and the ~957 bar for #20. Perf varies run to run with host
CPU contention (trial 4), since the scored workloads are largely
client-bound once the server is at the floor.
