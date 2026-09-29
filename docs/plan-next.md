# Plan: claudit, next

What to build after v1 (`docs/plan.md`), in order. Every item is pinned to a
number measured on one user's `~/.claude` (16 Jun – 27 Sep 2026: 169 session
transcripts, 80 subagent transcripts, $389 at list price). The numbers rank the
work. They are not a benchmark.

Phases ship independently. Phase 0 changes every figure the report prints, so it
goes first. Inside a phase, items are ordered by value per line of code.

## Ground rules (unchanged from v1)

- **Local and read-only.** No network, nothing written. Every new file read goes
  into README's "Everything is local" list in the same change.
- **Names and counts only** in the spend report. Never prompt text, command
  lines, tool inputs, file contents, `/insights` summaries or email addresses.
  The scanner keeps its 8+4 previews for credentials. PII gets counts, no
  preview at all.
- **No new flags.** A lesson prints when it has signal and stays silent
  otherwise, same as today. `-json` grows fields so `jq` covers the long tail.
- **One pass per file.** New fields are read in `apply()` / the `record` struct.
  No second parse.
- **Fewest files.** Keep `main.go` / `insights.go` / `scanner.go`. Split
  lessons into `lessons.go` only if `insights.go` passes ~800 lines.

## Evidence snapshot

| Signal | Measured |
|---|---|
| What claudit reports today (`-days 0`) | $327 |
| What the transcripts hold (list price) | $389 (top-level $349 + subagents $40) |
| Claude Code's own ledger (`cost-state`) vs transcripts, same 147 sessions | $382 vs $352 |
| Headline "idle MCP" lesson | claims $48; the whole MCP listing costs $17 |
| Sessions priced at $0 | 8 sessions, 204 calls |
| Skills listed / ever used | 136 / 15 |
| Subagent spend running on the parent's model | $23.79 of $40 |
| Same tokens: Sonnet 4.6 → Sonnet 5 | $163 → $108 (−33%) |
| Same tokens: Opus 5 → Opus 5.5 | $214 → $123 (−42%) |
| Cache rewritten after an idle gap | 22 times, ~$17.50 |
| Calls reacting to a failed tool call | 253 calls, $10.48 |
| Credential-shaped values the scanner never reports | 1 Anthropic-key, 4 AWS-key, 4 JWT (distinct) |

## Status (2026-09-29)

Built on `feat/v2`, one commit per item or pair, each checked against
`claudit -days 3` / `-days 14` / `-days 0` on this machine before committing.

| Item | State | What the run showed |
|---|---|---|
| 0.1 subagent transcripts | done | +10 runs, +$2 over 3 days; all time 95 runs |
| 0.2 per-call pricing | done | 3-day total $12 → $22: Opus 5.5 and dated Haiku ids had been $0; all time $327 → $409 |
| 0.3 idle MCP, MCP only | done | headline $3.75 → $0.90 (3 days), $15 all time; UUID aliases merge at ≥80% tool overlap (exact sets never match) |
| 0.4 CLAUDE.md once | done | CLAUDE.md (project) 10.4k → 6.3k tokens; opaque "instructions" row split per file |
| 0.5 harness ledger | done | +8% all time; the ledger includes subagent runs (closer, and above them in all but 2 of 155 sessions) |
| 1.1 inherited subagents | done | $31 of $42 on the main model all time |
| 1.2 cheaper successor | done | Opus 5 → 5.5 $91, Sonnet 4.6 → 5 $58 |
| 1.3 concentration | done | printed as a paragraph, not a finding; the depth curve moved into it |
| 1.4 idle cache rewrites | done | 24 rewrites, $20 all time, mostly after >1 h |
| 1.5 max effort | done | Opus 5 +19–26%/call; upper bound, max calls may be the harder tasks |
| 1.6 failed tool calls | **needs review** | the amount is the cost of recovering, not a saving; "Bash 131" names no fix |
| 1.7 going well | done | `/clear` read from `<command-name>` (42) — agrees with `SessionStart:clear` hooks (43) |
| 2.1 listed never used | done | 117 of 136 skills, 15 of 21 agents, $16 |
| 2.2 work driven | **needs review** | section built; the `listing tax` column was skipped (needs a two-value row) |
| 2.3 cache-miss reasons | done, rarely visible | $3.59 all time, ranks 8th here; replaces the guessed lesson when reasons exist |
| 2.4 cost per PR | done | 17 PRs, median $1.43 |
| 2.5 setup health | done | architecture-guidance fails in 49 of 171 sessions; Stop hook 12.8 s avg |
| 2.6 /insights join | done, low value | friction shown only with ≥10 sessions a side: it flipped sign between windows at ~40 |
| 2.7 tokenizer calibration | **needs review — not merged** | fitted ratios are stable (Sonnet 1.52, Haiku 1.37, Opus 1.5–1.7), but scaled weights overshoot provider input by 28–39% (145 of 150 sessions >5% over). The deltas are prose/tool output; most weight is schema JSON and replay. Would hide the 12% unaccounted row rather than explain it |
| 3.1 coverage | done | +2 matches from tool-results/file-history; history.jsonl dated by mtime |
| 3.2 patterns | done | AWS, sk-proj, Slack etc. found where the old rules saw nothing |
| 3.3 PEM in JSONL | done | latent; covered by a test |
| 3.4 provenance | done | source per hit; egress to MCP/WebFetch; one decode per line, only for files with hits |
| 3.5 prevention text | done | deny rule on a `.env` read; retention line always |
| 3.6 personal data | done | card numbers also need an issuer prefix (epoch-ms timestamps pass Luhn 1 in 10); public sample numbers skipped |
| 3.7 ran without asking | done | rm -rf 19× in auto, force push 4× in acceptEdits; a command the auto classifier blocked still counts |

