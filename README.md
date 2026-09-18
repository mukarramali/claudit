# claudit

Counts tokens locally from Claude Code transcripts and prices them against the
provider usage those transcripts already carry.

Provider dashboards give you a total. This gives you the breakdown: what filled
the context, what the model produced, and what each part cost — reconstructed
from `~/.claude/projects/**/*.jsonl` rather than taken on trust.

> Inspired by [Martin Monperrus](https://www.monperrus.net/martin/measuring-tokens)

## Everything is local

`claudit` runs entirely on your machine. It reads `~/.claude/projects/**/*.jsonl`
plus the two `CLAUDE.md` files (yours and the project-level one) to estimate their
token cost. It makes no network requests of any kind, sends nothing anywhere, and
writes nothing to disk. It needs no API key or login. The provider usage figures in
the report come directly from the transcript JSON that Claude Code writes locally —
nothing is fetched from an API.

## Build

```sh
go build -o claudit .
```

Two direct dependencies: `tiktoken-go` and `tiktoken-go-loader`. The `o200k_base`
vocabulary table is compiled into the binary — nothing is downloaded at runtime;
this is what makes the binary ~7 MB larger.

## Usage

```
claudit [-days N] [-here] [-json] [-scanner] [path...]

  path      a .jsonl transcript, or a ~/.claude/projects/<slug> directory
            default: every project; -here for the current one only
  -days     how far back to look (default 1 = today, 0 for everything);
            applies to every mode, including -json and -scanner
  -here     only the project in the current directory
  -json     machine-readable totals per session
  -scanner  scan traces for accidentally shared credentials

  -scanner-ignore-iss   comma-separated JWT issuers to suppress, for the
                        SSO tokens you already know about
                        e.g. -scanner-ignore-iss https://sso.example.com
```

### What your setup costs you

```sh
$ ./claudit              # today, every project
$ ./claudit -days 30     # last 30 days; -days 0 for all time
$ ./claudit -here        # today, current project only
```

```
  YOUR CLAUDE CODE SPEND
  14 Aug → 12 Sep 2026 · all projects
  111 sessions · 2,642 model calls · $168

  ALWAYS IN CONTEXT                            tokens      cost  share
    prepended to every single call
  system prompt + tools (no snapshot)       3,039,593       $48  28.6%
  built-in tool schemas                       764,850       $10   6.0%
  system prompt (Claude Code)                 116,159     $1.21   0.7%
  subtotal                                                  $65  38.8%

  YOUR CUSTOMISATIONS                          tokens      cost  share
    hooks, CLAUDE.md, listings, skill bodies
  tool listings (MCP)                         758,247       $10   6.0%
  skill listing                               280,840     $4.52   2.7%
  subagent listing                            217,558     $2.76   1.6%
  hook SessionStart:startup                    94,234     $1.29   0.8%
  skill update-config (loaded)                 89,496     $1.20   0.7%
  …and 43 more                                366,911     $4.93   2.9%
  subtotal                                                  $29  17.0%

  TOOL RESULTS YOU READ BACK                   tokens      cost  share
  Read                                        424,739     $8.22   4.9%
  Bash                                        380,616     $5.49   3.3%
  MCP claude_ai_Snowflake                     220,022     $3.90   2.3%
  …and 33 more                                 69,031     $0.92   0.5%
  subtotal                                                  $24  14.3%

  Your own configuration — customisations, MCP servers and their
  results — accounts for $38 of $168 (22%).
```

It ends with three findings, each pinned to a number from your own sessions:

```
  WHAT TO CHANGE
  ────────────────────────────────────────────────────────────────────

  1  38 of 48 MCP servers were never called                        $10
     Their tool listings ride every call anyway. You used 10.
     Idle: claude_ai_PagerDuty, claude_ai_Figma, +36 more

  2  A call late in a session costs 1.9x an early one
     per call:  1-10 $0.05   11-25 $0.05   26-50 $0.06   51+ $0.10
     Every turn resends the ones before it. /clear between tasks.

  3  Cache is healthy — 97% read, 3% rewritten                    good
```

1. **Idle MCP servers.** A connected server's tool listing is prepended to every
   call whether you use it or not. Servers listed but never called are pure
   waste, priced by their share of the listing. Disconnecting one has no
   downside, which is what makes this the first thing to act on.
2. **Session depth.** Every turn resends the ones before it, so the same work
   costs more the later it happens. The fix — `/clear` between unrelated tasks —
   is free.
3. **Cache health.** Cached input costs 0.1× to read but 1.25–2× to write. A
   hook that emits a timestamp, or anything else that changes near the start of
   the context, quietly turns every read into a rewrite. The healthy case is
   boring, which is exactly why it is worth checking.

**How the split works.** Cost is attributed by _replay_, not by size. A token
that enters the context early is re-billed on every later call, so an item's
real cost is its size × the number of inference calls that carried it. Those
weights sum exactly to the input actually sent, so the provider's real dollars
can be divided across them — share of replay is share of the bill. A 2k-token
hook that loads at session start costs far more than a 20k tool result read once
near the end, and this is the view that shows it.

The spend report is safe to share with a colleague: it prints session names and
token counts, never conversation content.

### Credential scanner

```sh
$ ./claudit -scanner           # today, every project
$ ./claudit -scanner -days 0   # all time
$ ./claudit -scanner -here     # current project only
```

```
  Credential scan — 9 matches across 3 projects · 2026-06-16 – 2026-09-18 (94 days)
  ────────────────────────────────────────────────────────────────────

  project  ~/work/do-it-for-me
  session  e3b85756  2026-09-16
  ~/.claude/projects/-Users-you-work-do-it-for-me/e3b85756-….jsonl
    Bearer Token (iss: https://sso.staging.example.com/) Bearer e…FCCQ (1217c)
    JWT (iss: https://sso.staging.example.com/) eyJhbGci…FCCQ (1210c)

  Action: rotate any non-expired credentials listed above.
```

Scanner output contains partial secrets (first 8 and last 4 characters of each
match). Do not paste it into a chat, ticket, or PR.

Two detection passes run over every `.jsonl` file:

1. **Known-format patterns** — regex matches for things with a recognisable
   prefix regardless of context: JWTs, GitHub tokens, Bearer tokens, Stripe
   keys, Anthropic/OpenAI keys, private key PEM blocks, DB connection URLs.
2. **Shannon entropy** — finds `"key": "value"` pairs where the key name looks
   secret-sounding (`password`, `secret`, `api_key`, `client_secret`, …) and
   the value has high entropy (>4.5 bits/char for base64, >4.8 for general
   text). Catches novel or internal secrets with no recognisable prefix.

Built-in false-positive suppression: Claude Code embeds its own API key as a
JWT in every trace (`{"jti":"ApiKey:N"}`); these are detected and skipped
automatically, including where they appear as `Bearer eyJ…`. The same applies
to any issuer you pass to `-scanner-ignore-iss`.

### Scripting

```sh
$ claudit -json -days 0 | jq -r '
    sort_by(-.cost_usd) | .[:5][] | "\(.session)  $\(.cost_usd|floor)"'
```

```sh
# what have the last 30 days cost?
$ claudit -json -days 30 | jq '[.[].cost_usd] | add'
```

## Reading the numbers

**YOUR CLAUDE CODE SPEND** is provider-reported usage, priced at list rates —
Opus 5 $5/$25 per million in/out, Sonnet 5 $2/$10, Haiku 4.5 $1/$5, Fable
5/5.1 $10/$50. Cache writes cost 1.25× input at the 5-minute TTL and 2× at one
hour (transcripts record which, so the two are priced separately); cache reads
cost 0.1×. An unrecognised model prints tokens with no invented price.

**Token counts** are the local reconstruction, tokenized with `o200k_base` —
the wrong tokenizer family for Claude, since no public Claude BPE exists. Treat
these as proportions, not exact counts. They are never used to compute cost.

**not in the log** is the gap between reconstructed and provider counts: the
system prompt, tool schemas, and other scaffolding the client never writes down.
Typically 12–37k tokens per call. It is the number no dashboard will give you.

**reasoning** is usually withheld — current transcripts store thinking blocks with
empty content. Where that happens the provider's own `thinking_tokens` count is
used and the report says so.

## Known limits

- Every subagent run in a session is lumped into one "subagents" trajectory;
  separating concurrent subagents needs `parentUuid` chain-walking.
- Replay assumes nothing is ever dropped from context between calls. Providers do
  strip stale reasoning, so `replay` runs slightly high on long sessions.
- Images are sized by Anthropic's `(w×h)/750` estimate from the PNG/JPEG header.
- Claude Code only. The parser is one function; another agent means another one.

## Tests

```sh
go test ./...
```

Fixture-driven and offline. Covers the traps: Claude splits one assistant message
across several records sharing a `message.id` (counting them as separate calls
inflates usage ~2×), the replay recurrence, the compaction reset, and image sizing.
