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

	fmt.Printf("\n  Cost is split by replay: size × how many calls carried it, against\n")
	fmt.Printf("  the real bill. Names and counts only — no conversation content.\n")
}