---

## Phase 0 — Correctness

The report is ~22% low today (about $420 real against $327 shown), and its
top recommendation is inflated.

### 0.1 Read subagent transcripts — S

**Why.** Claude Code writes each subagent run to
`<slug>/<session>/subagents/agent-<id>.jsonl` next to an `agent-<id>.meta.json`.
`transcripts()` only globs `<dir>/*.jsonl`, so those 80 files ($40, 979 calls)
never load, and the scanner never sees them either. The inline `isSidechain`
split in `parseClaude` never triggers: current transcripts contain 0 inline
sidechain records.

**Change.**
- `transcripts()` (main.go:726): also glob `<dir>/*/subagents/*.jsonl`.
- `main()` (main.go:835): for a file under `subagents/`, the session is the
  grandparent directory's name and the project is the directory above that.
  Today these come out as `agent-…` and `subagents`. The same derivation feeds
  the scanner's `traceFile`.
- Read `agentType` from the `.meta.json` next to each file. Ignore
  `description`, since that is prompt text.
- `Trajectory`: add `Parent` (session id) and `Agent` (agentType). ID becomes
  `<session> · <agentType>`.
- Keep the inline `isSidechain` split for old transcripts. Drop README's
  "subagents lumped into one trajectory" known limit, because separate files
  already separate concurrent subagents.

**Check.** A project dir holding one session plus one subagent file yields two
trajectories. The subagent one carries the parent's session id and project, not
`subagents`.

### 0.2 Price every call at its own model — S

**Why.** Three bugs, one cause:
- `prices[model]` is an exact match, so `claude-haiku-4-5-20251001` misses.
- `claude-opus-5-5` is not in the table at all.
- `sum()` prices a whole session at `t.Model`, which is the *last* model seen
  (main.go:489). A trailing `<synthetic>` error record therefore zeroes the
  session (3 sessions, 143 calls), and a mid-session `/model` switch prices
  everything at the final model.

Opus 5.5 also breaks the flat cache-read multiplier: its reads are $0.20
(0.05×), not 0.1×.

**Change.**
- `prices`: `{in, out, readMult}` per model, which folds in the Fable/Mythos 5.1
  special case (main.go:578). Add `claude-opus-5-5: {4, 20, 0.05}`. Verified
  rates (claude-api skill, 2026-09-27): Fable 5.1 $10/$50 reads 0.025×; Opus 5.5
  $4/$20 reads 0.05×; Opus 5 $5/$25; Sonnet 5 $2/$10; Sonnet 4.6 $3/$15;
  Haiku 4.5 $1/$5. Cache writes are 1.25× (5m) and 2× (1h).
- Model lookup: strip a `[…]` suffix (the ledger writes `claude-opus-5[1m]`),
  then take the **longest** table key that is a prefix. Without longest-first,
  `claude-opus-5` would swallow `claude-opus-5-5`.
