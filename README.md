# claudit

Counts tokens locally from Claude Code transcripts and prices them against the
provider usage those transcripts already carry.

Provider dashboards give you a total. This gives you the breakdown: what filled
the context, what the model produced, and what each part cost — reconstructed
from `~/.claude/projects/**/*.jsonl` rather than taken on trust.

> Inspired by [Martin Monperrus](https://www.monperrus.net/martin/measuring-tokens)

## Build

```sh
go build -o claudit .
```

One dependency (`tiktoken-go`). The `o200k_base` table downloads once and caches;
set `TIKTOKEN_CACHE_DIR` to control where.

## Usage

```
claudit [-v] [-json] [path...]           one session: where the money went
claudit insights [-days N] [-here] [path...]  your setup: what it costs you

  path    a .jsonl transcript, or a ~/.claude/projects/<slug> directory
          default: the project matching $PWD; insights defaults to every project
  -v      per-inference-call table
  -json   machine-readable output
  -days   insights: how far back to look (default 1 = today, 0 for everything)
  -here   insights: only the project in the current directory
```

### Audit the session you're in

```sh
$ cd ~/work/my-project && ./claudit
```

```
  claude-opus-5 · 36 model calls · 4 prompts from you

  THE BILL                                            tokens      cost
  output         everything Claude wrote              54,426     $1.36
  cache reads    context re-read on every call     3,241,199     $1.62
  cache writes   saved so re-reads are cheap         130,700     $1.31
  fresh input    text it had never seen before            72    <$0.01
  ──────────────────────────────────────────────────────────────────
  total                                            3,426,397     $4.29
                 per model call                                  $0.12
                 per prompt you sent                             $1.07

  WHAT IT WAS READING                                 tokens     share
  replay         prior turns re-sent each call     2,761,860     96.9%
  attachments    CLAUDE.md, hooks, skill lists        76,324      2.7%
  tool results   files read, commands run             11,085      0.4%
  your prompts   the words you actually typed            184      0.0%
  not in the log system prompt + tool schemas        522,518     15.5%
                 ≈ per model call                     14,514

  WHAT IT PRODUCED                                    tokens     share
  reasoning      thinking, billed either way          22,135     50.5%
  tool calls     commands and edits it issued         20,677     47.2%
  text           the words you read                    1,021      2.3%
```

### One transcript

```sh
$ ./claudit ~/.claude/projects/-Users-me-work-app/d13c23d7-….jsonl
```

### Every session you've ever run

```sh
$ ./claudit ~/.claude/projects/*/
```

Each session gets its own report, followed by a merged one. Sessions on different
models are priced individually and summed — no single rate is applied to the merge.

### Personal insights — what your own setup costs

```sh
$ ./claudit insights            # today, every project
$ ./claudit insights -days 30   # last 30 days; -days 0 for all time
$ ./claudit insights -here      # today, current project only
```

The main report says where the money went. `insights` says _whose fault it is_ —
your hooks, your CLAUDE.md, the MCP servers you have connected, the skills you
load, the tools you reach for.

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

Safe to share with a colleague: it prints names and token counts, never
conversation content.

### Per-call detail

```sh
$ ./claudit -v
```

```
    n   prompt    tools   attach     replay in(actual)       out      cost
   33        0        6   24,334     95,774    150,686       467     $0.45
   34        0    1,642       13    120,480    153,510     8,346     $0.31
   35        0       12       13    129,053    161,877        99     $0.16
```

`in(actual)` is the provider's own input count for that call; the columns left of
it are the local reconstruction. The difference is what the transcript can't see.

### Scripting

```sh
$ claudit -json ~/.claude/projects/*/ | jq -r '
    sort_by(-.cost_usd) | .[:5][] | "\(.session)  $\(.cost_usd|floor)"'
```

```sh
# what has this month cost so far?
$ claudit -json ~/.claude/projects/*/ | jq '[.[].cost_usd] | add'
```

## Reading the numbers

**THE BILL** is provider-reported usage, priced at list rates — Opus 5 $5/$25 per
million in/out, Sonnet 5 $2/$10, Haiku 4.5 $1/$5, Fable 5/5.1 $10/$50. Cache
writes cost 1.25× input at the 5-minute TTL and 2× at one hour (transcripts record
which, so the two are priced separately); cache reads cost 0.1×. An unrecognised
model prints tokens with no invented price.

**WHAT IT WAS READING / PRODUCED** is the local reconstruction, tokenized with
`o200k_base` — the wrong tokenizer family for Claude, since no public Claude BPE
exists. Treat these as proportions, not exact counts. They are never used to
compute cost.

**not in the log** is the gap between the two: the system prompt, tool schemas,
and other scaffolding the client never writes down. Typically 12–37k tokens per
call. It is the number no dashboard will give you, and the reason the local and
provider columns are kept apart.

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
