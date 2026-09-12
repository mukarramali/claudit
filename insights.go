package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Personal insights: which parts of *your* setup — hooks, skills, MCP servers,
// CLAUDE.md, the tools you reach for — actually cost you money.
//
// Attribution works on replay, not raw size. A token that enters the context
// early is re-billed on every later call, so an item's real cost is its size
// times the number of inference calls that carried it. Those weights sum
// exactly to trajectory_input, so the provider's actual dollars can be split
// across them: share of replay = share of the bill.

const scaffoldKind = "scaffold"

// topN is how many rows a section shows before rolling the tail into one line.
const topN = 8

type group struct {
	Kind, Name string
	Tok, N     int
	Cost       float64
}

// claudeMd tokenizes a CLAUDE.md once per path. It never appears in the
// transcript — it is folded into the system prompt — so it is estimated from
// disk and carved out of the unaccounted remainder.
var claudeMd = func() func(string) int {
	var mu sync.Mutex
	seen := map[string]int{}
	return func(path string) int {
		mu.Lock()
		defer mu.Unlock()
		if n, ok := seen[path]; ok {
			return n
		}
		n := 0
		if b, err := os.ReadFile(path); err == nil {
			n = countTokens(string(b))
		}
		seen[path] = n
		return n
	}
}()

func memoryFiles(cwd string) []string {
	var out []string
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out, filepath.Join(home, ".claude", "CLAUDE.md"))
	}
	if cwd != "" {
		out = append(out, filepath.Join(cwd, "CLAUDE.md"))
	}
	return out
}

// attribute splits one session's real cost across everything that occupied its
// context, and adds what the transcript cannot account for as its own entry.
func attribute(t Trajectory, into map[string]*group) {
	o := sum(t)
	b := priceOf(o.Model, o)
	inCost := b.FreshIn + b.CacheWrite + b.CacheRead
	outRate := 0.0
	if p, ok := prices[o.Model]; ok {
		outRate = p[1] / 1e6
	}

	items := t.Items
	for _, f := range memoryFiles(t.CWD) {
		if n := claudeMd(f); n > 0 {
			name := "CLAUDE.md (project)"
			if strings.Contains(f, "/.claude/") {
				name = "CLAUDE.md (global)"
			}
			items = append(items, item{Kind: setup, Name: name, Tok: n, Call: 0})
		}
	}

	if sc := t.Scaffold; sc != nil {
		items = append(items, item{Kind: scaffoldKind, Name: "system prompt (Claude Code)", Tok: sc.System})
		for label, n := range sc.Tools {
			items = append(items, item{Kind: scaffoldKind, Name: label + " tool schemas", Tok: n})
		}
	}

	weight := 0
	for _, it := range items {
		weight += it.Tok * t.rounds(it)
	}
	total := max(weight, o.ProviderInputTotal)

	put := func(kind, name string, tok, n int, cost float64) {
		k := kind + "\x00" + name
		g, ok := into[k]
		if !ok {
			g = &group{Kind: kind, Name: name}
			into[k] = g
		}
		g.Tok += tok
		g.N += n
		g.Cost += cost
	}

	for _, it := range items {
		cost := 0.0
		if total > 0 {
			cost = float64(it.Tok*t.rounds(it)) / float64(total) * inCost
		}
		if it.Out {
			cost += float64(it.Tok) * outRate // generating it, on top of replaying it
		}
		put(it.Kind, it.Name, it.Tok, 1, cost)
	}
	if gap := total - weight; gap > 0 && total > 0 {
		name := "system prompt + tools (no snapshot)"
		if t.Scaffold != nil {
			name = "unaccounted"
		}
		put(scaffoldKind, name, gap/max(len(t.Calls), 1), len(t.Calls),
			float64(gap)/float64(total)*inCost)
	}
}

func row(name string, tok int, cost, grand float64) {
	toks := comma(tok)
	if tok == 0 {
		toks = "" // subtotals have no meaningful token sum
	}
	if n := []rune(name); len(n) > 38 {
		name = string(n[:37]) + "…"
	}
	share := "   - "
	if grand > 0 {
		share = fmt.Sprintf("%4.1f%%", 100*cost/grand)
	}
	fmt.Printf("  %-38s %12s %9s %6s\n", name, toks, money(cost), share)
}