- `Call.Model` is set per assistant record. `sum()` adds up per-call bills.
  `totals.Model` becomes the model with the most spend, used for display and
  JSON. `depthCurve` (insights.go:334) prices with `c.Model`.
- Never assign `<synthetic>` to `t.Model`.
- Stop dropping unknown models silently. Add one footnote:
  `N calls on unpriced models (x, y) — not in the total`.

**Check.** A fixture that switches Opus → Sonnet mid-session and ends with a
`<synthetic>` record prices each call at its own rate and totals non-zero. A
dated Haiku id prices at Haiku rates.

### 0.3 Idle-MCP cost: MCP only, one name per connector — S

**Why.** `whatToChange` builds `listingCost` from every group whose name ends
in `tool schemas` (insights.go:363). That includes **built-in** tool schemas
($65), so the "$48 idle MCP" headline is about 5× the whole MCP listing ($17).
The listing also misses per-server instruction text (`mcp_instructions_delta`).
Separately, the same claude.ai connector appears under two names across
sessions: `claude_ai_Notion`, and `7dedc1d6-…` with an identical tool set. That
splits listed/used, so one alias can show as idle while the other is used.

**Change.**
- `listingCost` = `tool listings (MCP)` + `MCP server instructions` +
  `MCP <srv> tool schemas`. Never `built-in tool schemas`.
- `noteListing`: also count `mcp_instructions_delta` (`addedNames[i]` ↔
  `addedBlocks[i]`) into the server's weight.
- Normalise names (`claude.ai Notion` → `claude_ai_Notion`). Merge a UUID-named
  server into the named server with the same tool-name set. If there's no
  match, label it `connector 7dedc1d6 (notion-search, …)` using the first tool
  names as a hint.

**Check.** A trajectory with built-in schemas and one idle MCP server prices
the idle server from the MCP listing only. Two aliases with the same tools count
as one server.

### 0.4 CLAUDE.md is counted twice — S (deletion)

**Why.** `instructions` attachments already carry every loaded memory file with
its content (400 entries: `User` 183, `Project` 86, `AutoMem` 131).
`attribute()` then adds the global and project CLAUDE.md again from disk
(insights.go:77). The disk read is also *today's* file, not the one each session
loaded, and it misses nested files.

**Change.**
- Delete `claudeMd`, `memoryFiles` and the append in `attribute()`.
- In `apply()`, split an `instructions` attachment into one item per file:
  `CLAUDE.md (user)`, `CLAUDE.md (project)`, `memory (auto)`. Label
  `nested_memory` as `CLAUDE.md (nested)`.
- README: claudit no longer reads CLAUDE.md from disk.

**Check.** A fixture with an `instructions` attachment yields one item per file
type, and no item is read from disk.

### 0.5 Reconcile with Claude Code's own ledger — S

**Why.** `type:"cost-state"` records carry the harness's own `totalCostUSD` and
per-model `modelUsage` (`inputTokens`, `outputTokens`, cache fields, `costUSD`).
Each session has one `startTime` and a monotonic total, so the last record wins.
Same 147 sessions: ledger $382, transcripts $352. The gap is tokens, not price:
Haiku output is 52.7k in the ledger vs 5.0k in the transcripts. Those are
background calls with no transcript record.

**Change.**
- Parse the last `cost-state` per session.
- Header line: `Claude Code's own ledger: $382 · $30 (8%) is calls the
  transcripts don't record`.
- `-json`: `harness_cost_usd`, plus a per-model gap.
- Replay attribution keeps using transcript usage; it needs per-call data. Don't
  name an env var that "turns background calls off": the docs don't pin one
  down (see Skipped).

**Check.** A fixture with two `cost-state` records reports the later total.

---

## Phase 1 — Spending behaviour

This phase turns `WHAT TO CHANGE` from three fixed lessons into ranked
candidates. `whatToChange` collects `{headline, $, detail}`, sorts by $, and
prints the top five. Then up to three "good" lines use the existing `good`
marker. All counterfactuals reprice the **same tokens at list price**, and each
says so in one line. Tokenizers differ by up to ~1.35× and a cheaper model can
take more turns, so they are upper bounds.

Depends on 0.1 (subagents) and 0.2 (per-call pricing, reused for repricing).

### 1.1 Subagents inherit the parent's model — S

