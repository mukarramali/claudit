# claudit

Counts tokens locally from Claude Code transcripts and prices them against the
provider usage those transcripts already carry.

Provider dashboards give you a total. This gives you the breakdown: what filled
the context, what the model produced, and what each part cost — reconstructed
from `~/.claude/projects/**/*.jsonl` rather than taken on trust.

> Inspired by [Martin Monperrus](https://www.monperrus.net/martin/measuring-tokens)

## Everything is local

`claudit` runs entirely on your machine. It reads `~/.claude/projects/**/*.jsonl`,
including subagent runs under `<session>/subagents/` and the agent type from their
`.meta.json`. It makes no network requests of any kind, sends nothing anywhere, and
writes nothing to disk. It needs no API key or login. CLAUDE.md is not read from
disk: each transcript records the memory files that session actually loaded.
`-scanner` also reads each session's `tool-results/`, and when scanning every
project `~/.claude/file-history/`, `history.jsonl` and `paste-cache/`: the places
a pasted or read secret lands besides the transcript. Where `/insights` has run,
it reads the session type and friction counts from `~/.claude/usage-data/facets/`,
never the summaries. The
provider usage figures come directly from the transcript JSON that Claude Code
writes locally — nothing is fetched from an API.

## Build

```sh
go build -o claudit .
```

Two direct dependencies: `tiktoken-go` and `tiktoken-go-loader`. The `o200k_base`
vocabulary table is compiled into the binary — nothing is downloaded at runtime;
this is what makes the binary ~7 MB larger.

## Usage

```
claudit [-days N] [-here] [-json] [-breakdown] [-scanner] [path...]

  path       a .jsonl transcript, or a ~/.claude/projects/<slug> directory
             default: every project; -here for the current one only
  -days      how far back to look (default 1 = today, 0 for everything);
             applies to every mode, including -json and -scanner
  -here      only the project in the current directory
  -json      machine-readable totals per session
  -breakdown add the TOOL RESULTS YOU READ BACK section (per-tool token/cost
             breakdown of tool results returned to the model)
  -scanner   scan traces for accidentally shared credentials

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
  last 3 days · all projects
  28 sessions · 19 subagent runs · 727 model calls · $29
  + $3.43 (11%) more in Claude Code's own ledger: calls no transcript records

  ALWAYS IN CONTEXT                            tokens      cost  share
    prepended to every single call
  built-in tool schemas                     1,000,468     $8.90  30.4%
  unaccounted                                  79,738     $1.50   5.1%
  system prompt (Claude Code)                 175,757     $1.46   5.0%
  system prompt + tools (no snapshot)         123,853     $0.94   3.2%
  subtotal                                                  $13  43.8%

  YOUR CUSTOMISATIONS                          tokens      cost  share
    hooks, CLAUDE.md, listings, skill bodies
  tool listings (MCP)                         271,618     $2.42   8.3%
  skill listing                                87,395     $0.84   2.9%
  subagent listing                             66,310     $0.60   2.0%
  CLAUDE.md (project)                          59,825     $0.46   1.6%
  hook SessionStart:startup                    20,447     $0.18   0.6%
  session bookkeeping                          15,818     $0.14   0.5%
  MCP server instructions                      14,208     $0.12   0.4%
  session context                              11,257     $0.09   0.3%
  …and 25 more                                 68,682     $0.50   1.7%
  subtotal                                                $5.34  18.3%

  PLUGINS AND SKILLS                      listing tax  work driven
    on every call, used or not · calls it drove
  testing-principles                            $0.12        $1.20
  skill code-review                             $0.01        $0.47
  dev-toolkit · 0 of 21 used                    $0.38            -
  architecture-guidance · 0 of 5 used           $0.27            -
  figma · 0 of 14 used                          $0.20            -
  ava-analytics · 0 of 14 used                  $0.08            -
  skill claude-code-guide                       $0.07            -
  skill Explore                                 $0.01            -
  …and 29 more                                  $0.34            -

  Your own configuration — customisations, MCP servers and their
  results — accounts for $6.40 of $29 (22%).
```

Add `-breakdown` to also see `TOOL RESULTS YOU READ BACK` — a per-tool
breakdown of tokens returned by Read, Bash, MCP calls, and so on.

Below the sections, a short paragraph says where the money concentrates (the
priciest tenth of sessions, their size and model), then cost per PR and, where
`/insights` has run, per session type. The report ends with ranked findings,
each pinned to a number from your own sessions:

```
  WHAT TO CHANGE
  ────────────────────────────────────────────────────────────────────

  1  Opus 5 where Opus 5.5 is cheaper                              $91
     1,653 calls · same tokens at list price: $214 → $123
     Pinned? ANTHROPIC_DEFAULT_OPUS_MODEL: ask whoever sets it.

  2  Subagents ran on your main model: $31 of $42                  $23
     ~$7.47 on Haiku · general-purpose $24, Explore $7.08
     Set model: in the agent, or CLAUDE_CODE_SUBAGENT_MODEL.

  3  Resuming idle sessions rewrote the cache 24 times             $20
     5 after 5-60 min idle, 19 after over an hour
     Start a new session instead of resuming, or /compact first.

  GOING WELL
  ✓  Cache is healthy — 97% read, 3% rewritten
  ✓  Model switches are rare — 3 of 171 sessions
  ✓  /clear used 12 times — context stays short

  SETUP HEALTH
  !  architecture-guidance failed to connect in 49 of 171 sessions
  !  Stop hook averages 12.8 s per run, 2 timed out
```

Every candidate is priced, sorted by what it saves, and the top five print. A
finding with no signal stays silent. The candidates:

- **Older model where a newer one is cheaper** (Sonnet 4.6 → 5, Opus 5 → 5.5),
  and **subagents on your main model** (priced on Haiku; forks are left out,
  since they need the parent's model to reuse its cache). Both reprice the same
  tokens at list price. A cheaper model can take more turns, so treat them as
  upper bounds.
- **Idle-session cache rewrites.** After a gap past the cache TTL, the next call
  rewrites the whole context at write price.
- **Listed but never used**: MCP servers, skills and agents whose listings ride
  every call, priced by their share of the listing. The PLUGINS AND SKILLS
  section shows the per-plugin listing tax alongside the work each actually
  drove, so you can see which plugins earn their keep.
- **Max effort as a default**, **failed tool calls** (the calls spent reading
  the error and retrying), and **cache misses the provider explains**
  (`model_changed`, `tools_changed`, …) with their fix.

GOING WELL and SETUP HEALTH print only lines over a threshold: healthy cache,
rare model switches and `/clear` use; MCP servers failing to connect and hooks
that fail or run slow.

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
    Bearer Token (iss: https://sso.staging.example.com/) Bearer e…FCCQ (1217c)  you typed/pasted it
    JWT (iss: https://sso.staging.example.com/) eyJhbGci…FCCQ (1210c)  Read user-token

  Action: rotate any non-expired credentials listed above.
  The matched files keep the secret until cleanupPeriodDays
  (default 30). Delete them after rotating; claudit deletes nothing.

  PERSONAL DATA — checksum-valid numbers, counts only
  session  865fcb8e  IBAN 1 (local) · tax id 3 (local)

  RAN WITHOUT ASKING — risky shell commands, no prompt
  rm -rf          19× in auto        sessions 0c13893b, 2783298c +3
```

Scanner output contains partial secrets (first 8 and last 4 characters of each
match). Do not paste it into a chat, ticket, or PR.

Each hit names how it got in (you typed or pasted it, `Read .env`, `Bash cat`,
an MCP result, an edit backup) and whether it went out again in an MCP tool call
or a WebFetch URL. Personal numbers (IBAN, German tax id, card) are counted per
session, never shown. Risky shell commands (`rm -rf`, force push, `curl | sh`,
`sudo`, …) run in a mode that does not prompt are listed by class; the command
line itself is never printed.

Three detection passes run over every file:

1. **Known-format patterns** — regex matches for things with a recognisable
   prefix regardless of context: JWTs, GitHub tokens, Bearer tokens, Stripe,
   Anthropic, OpenAI, AWS, Slack, Google, GitLab, npm and Atlassian keys, private
   key PEM blocks (also inside JSON strings), DB connection URLs.
2. **Shannon entropy** — finds `"key": "value"` pairs where the key name looks
   secret-sounding (`password`, `secret`, `api_key`, `client_secret`, …) and
   the value has high entropy (>4.5 bits/char for base64, >4.8 for general
   text). Catches novel or internal secrets with no recognisable prefix.
3. **Env-style assignments** — `FOO_TOKEN=value` lines in shell output and
   `.env` contents, kept only when the value is high-entropy, mixes letters and
   digits, and holds no placeholder word.

Built-in false-positive suppression: Claude Code embeds its own API key as a
JWT in every trace (`{"jti":"ApiKey:N"}`); these are detected and skipped
automatically, including where they appear as `Bearer eyJ…`. The same applies
to any issuer you pass to `-scanner-ignore-iss`.

### Scripting

```sh
$ claudit -json -days 0 | jq -r '
    sort_by(-.cost_usd) | .[:5][] | "\(.session)  $\(.cost_usd|floor)"'
```

`-json` also carries `models` (model → cost), `unpriced`, `harness_cost_usd` and
`harness_models` (Claude Code's own ledger), `parent_session` and `agent_type` on
subagent runs, `branch` and `prs`.

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