func section(title, note string, gs []*group, grand float64) {
	if len(gs) == 0 {
		return
	}
	sort.Slice(gs, func(i, j int) bool { return gs[i].Cost > gs[j].Cost })
	sub := 0.0
	for _, g := range gs {
		sub += g.Cost
	}
	fmt.Printf("\n  %-38s %12s %9s %6s\n", title, "tokens", "cost", "share")
	if note != "" {
		fmt.Printf("  %s\n", note)
	}
	shown := gs
	if len(gs) > topN {
		shown = gs[:topN]
	}
	for _, g := range shown {
		row(g.Name, g.Tok, g.Cost, grand)
	}
	if rest := gs[len(shown):]; len(rest) > 0 {
		tok, cost := 0, 0.0
		for _, g := range rest {
			tok += g.Tok
			cost += g.Cost
		}
		row(fmt.Sprintf("…and %d more", len(rest)), tok, cost, grand)
	}
	row("subtotal", 0, sub, grand)
}

func insights(all []Trajectory, since time.Time, days int) {
	if days <= 0 {
		since = time.Time{} // -days 0 means everything
	}
	into := map[string]*group{}
	calls, sessions, grand := 0, 0, 0.0
	oldest, newest := time.Time{}, time.Time{}
	for _, t := range all {
		if !t.Start.IsZero() && t.Start.Before(since) {
			continue
		}
		attribute(t, into)
		calls += len(t.Calls)
		sessions++
		grand += sum(t).CostUSD
		if !t.Start.IsZero() {
			if oldest.IsZero() || t.Start.Before(oldest) {
				oldest = t.Start
			}
			if t.Start.After(newest) {
				newest = t.Start
			}
		}
	}
	if sessions == 0 {
		fmt.Printf("\n  No sessions in the last %d days.\n\n", days)
		return
	}

	by := map[string][]*group{}
	for _, g := range into {
		by[g.Kind] = append(by[g.Kind], g)
	}

	span := ""
	if !oldest.IsZero() {
		span = fmt.Sprintf("%s → %s · ", oldest.Format("2 Jan"), newest.Format("2 Jan 2006"))
	}
	fmt.Printf("\n%s\n  YOUR CLAUDE CODE SPEND\n  %s%s sessions · %s model calls · %s\n%s\n",
		rule, span, comma(sessions), comma(calls), money(grand), rule)

	section("ALWAYS IN CONTEXT", "  prepended to every single call", by[scaffoldKind], grand)
	section("YOUR CUSTOMISATIONS", "  hooks, CLAUDE.md, listings, skill bodies", by[setup], grand)
	section("TOOL RESULTS YOU READ BACK", "", by[toolKind], grand)
	section("WHAT CLAUDE PRODUCED", "  generated once, then replayed", by[outKind], grand)
	section("WHAT YOU TYPED", "", by[promptKind], grand)

	yours := 0.0
	for _, g := range into {
		if g.Kind == setup || strings.HasPrefix(g.Name, "MCP ") {
			yours += g.Cost
		}
	}
	if grand > 0 {
		fmt.Printf("\n  Your own configuration — customisations, MCP servers and their\n"+
			"  results — accounts for %s of %s (%.0f%%).\n",
			money(yours), money(grand), 100*yours/grand)
	}

	whatToChange(all, since, into)

	fmt.Printf("\n  Cost is split by replay: size × how many calls carried it, against\n")
	fmt.Printf("  the real bill. Names and counts only — no conversation content.\n")
}

// ---------- what to change ----------
//
// Three lessons, each pinned to a number from the reader's own sessions. Generic
// advice next to real numbers makes the numbers look like advice too.