**Why.** Documented precedence: per-call `model` > agent frontmatter `model:` >
`CLAUDE_CODE_SUBAGENT_MODEL` > main model. So by default a child runs on
whatever the parent runs on.
- General-purpose subagents on the parent's model cost $23.79 of $40 subagent
  spend, 213 of those calls on Opus 5.
- Explore made 315 calls, all Sonnet 4.6.
- The same subagent tokens on Haiku 4.5 would list at $11.

**Change.**
- Per agent type: spend, and share on the parent's model (join `Parent` → the
  parent trajectory's main model).
- Reprice on `claude-haiku-4-5` and `claude-sonnet-5`.
- **Exclude forks.** They inherit the parent context and must stay on its model
  to reuse its cache.
- Text: `Subagents ran on your main model: $24 of $40 · on Haiku ~$11 ·
  set model: in the agent, or CLAUDE_CODE_SUBAGENT_MODEL`.

**Check.** A parent on Opus and a child on Opus report 100% inherited. A fork
child is excluded.

### 1.2 An older model where a newer one is cheaper — S

**Why.** 70% of calls ran on Sonnet 4.6 ($3/$15) while Sonnet 5 lists at $2/$10:
$163 → $108 on the same tokens. On this machine the cause is managed settings
(`ANTHROPIC_DEFAULT_SONNET_MODEL=claude-sonnet-4-6`, with `claude-sonnet-5` in
`availableModels`). Opus 5 → Opus 5.5 is $214 → $123, mostly cache reads
($0.50 → $0.20).

**Change.**
- A static successor table, listing only pairs where the newer model is cheaper
  at list price: `sonnet-4-6 → sonnet-5`, `opus-5 → opus-5-5`,
  `opus-4-{6,7,8} → opus-5-5`.
- Reprice the older model's calls.
- Infer everything from the transcripts, without reading settings files. The
  text carries the hint instead: `if pinned (ANTHROPIC_DEFAULT_SONNET_MODEL),
  ask whoever manages it`.

**Check.** Sonnet 4.6 calls reprice at Sonnet 5 rates. A model with no cheaper
successor produces no line.

### 1.3 Where the money concentrates — S (extends the depth lesson)

**Why.** 12 sessions of 51–200 calls on Opus 5 cost $159, over 40% of spend.
Quick Opus sessions (≤10 calls) total $4.45. The expensive-model problem is long
sessions, not quick questions, and the report should say which it is.

**Change.**
- Next to the existing depth curve, one line: the share of spend in the top 10%
  of sessions, with their size bucket and model.
- If short sessions on the top-tier model cost under ~2% of spend, say so
  explicitly, so nobody optimises the wrong thing.

**Check.** A skewed fixture reports the top-decile share.

### 1.4 Resuming an idle session rewrites its cache — S

**Why.** After a gap longer than the cache TTL, the next call re-sends the
whole context at write price (1.25–2×) instead of read price (≤0.1×). That
happened 22 times: 2.3M tokens, ~$17.50 above re-reading. 17 of the 22 gaps
were over an hour, so a longer TTL wouldn't have helped.

**Change.**
- `Call.At` is taken from the record timestamp.
- Flag a call when the gap is over 5 min and `cache_creation` is over 20k. The
  premium is `write5m×(1.25−read)×in + write1h×(2−read)×in`.
- Split the gaps into 5–60 min and over 1 h. Advice depends on which dominates:
  - Mostly 5–60 min: `CLAUDE_CODE_PROMPT_CACHE_TTL=1h` (documented; the main
    conversation already defaults to 1h on subscriptions and 5m on API and
    usage credits).
  - Mostly over an hour: `/clear` or a new session instead of resuming, or
    `/compact` first.

**Check.** Two calls 2 h apart with a large cache write produce one rebuild
priced at the write premium. Calls 30 s apart produce none.

### 1.5 Max effort as a habit — S

**Why.** Opus 5 at `max`: 534 calls at $0.146 each, against $0.122 at `high`
(+20%).

**Change.**
- Read the `effort` field on assistant records.
- Report `$`/call per (model, effort). Emit a line only when `max` is more than
  15% above `high` on the same model with at least 50 calls each. Text: use
  `/effort max` for the task, not as a default.

**Check.** Fixture calls at two effort levels on one model produce the ratio.
With under 50 calls there is no line.

### 1.6 Failed tool calls — S

