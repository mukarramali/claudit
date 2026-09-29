# Plan: claudit, next

What is still open after `feat/v2`. Everything else from the previous version of
this plan shipped; `git log 848c453..feat/v2` has it, one commit per item.
Numbers come from one user's `~/.claude` (16 Jun – 29 Sep 2026).

## Ground rules (unchanged)

- **Local and read-only.** No network, nothing written. Every new file read goes
  into README's "Everything is local" list in the same change.
- **Names and counts only** in the spend report. The scanner keeps its 8+4
  previews for credentials; personal data gets counts, no preview.
- **No new flags.** A lesson prints when it has signal and stays silent
  otherwise. `-json` grows fields for the long tail.
- **One pass per file.** New fields are read in `apply()` / `record`.
- **Fewest files.** Split lessons into `lessons.go` only if `insights.go` passes
  ~800 lines. It is past that now (~1,300); decide whether to split.

---

## Needs review

### Tokenizer calibration (was 2.7) — not merged

**Why.** `unaccounted` is 11–13% of spend. Local counts use o200k, not Claude's
tokenizer.

**What was tried.** Two attempts, both measured against provider input on
sessions with a `prompt_snapshot`, and both reverted:

| Variant | weight / provider | sessions >5% over | >5% under |
|---|---|---|---|
| uncalibrated | 0.90 | 132 of 245 | 64 |
| one ratio per family (Sonnet 1.52, Haiku 1.37, Opus 1.5–1.7) | 1.42 | 240 | 4 |
| message ratio + separate scaffold ratio | 1.19 | 225 | 13 |
| same, plus no replay of stripped reasoning | 1.05 | 195 | 20 |

The last row looks close in aggregate only because errors cancel. Per session it
is worse than doing nothing.

**What the probe found.** Two local errors offset the tokenizer's undercount:
- The first call's scaffold (system prompt + tool schemas) counts 20–25% *above*
  what the provider bills (ratio 0.75 Haiku, 0.80 Sonnet). Likely cause: the
  snapshot lists deferred tools that are never sent. Unverified.
- Replay carries reasoning forward that the provider strips: about 9% of weight.

**Next.** Fix those two first: confirm which snapshot tools are actually sent,
and stop replaying stale reasoning in `replay()`. Then refit the ratio and
re-run the same per-session check. Merge only if per-session error drops below
the uncalibrated baseline.

### Failed tool calls (was 1.6) — shipped, value unclear

The finding prices the calls that reacted to an `is_error` result (292 calls,
$11 all time). That is the cost of recovering, not a saving: a retry is usually
needed. "Bash 131" names no fix. Either find an actionable split (which
commands fail, repeated identical failures) or drop the finding.

### Cache-miss reasons (was 2.3) — shipped, rarely visible

Works ($3.59 all time) but ranks 8th, below the top-five cut, on this data. It
matters when the cache is unhealthy, where it replaces the guessed lesson. Keep
as is, or give it a slot whenever the cache share crosses the "check" threshold.

### `/insights` join (was 2.6) — shipped, low value

$/session by session type prints when ≥3 sessions have facets. The friction
line is gated to ≥10 sessions a side because it flipped sign between windows
(+16% all time, −7% over 14 days). Decide whether the line earns its place.

---

## Known gaps

- **Auto-mode blocks count as runs.** `RAN WITHOUT ASKING` counts a risky
  command the auto-mode classifier blocked. Telling blocked from run needs the
  `tool_result`'s denial text.
- **`history.jsonl` is one scanner entry.** Its records carry a `sessionId` per
  line, but the scanner works per file, so hits land under "prompt history".
- **UUID connector names in the scanner.** Provenance prints `MCP 7dedc1d6-…
  result`; the insights alias resolution is not applied there.
- **Scan date range.** The header range comes from each file's first record, so
  old transcripts written recently stretch it back to June.

---

## Open questions

1. Counterfactual savings: print `$` with the one-line caveat, or `%` only?
   Built as `$`.
2. Branch names in the shareable report, or `-json` only? Built as `-json` only.
3. The Sonnet 4.6 pin is an org setting. Keep the lesson phrased for the
   individual ("ask whoever sets it"), or point at the org directly? Built as
   the former.
