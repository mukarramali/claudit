# Plan: token-usage-auditor

Go CLI that counts tokens locally from Claude Code transcripts and compares them
against the provider numbers already embedded in those transcripts.

Scope: Claude only. Generic in shape (one neutral `Trajectory` struct, one
`parseClaude` function) so a second agent = a second parse func, no refactor.
No interface until there are two implementations.

## Deliverable

```
main.go        ~250 lines
main_test.go   ~60 lines, fixture-driven
go.mod         1 dep: github.com/pkoukk/tiktoken-go (already in module cache)
```

## CLI

```
claudit [flags] [path...]

  path    .jsonl file, or a ~/.claude/projects/<slug> dir
          default: the project dir matching $PWD
  -json   machine-readable output
  -v      per-inference-call table
```

## Data model

```go
type Trajectory struct {          // agent-neutral
    ID, Model string
    Calls     []Call              // one per inference round
}

type Call struct {
    P, T, A            int        // prompt / tool output / attachment  (new this round)
    MText, MTool, R    int        // model text / tool calls / reasoning
    X                  int        // replay carried in
    Provider           Usage      // from the transcript, zero if absent
}
```

`parseClaude(path) []Trajectory` is the only parser.

## Bucketing (verified against a real 1136-line transcript)

| Bucket          | JSONL source                                                                              |
| --------------- | ----------------------------------------------------------------------------------------- |
| `P` prompt      | `type:user`, `isMeta` unset, content string or `text` blocks                              |
| `T` tool output | `type:user`, `tool_result` blocks (content only — **not** the duplicated `toolUseResult`) |
| `A` attachment  | `type:attachment` + `isMeta:true` user records + `type:system` hook output                |
| `M_text`        | assistant `text` blocks                                                                   |
| `M_tool`        | assistant `tool_use` blocks, serialized name+input JSON                                   |
| `R` reasoning   | assistant `thinking` blocks                                                               |
| `N`             | count of distinct `message.id`                                                            |

Attachments have ~10 different shapes and no common content field. Rule:
`.attachment.content // .attachment.text // <whole object as JSON>`. Covers the
text-bearing kinds exactly, over-counts the delta kinds slightly. `ponytail:`
comment marks it; tighten only if the residual (below) turns out to be
attachment-dominated.

## Replay X

```
carry = 0
for each call n:
    new      = P_n + T_n + A_n
    X_n      = carry
    input_n  = new + X_n
    output_n = MText_n + MTool_n + R_n
    carry   += new + output_n
```

Reset `carry` to 0 on a record with `isCompactSummary:true` (present in real
transcripts) — after a compaction the context is the summary, not the history.

## Output

```
buckets     P, T, A, M_text, M_tool, R, X
formulas    chatting, agent_visible, trajectory_input/output/total   (idea.md 1-4)
provider    input_tokens + cache_creation + cache_read, output_tokens
delta       local/provider ratio for input and output, absolute residual
```

The residual is the deliverable insight: it is the hidden scaffolding (system
prompt, tool schemas) plus tokenizer-family bias, and it is the one number a
provider dashboard cannot show you.

## Edge cases (all verified in a real file, all load-bearing)

1. **Assistant records are split one-per-content-block, sharing `message.id` and
   the identical `usage` object.** 293 records → 159 real inference calls in the
   sample. Summing naively inflates provider totals ~2x. Group by `message.id`;
   take `usage` once. This is the single biggest correctness trap.
2. `toolUseResult` duplicates the `tool_result` block. Count one.
3. `isSidechain:true` records (subagent runs) are separate trajectories — own
   replay chain, tallied separately. None in the local sample; other files will
   have them.
4. Non-message records (`mode`, `last-prompt`, `file-history-snapshot`, …, ~40%
   of lines) are client bookkeeping, never sent to the model. Skip.
5. Records with no `usage` (older/aborted) contribute local counts only; the
   provider column is marked partial rather than silently wrong.

## Tokenizer

`o200k_base` via tiktoken-go — the idea doc's baseline. It is the _wrong family_
for Claude and there is no public Claude BPE, so counts carry a systematic bias.
That is acceptable precisely because the transcript carries ground truth: the
tool prints the local/provider ratio, which makes the bias visible and
correctable instead of hidden. The BPE file downloads once and caches
(`TIKTOKEN_CACHE_DIR`).

## Test

`main_test.go`, one 12-line fixture transcript covering: the split-record dedup,
the replay recurrence, and the compaction reset. `countTokens` is a package-level
`var` so the test swaps in `len(s)/4` and needs no network.

## Skipped

- Cost/pricing table — prices drift, the token counts are the hard part.
- Codex/other agents — the parse func slot exists, fill it when there is a trace.
- Streaming/live monitoring, SQLite history, charts.