**Why.** 267 tool results had `is_error`. The calls that reacted to them cost
$10.48. By tool: Bash 131, flagdexter 44, Edit 20, Read 17.

**Change.**
- Mark the call that follows an `is_error` result, and sum those calls.
- Group by tool label, reusing `toolLabel`.
- One line with the top two tools.

**Check.** An error result followed by a call attributes that call's cost to
the erroring tool.

### 1.7 What's going well — S

**Why.** Generic praise is noise, but real "good" numbers make the bad ones
credible. Measured here:
- Cache was 98% read, 2% rewritten.
- Only 3 of 163 sessions switched model mid-way. Each switch is a 43–55k-token
  miss.
- 108 fork calls stayed on the parent model.
- 65 general-purpose calls were delegated to Haiku.
- `/clear` was used 42 times.

**Change.**
- Keep the existing cache line.
- Add "model switches are rare" (from `model_changed`, see 2.3, or model
  changes within a trajectory).
- Add "subagents mostly on cheaper models" when the inherited share is under
  30%.
- Detect `/clear` from `local_command` records or `SessionStart:clear` hook
  events. Confirm which one is reliable before relying on it.
- At most three good lines.

**Check.** Each good line appears only when its threshold holds.

---

## Phase 2 — New views from harness data

### 2.1 Listed but never used: skills, agents, plugins — S

**Why.** 136 skills listed, 15 ever used. 21 agents listed, 5 used. By plugin
(listed/used): dev-toolkit 18/0, figma 14/0, anthropic-skills 11/0,
plugin-dev 8/0, architecture-guidance 4/0. The skill listing costs $8.87 and the
agent listing $5.83. This is the same shape as the MCP lesson and the same fix
class.

**Change.**
- Listed: `skill_listing.names[]` plus its `content`, which has one
  `- plugin:skill: description` line per skill, so per-skill tokens are
  measurable. Agents come from `agent_listing_delta.addedTypes[]` and
  `addedLines[]`.
- Used: `Skill` tool input `skill`, `<command-name>` in user records, and
  `Agent` tool input `subagent_type`.
- Price by share of the listing group, reusing the `idleMCP` machinery.
- Roll up per plugin (name prefix before `:`, and `plugin_<name>_…` MCP
  servers) so one line says what disabling a plugin saves.
- Text: `/plugin disable <name>`. For plugin authors:
  `disable-model-invocation: true` keeps a skill out of the listing until it is
  invoked.

**Check.** Three listed skills with one invoked report two idle, priced by
their line share.

### 2.2 Cost by plugin, skill and agent — S

**Why.** Every assistant record carries `attributionPlugin`,
`attributionSkill`, `attributionAgent` and `attributionMcpServer`: Claude Code's
own answer to "who drove this call". testing-principles drove $50.72 (13% of
spend); the visual-diff skill $8.95. That's the *work* a plugin caused.
Replay-weighted attribution measures its *context tax*. The pair is a
per-plugin ROI.

**Change.**
- Four string fields on `record`.
- Per-call cost summed by attribution.
- A two-column section, `listing tax | work driven`, top 8 plugins.

**Check.** Attributed fixture calls sum per plugin. Unattributed calls land in
`(none)`.

### 2.3 Real cache-miss reasons — S

**Why.** Lesson 3 guesses ("a hook that emits a timestamp").
`message.diagnostics.cache_miss_reason` says why:
- `tools_changed` ×5 (10.9–79.6k tokens)
- `model_changed` ×3 (43–55k)
- `messages_changed` ×6
- `system_changed` ×1
- `previous_message_not_found` ×18

Each reason comes with `cache_missed_input_tokens`.

**Change.**
- Parse `diagnostics`.
- Price each miss at `missed × (write − read) × in`.
- One detail line per top reason, with its fix:
  - `model_changed`: start a new session rather than switching `/model`.
  - `tools_changed`: an MCP server connected late or flapped (see 2.5).
  - `system_changed`: CLAUDE.md or a hook changed mid-session.
- Older transcripts have no diagnostics. Keep today's share-based lesson as the
  fallback.

**Check.** A fixture with a `model_changed` miss prices it and names the
reason.

### 2.4 Cost per PR and per branch — S

**Why.** `pr-link` records (`prRepository`, `prNumber`) link 16 PRs: median
$1.43, max $26.46. `gitBranch` is on every record (47 branches). That's unit
economics a team lead can use.