// lesson prints one numbered finding: a headline, an optional amount, and at
// most a couple of supporting lines.
func lesson(n int, headline, amount string, detail ...string) {
	if amount == "" {
		fmt.Printf("\n  %d  %s\n", n, headline)
	} else {
		fmt.Printf("\n  %d  %-56s %8s\n", n, headline, amount)
	}
	for _, d := range detail {
		fmt.Printf("     %s\n", clip(d, 63))
	}
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// idleMCP prices the MCP servers that were listed on every call but never once
// called. Their share of the listing cost is proportional to what their entries
// add to it.
func idleMCP(all []Trajectory, since time.Time, listingCost float64) (idle []group, total float64, listed, used int) {
	tokens, everUsed := map[string]int{}, map[string]bool{}
	for _, t := range all {
		if !t.Start.IsZero() && t.Start.Before(since) {
			continue
		}
		for srv, n := range t.Listed {
			tokens[srv] = max(tokens[srv], n) // the listing is the same each session, not cumulative
		}
		for srv := range t.UsedMCP {
			everUsed[srv] = true
		}
	}
	listedTok := 0
	for _, n := range tokens {
		listedTok += n
	}
	for srv, n := range tokens {
		listed++
		if everUsed[srv] {
			used++
			continue
		}
		cost := 0.0
		if listedTok > 0 {
			cost = listingCost * float64(n) / float64(listedTok)
		}
		idle = append(idle, group{Name: srv, Tok: n, Cost: cost})
		total += cost
	}
	sort.Slice(idle, func(i, j int) bool { return idle[i].Cost > idle[j].Cost })
	return idle, total, listed, used
}

// depthCurve is the average real input per call, bucketed by how deep into the
// session the call was. Every turn resends the ones before it, so this rises.
func depthCurve(all []Trajectory, since time.Time) ([]string, float64) {
	type bucket struct {
		label      string
		max        int
		sum, calls int
		cost       float64
	}
	bs := []bucket{{"1-10", 10, 0, 0, 0}, {"11-25", 25, 0, 0, 0},
		{"26-50", 50, 0, 0, 0}, {"51+", 1 << 30, 0, 0, 0}}
	for _, t := range all {
		if !t.Start.IsZero() && t.Start.Before(since) {
			continue
		}
		for i, c := range t.Calls {
			for j := range bs {
				if i+1 <= bs[j].max {
					bs[j].sum += c.Provider.InputTotal()
					bs[j].calls++
					bs[j].cost += priceOf(t.Model, sum(Trajectory{Model: t.Model, Calls: []Call{c}})).total()
					break
				}
			}
		}
	}
	var out []string
	var first, last float64
	for _, b := range bs {
		if b.calls == 0 {
			continue
		}
		avg := b.cost / float64(b.calls)
		if first == 0 {
			first = avg
		}
		last = avg
		out = append(out, fmt.Sprintf("%s %s", b.label, money(avg)))
	}
	mult := 0.0
	if first > 0 {
		mult = last / first
	}
	return out, mult
}

func whatToChange(all []Trajectory, since time.Time, into map[string]*group) {
	listingCost := 0.0
	for _, g := range into {
		if strings.HasPrefix(g.Name, "tool listings") || strings.HasSuffix(g.Name, "tool schemas") {
			listingCost += g.Cost
		}
	}

	fmt.Printf("\n%s\n  WHAT TO CHANGE\n%s\n", rule, rule)
	n := 0

	if idle, total, listed, used := idleMCP(all, since, listingCost); len(idle) > 0 {
		n++
		names := []string{}
		for _, g := range idle[:min(2, len(idle))] {
			names = append(names, clip(g.Name, 20))
		}
		if rest := len(idle) - len(names); rest > 0 {
			names = append(names, fmt.Sprintf("+%d more", rest))
		}
		lesson(n, fmt.Sprintf("%d of %d MCP servers were never called", len(idle), listed), money(total),
			fmt.Sprintf("Their tool listings ride every call anyway. You used %d.", used),
			"Idle: "+strings.Join(names, ", "))
	}

	if rows, mult := depthCurve(all, since); len(rows) > 1 {
		n++
		head := "Later calls in a session cost more than early ones"
		if mult > 1.5 {
			head = fmt.Sprintf("A call late in a session costs %.1fx an early one", mult)
		}
		lesson(n, head, "",
			"per call:  "+strings.Join(rows, "   "),
			"Every turn resends the ones before it. /clear between tasks.")
	}

	writes, reads := 0, 0
	for _, t := range all {
		if !t.Start.IsZero() && t.Start.Before(since) {
			continue
		}
		o := sum(t)
		writes += o.ProviderCacheMake
		reads += o.ProviderCacheRead
	}
	if w := writes + reads; w > 0 {
		n++
		share := 100 * float64(writes) / float64(w)
		switch {
		case share < 15:
			lesson(n, fmt.Sprintf("Cache is healthy — %.0f%% read, %.0f%% rewritten", 100-share, share), "good")
		case share < 40:
			lesson(n, fmt.Sprintf("Cache misses more than it should — %.0f%% rewritten", share), "check",
				"Something near the start of your context changes between calls.",
				"A hook that emits a timestamp or a counter will do this.")
		default:
			lesson(n, fmt.Sprintf("Cache is barely working — %.0f%% rewritten", share), "fix",
				"Cached input costs 0.1x to read but 1.25-2x to write. Something in",
				"your system prompt, CLAUDE.md or a hook changes on every call.")
		}
	}
}