**Change.**
- Collect PR links per session. Split a session's cost equally across its PRs
  (`ponytail:` equal split; weight by call timing if anyone asks).
- Report one line: `16 PRs · median $1.43 · priciest repo#n $26.46`.
- Branches go in `-json` only, since a branch name can say more than a PR
  number (see open questions).

**Check.** A session linked to two PRs splits its cost evenly.

### 2.5 Harness health — S

**Why.** These are setup defects that cost time rather than tokens, and nothing
else surfaces them:
- `deferred_tools_delta.failedMcpServers`: architecture-guidance failed to
  connect in 58 of 165 sessions. flagdexter failed 19 times and PagerDuty 5;
  figma needed auth 9 times.
- Hooks: Stop averages 12.8 s per run, with 2 timeouts. UserPromptSubmit failed
  14 of 152 runs.
- Tool error rates, from 1.6.

**Change.** A `SETUP HEALTH` block that prints only lines over a threshold:
- a server failing in more than 10% of sessions
- a hook failing more than 5% of the time, or averaging over 5 s
- sources: `hook_success`, `hook_non_blocking_error`, `hook_cancelled`
  (`durationMs`, `timedOut`, `exitCode`)

**Check.** A server failing in 2 of 3 sessions prints. One failing in 1 of 20
doesn't.

### 2.6 Join with `/insights` — S, low priority

**Why.** `~/.claude/usage-data/facets/<session>.json` holds `session_type`,
`outcome` and `friction_counts` for the sessions `/insights` analysed (50
here). A quick question costs $0.23 per session, a multi-task session $3.25, and
sessions with friction cost +19%.

**Change.**
- Read the enum fields only. Never `brief_summary`, `underlying_goal` or
  `friction_detail`.
- One line per session type with `$`/session, and state the coverage.
- Silent when the directory is absent.

**Check.** A facet file joins to its session's cost. Missing facets change
nothing.

### 2.7 Calibrate the tokenizer against the provider — M, last

**Why.** `unaccounted` is 12% of spend, and much of it is o200k being the
wrong tokenizer. The difference in provider input between consecutive calls
(no compaction in between) is the exact Claude-token size of what entered in
between: the previous output plus the new input.

**Change.**
- Fit o200k counts to those deltas per model family.
- Skip pairs where the earlier call had thinking, because the provider strips
  it from context.
- Scale item tokens by the fitted ratio, so the tokens column becomes Claude
  tokens.

**Check.** On a fixture with known provider deltas, calibrated weights land
within a few percent of provider input.

---

## Phase 3 — Scanner

### 3.1 Coverage — S

**Why.** Places the scanner never looks, with counts only:
- `subagents/` (0.1): 1 Anthropic-key-shaped value, 1 AWS-key-shaped value,
  2 JWTs.
- `~/.claude/file-history/<session>/` (edit backups, 542 files): 2 JWTs.
- `<session>/tool-results/` (19 files, 4.9 MB of large tool outputs): nothing
  yet.

None of these values appear in the files scanned today.
`~/.claude/history.jsonl` (typed prompts) and `paste-cache/` scanned clean, but
a pasted secret would land exactly there.

**Change.**
- Add those five locations to `-scanner`, under the same `-days` mtime rule.
- Attribute file-history and tool-results to their session through the
  directory name.

**Check.** A secret placed in a subagent file and in a file-history backup is
reported under the parent session.

### 3.2 Patterns — S

**Why.** There is no AWS rule: 3 distinct AWS-key shapes sit in files the
scanner already reads. The OpenAI rule `sk-[A-Za-z0-9]{48,}` can't match
`sk-proj-…` keys, because they contain `-`. The entropy pass only reads JSON
`"key": "value"` pairs, so `FOO_TOKEN=…` lines in Bash output and `.env`
contents slip through.

**Change.** Add:

| Label | Regex |
|---|---|
| AWS access key | `\b(?:AKIA\|ASIA)[0-9A-Z]{16}\b` |
| OpenAI project key | `sk-proj-[A-Za-z0-9_-]{40,}` |
| GitHub fine-grained | `github_pat_[A-Za-z0-9_]{60,}` |
| Slack token | `xox[abposr]-[A-Za-z0-9-]{10,}` |
| Google API key | `AIza[0-9A-Za-z_-]{35}` |
| GitLab PAT | `glpat-[A-Za-z0-9_-]{20,}` |
| npm token | `\bnpm_[A-Za-z0-9]{36}\b` |
| Atlassian API token | `ATATT[A-Za-z0-9_=-]{50,}` |
| env-style assignment | `\b[A-Z0-9_]*(?:SECRET\|TOKEN\|PASSWORD\|PASSWD\|API_?KEY\|ACCESS_?KEY\|PRIVATE_?KEY)[A-Z0-9_]*\s*=\s*['"]?([A-Za-z0-9+/_=-]{20,})` |

The env-style value must also pass `isHighEntropy`, contain both a digit and a
letter, and not look like a placeholder (`EXAMPLE`, `xxxx`, `your`, `dummy`,
`test`, `…`). Without those gates the raw regex had 389 hits. With them it had
1 distinct value.

Fixtures assemble keys from parts, as `scanner_test.go` already does, so push
protection and the scanner itself stay quiet.

**Check.** One assembled value per new label is found. Placeholders and
low-entropy values are not.

### 3.3 PEM keys inside JSONL — S

**Why.** Inside a JSON string a newline is stored as the two characters `\n`.
The Private Key regex (scanner.go:97) requires a real newline, so it can never
match a transcript. It's latent: there are no PEMs in this data.

**Change.** Accept `(?:\\n|\n)` wherever the regex expects a newline.

**Check.** An assembled PEM block embedded in a JSON string is found.

### 3.4 Provenance and egress — M

**Why.** "Rotate it" is the only action today. Knowing how a secret got in,
and whether it went out again, is what makes it preventable.

**Change.**
- Scan per record instead of per file (a JSON unmarshal, no tokenising), so
  each hit knows its source:
  - `you typed/pasted it`
  - `Read <ext>` (e.g. `.env`)
  - `Bash <first word>`
  - `MCP <server> result`
  - `attachment`
- Also check whether it was *sent out*: inside a `tool_use` input to an MCP
  server, or in a `WebFetch` URL. Report that as `sent to MCP <server>`.
- Keep the dedup key per session.

**Check.** A secret appearing first in a Read result and then in an MCP tool
input reports both its source and its destination.

### 3.5 Prevention and retention in the action line — S

**Why.** Documented facts:
- A `permissions.deny` rule like `Read(**/.env)` also blocks `cat`, `head` and
  `tail` on that path.
- Transcripts, `tool-results/` and `file-history/` persist until
  `cleanupPeriodDays` (default 30).

**Change.**
- When provenance (3.4) shows a `.env`-style Read, print the deny rule.
  `Read(**/.env.*)` for suffixed files still needs checking against the
  permissions docs.
- Always add: `the matched files keep the secret until cleanupPeriodDays
  (default 30) — delete them after rotating`.
- claudit still deletes nothing itself.

**Check.** A `.env` Read hit prints the deny rule. Other sources don't.

### 3.6 Personal data, counts only — M

**Why.** For a tax product the sensitive values are IBANs, tax IDs and card
numbers, not only API keys. 3 checksum-valid IBANs sat in local tool results;
they may be fixtures. The 161 email addresses found came mostly from Slack and
Jira connector results. They are colleague addresses from sanctioned tools, so
emails are out of scope.

**Change.**
- IBAN: ISO 13616 mod-97.
- German Steuer-ID: 11 digits, ISO 7064 MOD 11,10 check digit, and the
  digit-repetition rule.
- Card numbers: 13–19 digits plus Luhn.
- A `PERSONAL DATA` section in `-scanner`: counts per session, by kind and by
  source (local tool / MCP server). No preview, not even redacted.

**Check.** Checksum-valid and invalid numbers are told apart for each kind, and
no digits reach the output.

### 3.7 What ran without asking — S

**Why.** 2,863 of 6,770 tool calls (42%) ran in `auto` mode. Risky Bash that
ran without a prompt:
- `rm -rf` 16× in auto
- force-push 1× in auto and 4× in acceptEdits
- `sudo` 1× in auto
- `curl | sh` 1× in acceptEdits

Denials recorded: 34 user-rejected, 3 automode-blocked, 1 permission-rule.

**Change.**
- Track the permission mode per record (`permission-mode` records and the
  `permissionMode` field).
- Classify Bash commands with a short list of risky classes:
  `rm -rf`, `git push --force`, `reset --hard`, `curl|sh`, `sudo`,
  `chmod 777`, `DROP TABLE`.
- Print class, session and mode only. The command line itself can carry
  secrets or paths, so it never reaches the output.

**Check.** A `rm -rf` in auto mode is listed under its class. The same command
in default mode after an approval is not listed as unsupervised.

---

## Output sketch

```
  YOUR CLAUDE CODE SPEND
  16 Jun → 27 Sep 2026 · all projects
  169 sessions · 80 subagent runs · 6,191 model calls · $389
  Claude Code's own ledger: $419 · $30 (8%) is calls the transcripts don't record

  … sections as today, plus:

  PLUGINS                          listing tax   work driven
  testing-principles                     $2.10        $51
  dev-toolkit (18 skills, 0 used)        $1.90          -

  WHAT TO CHANGE
  1  Sonnet 4.6 where Sonnet 5 is cheaper             $54
  2  Subagents ran on your main model                 $29
  3  Resumed idle sessions rewrote their cache        $18
  4  121 of 136 skills were never used                 $8
  5  Failed tool calls, and the calls reacting        $10

  GOING WELL
  ✓  Cache 98% read · model switches rare (3 of 163 sessions)

  SETUP HEALTH
  architecture-guidance failed to connect in 58 of 165 sessions
  UserPromptSubmit hook failed 14 of 152 runs
```

`WHAT TO CHANGE` amounts are the measured savings from the sections above. The
ledger total ($389 + $30) and the plugin rows are illustrative, not measured.

## JSON changes

Existing fields keep their meaning. `cost_usd` becomes the sum of per-call
prices. Added: `models` (model → cost), `harness_cost_usd`, `parent_session`,
`agent_type`, `branch`, `prs`.

## Tests

The testing-principles classifier (2026-09-28) put both kinds of check in this
plan on the **unit** layer. Write them with the `unit-testing` skill.

- **Text in, numbers out** (parsing, pricing, lessons, scanner patterns, PII
  checksums): feed fixture strings to the functions, as `main_test.go` and
  `scanner_test.go` already do. Extend the existing `fixture` wherever the new
  record fits. Assemble secrets and PII from parts.
- **Finding files** (0.1 subagents, 3.1 scanner locations): mock the
  filesystem, don't create a real one. `transcripts()` and the scanner's file
  discovery take an `fs.FS`, and `filepath.Glob` becomes `fs.Glob`. Tests use
  `testing/fstest.MapFS` (stdlib). `main()` passes `os.DirFS(dir)`.
- **One check per item**, the **Check.** line under each. No suites, no
  fixture directories.
- **No integration layer.** There is no network or database, and rates are an
  embedded table. The classifier's one caveat, a live pricing fetch, is ruled
  out by the no-network rule.

## Skipped — checked, not worth building

- **Repeated identical tool calls.** 60 of 6,756, ~3.5k tokens.
- **Web search/fetch billing.** 0 requests. Price `server_tool_use` when it
  appears.
- **Fast mode.** 0 calls (`usage.speed` is always `standard`). When it appears,
  price it at 2× (Opus 5 $10/$50, Opus 5.5 $8/$40).
- **Long-context premium.** None listed for current models.
- **Compaction analysis.** 4 events, all auto.
- **Bash output by command.** $25, spread over 4,000+ calls, with no single fix.
- **A general "data sent to MCP" map.** All servers seen are sanctioned
  connectors. 3.4 covers the case that matters: a secret going out.
- **Prompt-injection heuristics.** Too noisy to act on.
- **A switch for background calls.** The docs don't pin down what
  `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC` covers. Report the ledger gap
  (0.5) and don't prescribe a toggle.

## Open questions

1. New read-only inputs: `subagents/*.meta.json`, `usage-data/facets`,
   `file-history`, `tool-results`, `history.jsonl`, `paste-cache`. Is it fine to
   extend "Everything is local" to cover them? All are under `~/.claude` and
   none leaves the machine.
2. Counterfactual savings: print `$` with the one-line caveat, or `%` only?
   Plan assumes `$`.
3. Branch names in the shareable report, or `-json` only? Plan assumes
   `-json`.
4. The Sonnet 4.6 pin is an org setting. Keep the lesson phrased for the
   individual, with the "ask whoever manages it" hint (plan), or point at the
   org directly?
