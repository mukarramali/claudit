package main

import (
	"cmp"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strings"
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

// attribute splits one session's real cost across everything that occupied its
// context, and adds what the transcript cannot account for as its own entry.
func attribute(t Trajectory, into map[string]*group) {
	o := sum(t)
	inCost := billOf(t).in()
	outRate := func(call int) float64 {
		if call < 0 || call >= len(t.Calls) {
			return 0
		}
		p, _ := rateOf(t.Calls[call].Model)
		return p.Out / 1e6
	}

	items := t.Items

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
			cost += float64(it.Tok) * outRate(it.Call) // generating it, on top of replaying it
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

func insights(all []Trajectory, since time.Time, days int, scope string) {
	if days <= 0 {
		since = time.Time{} // -days 0 means everything
	}
	into := map[string]*group{}
	calls, sessions, subs, grand := 0, 0, 0, 0.0
	unpriced := map[string]int{}
	oldest, newest := time.Time{}, time.Time{}
	for _, t := range all {
		if !t.Start.IsZero() && t.Start.Before(since) {
			continue
		}
		attribute(t, into)
		calls += len(t.Calls)
		if t.Parent != "" {
			subs++
		} else {
			sessions++
		}
		o := sum(t)
		grand += o.CostUSD
		for m, n := range o.Unpriced {
			unpriced[m] += n
		}
		if !t.Start.IsZero() {
			if oldest.IsZero() || t.Start.Before(oldest) {
				oldest = t.Start
			}
			if t.Start.After(newest) {
				newest = t.Start
			}
		}
	}
	if sessions+subs == 0 {
		fmt.Printf("\n  No sessions in the last %d days.\n\n", days)
		return
	}

	by := map[string][]*group{}
	for _, g := range into {
		by[g.Kind] = append(by[g.Kind], g)
	}

	period := ""
	switch {
	case days == 1:
		period = "today"
	case days > 1:
		period = fmt.Sprintf("last %d days", days)
	case !oldest.IsZero():
		period = fmt.Sprintf("%s → %s", oldest.Format("2 Jan"), newest.Format("2 Jan 2006"))
	}
	label := period
	if scope != "" {
		label = period + " · " + scope
	}
	runs := ""
	if subs > 0 {
		runs = fmt.Sprintf(" · %s subagent run%s", comma(subs), plural(subs))
	}
	fmt.Printf("\n%s\n  YOUR CLAUDE CODE SPEND\n  %s\n  %s sessions%s · %s model calls · %s\n",
		rule, label, comma(sessions), runs, comma(calls), money(grand))
	if ledger, gap := ledgerGap(inWindow(all, since)); gap >= 0.01*ledger && gap > 0 {
		fmt.Printf("  + %s (%.0f%%) more in Claude Code's own ledger: calls no transcript records\n",
			money(gap), 100*gap/ledger)
	}
	fmt.Println(rule)

	if len(unpriced) > 0 {
		n, names := 0, []string{}
		for m, c := range unpriced {
			n += c
			names = append(names, cmp.Or(m, "(no model)"))
		}
		sort.Strings(names)
		fmt.Printf("  %s call%s on unpriced models (%s) — not in the total\n", comma(n), plural(n), strings.Join(names, ", "))
	}

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

	concentration(inWindow(all, since), since)

	whatToChange(all, since, into)

	fmt.Printf("\n  Cost is split by replay: size × how many calls carried it, against\n")
	fmt.Printf("  the real bill. Names and counts only — no conversation content.\n")
}

// ledgerGap compares Claude Code's own cost ledger with the transcripts, over
// the sessions that have one. The ledger includes the session's subagent runs;
// what it adds beyond them is background calls no transcript records.
func ledgerGap(all []Trajectory) (ledger, gap float64) {
	seen := map[string]bool{}
	for _, t := range all {
		if t.Ledger > 0 {
			seen[t.ID] = true
			ledger += t.Ledger
		}
	}
	for _, t := range all {
		if seen[t.ID] || seen[t.Parent] {
			gap -= sum(t).CostUSD
		}
	}
	return ledger, ledger + gap
}

// ---------- what to change ----------
//
// Three lessons, each pinned to a number from the reader's own sessions. Generic
// advice next to real numbers makes the numbers look like advice too.

// lesson prints one numbered finding: a headline, an optional amount, and at
// most a couple of supporting lines.
func lesson(n int, headline, amount string, detail ...string) {
	if amount == "" || amount == "-" {
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

var connectorID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-`)

// aliasOf maps a claude.ai connector recorded under its UUID to the named
// server listing the same tools, or labels it by its first tools. Tool sets
// drift between sessions, so 80% overlap counts as the same connector.
func aliasOf(srv string, tools map[string]map[string]bool) string {
	if !connectorID.MatchString(srv) || len(tools[srv]) == 0 {
		return srv
	}
	best, bestShare := "", 0.0
	for name, ts := range tools {
		if connectorID.MatchString(name) {
			continue
		}
		n := 0
		for tl := range tools[srv] {
			if ts[tl] {
				n++
			}
		}
		if share := float64(n) / float64(len(tools[srv])); share > bestShare {
			best, bestShare = name, share
		}
	}
	if bestShare >= 0.8 {
		return best
	}
	names := slices.Sorted(maps.Keys(tools[srv]))
	return fmt.Sprintf("connector %s (%s, …)", srv[:8], strings.Join(names[:min(2, len(names))], ", "))
}

// idleMCP prices the MCP servers that were listed on every call but never once
// called. Their share of the listing cost is proportional to what their entries
// add to it.
func idleMCP(all []Trajectory, since time.Time, listingCost float64) (idle []group, total float64, listed, used int) {
	tools := map[string]map[string]bool{}
	for _, t := range all {
		for srv, l := range t.Listed {
			if tools[srv] == nil {
				tools[srv] = map[string]bool{}
			}
			maps.Copy(tools[srv], l.Tools)
		}
	}
	tokens, everUsed := map[string]int{}, map[string]bool{}
	for _, t := range all {
		if !t.Start.IsZero() && t.Start.Before(since) {
			continue
		}
		for srv, l := range t.Listed {
			srv = aliasOf(srv, tools)
			tokens[srv] = max(tokens[srv], l.Tok) // the listing is the same each session, not cumulative
		}
		for srv := range t.UsedMCP {
			everUsed[aliasOf(srv, tools)] = true
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
					bs[j].cost += c.bill().total()
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

// mcpListingCost is what MCP servers add to every call. Built-in tool schemas
// ride along whether or not any server is connected, so they are not in it.
func mcpListingCost(into map[string]*group) float64 {
	c := 0.0
	for _, g := range into {
		if g.Name == "tool listings (MCP)" || g.Name == "MCP server instructions" ||
			strings.HasPrefix(g.Name, "MCP ") && strings.HasSuffix(g.Name, " tool schemas") {
			c += g.Cost
		}
	}
	return c
}

// finding is one candidate for WHAT TO CHANGE. The biggest saving prints
// first; Tag replaces the amount for findings that have none.
type finding struct {
	Headline string
	Save     float64
	Tag      string
	Detail   []string
}

// inWindow is the trajectories that started inside the report window.
func inWindow(all []Trajectory, since time.Time) []Trajectory {
	var out []Trajectory
	for _, t := range all {
		if t.Start.IsZero() || !t.Start.Before(since) {
			out = append(out, t)
		}
	}
	return out
}

func whatToChange(all []Trajectory, since time.Time, into map[string]*group) {
	all = inWindow(all, since)
	var fs []finding
	var good []string

	if idle, total, listed, used := idleMCP(all, since, mcpListingCost(into)); len(idle) > 0 {
		names := []string{}
		for _, g := range idle[:min(2, len(idle))] {
			names = append(names, clip(g.Name, 20))
		}
		if rest := len(idle) - len(names); rest > 0 {
			names = append(names, fmt.Sprintf("+%d more", rest))
		}
		fs = append(fs, finding{fmt.Sprintf("%d of %d MCP servers were never called", len(idle), listed), total, "", []string{
			fmt.Sprintf("Their tool listings ride every call anyway. You used %d.", used),
			"Idle: " + strings.Join(names, ", ")}})
	}

	writes, reads := 0, 0
	for _, t := range all {
		o := sum(t)
		writes += o.ProviderCacheMake
		reads += o.ProviderCacheRead
	}
	misses, known := cacheMisses(all)
	if misses.Save >= 0.05 {
		fs = append(fs, misses)
	}
	if w := writes + reads; w > 0 {
		share := 100 * float64(writes) / float64(w)
		switch {
		case share < 15:
			good = append(good, fmt.Sprintf("Cache is healthy — %.0f%% read, %.0f%% rewritten", 100-share, share))
		case known: // the provider said why; that finding replaces the guess
		case share < 40:
			fs = append(fs, finding{fmt.Sprintf("Cache misses more than it should — %.0f%% rewritten", share), 0, "check", []string{
				"Something near the start of your context changes between calls.",
				"A hook that emits a timestamp or a counter will do this."}})
		default:
			fs = append(fs, finding{fmt.Sprintf("Cache is barely working — %.0f%% rewritten", share), 0, "fix", []string{
				"Cached input costs 0.1x to read but 1.25-2x to write. Something in",
				"your system prompt, CLAUDE.md or a hook changes on every call."}})
		}
	}

	if f, ok := idleListings(all, into); ok {
		fs = append(fs, f)
	}
	good = append(good, habits(all)...)
	fs = append(fs, newerModels(all)...)
	if f, ok, g := inheritedSubagents(all); ok {
		fs = append(fs, f)
	} else if g != "" {
		good = append(good, g)
	}
	for _, lesson := range []func([]Trajectory) (finding, bool){idleRebuilds, maxEffort, failedTools} {
		if f, ok := lesson(all); ok {
			fs = append(fs, f)
		}
	}

	// ponytail: stable sort keeps insertion order among amount-less findings.
	sort.SliceStable(fs, func(i, j int) bool { return fs[i].Save > fs[j].Save })
	fmt.Printf("\n%s\n  WHAT TO CHANGE\n%s\n", rule, rule)
	for i, f := range fs[:min(5, len(fs))] {
		lesson(i+1, f.Headline, cmp.Or(f.Tag, money(f.Save)), f.Detail...)
	}
	if len(good) > 0 {
		fmt.Printf("\n  GOING WELL\n")
		for _, g := range good[:min(3, len(good))] {
			fmt.Printf("  ✓  %s\n", g)
		}
	}
	if lines := setupHealth(all); len(lines) > 0 {
		fmt.Printf("\n  SETUP HEALTH\n")
		for _, l := range lines {
			fmt.Printf("  !  %s\n", l)
		}
	}
}

// successor lists only pairs where the newer model is cheaper at list price.
var successor = map[string]string{
	"claude-sonnet-4-6": "claude-sonnet-5",
	"claude-opus-5":     "claude-opus-5-5",
	"claude-opus-4-8":   "claude-opus-5-5",
	"claude-opus-4-7":   "claude-opus-5-5",
	"claude-opus-4-6":   "claude-opus-5-5",
}

// pretty turns claude-sonnet-4-6 into Sonnet 4.6.
func pretty(model string) string {
	f := strings.Split(strings.TrimPrefix(modelKey(model), "claude-"), "-")
	if len(f) == 0 || f[0] == "" {
		return model
	}
	return strings.ToUpper(f[0][:1]) + f[0][1:] + " " + strings.Join(f[1:], ".")
}

// newerModels reprices calls on an older model at its cheaper successor.
func newerModels(all []Trajectory) []finding {
	type acc struct {
		n        int
		was, now float64
	}
	by := map[string]*acc{}
	for _, t := range all {
		for _, c := range t.Calls {
			k := modelKey(c.Model)
			next, ok := successor[k]
			if !ok {
				continue
			}
			if by[k] == nil {
				by[k] = &acc{}
			}
			by[k].n++
			by[k].was += c.bill().total()
			by[k].now += priceAt(next, c.Provider).total()
		}
	}
	var out []finding
	for k, a := range by {
		if a.was-a.now < 0.01 {
			continue
		}
		family := strings.ToUpper(strings.Fields(pretty(k))[0])
		out = append(out, finding{
			Headline: fmt.Sprintf("%s where %s is cheaper", pretty(k), pretty(successor[k])),
			Save:     a.was - a.now,
			Detail: []string{
				fmt.Sprintf("%s calls · same tokens at list price: %s → %s", comma(a.n), money(a.was), money(a.now)),
				fmt.Sprintf("Pinned? ANTHROPIC_DEFAULT_%s_MODEL: ask whoever sets it.", family),
			},
		})
	}
	return out
}

// inheritedSubagents finds subagent runs that ran on their parent session's
// model, which is the default when neither the agent nor the environment sets
// one. Forks are left out: they must share the parent's model to reuse its
// cache. With little inherited it returns a good line instead.
func inheritedSubagents(all []Trajectory) (finding, bool, string) {
	parent := map[string]string{}
	for _, t := range all {
		if t.Parent == "" {
			parent[t.ID] = modelKey(sum(t).Model)
		}
	}
	const cheap = "claude-haiku-4-5"
	spend, inherited, onHaiku := 0.0, 0.0, 0.0
	byAgent := map[string]float64{}
	for _, t := range all {
		if t.Parent == "" || t.Agent == "fork" {
			continue
		}
		for _, c := range t.Calls {
			cost := c.bill().total()
			spend += cost
			if pm := parent[t.Parent]; pm == "" || modelKey(c.Model) != pm {
				continue
			}
			inherited += cost
			byAgent[t.Agent] += cost
			onHaiku += min(cost, priceAt(cheap, c.Provider).total())
		}
	}
	if spend < 0.01 {
		return finding{}, false, ""
	}
	if share := inherited / spend; share < 0.3 {
		return finding{}, false, fmt.Sprintf("Subagents mostly on cheaper models — %.0f%% on your main model", 100*share)
	}
	top := slices.SortedFunc(maps.Keys(byAgent), func(a, b string) int { return cmp.Compare(byAgent[b], byAgent[a]) })
	var names []string
	for _, a := range top[:min(2, len(top))] {
		names = append(names, fmt.Sprintf("%s %s", clip(a, 20), money(byAgent[a])))
	}
	return finding{
		Headline: fmt.Sprintf("Subagents ran on your main model: %s of %s", money(inherited), money(spend)),
		Save:     inherited - onHaiku,
		Detail: []string{
			fmt.Sprintf("~%s on Haiku · %s", money(onHaiku), strings.Join(names, ", ")),
			"Set model: in the agent, or CLAUDE_CODE_SUBAGENT_MODEL.",
		},
	}, true, ""
}

// idleRebuilds flags cache rewrites after gaps longer than the cache TTL.
// After a gap > 5 min, the next call re-sends context at write price (1.25-2x)
// instead of read price (0.1x), costing extra tokens unnecessarily.
func idleRebuilds(all []Trajectory) (finding, bool) {
	var short, long int
	var shortPrem, longPrem float64
	const gapShort = 5 * time.Minute
	const gapLong = 1 * time.Hour

	for _, t := range all {
		for i := 1; i < len(t.Calls); i++ {
			c := &t.Calls[i]
			prev := &t.Calls[i-1]
			if c.At.IsZero() || prev.At.IsZero() || c.Provider.CacheMake <= 20000 {
				continue
			}
			gap := c.At.Sub(prev.At)
			if gap <= gapShort {
				continue
			}
			r, ok := rateOf(c.Model)
			if !ok {
				continue
			}
			in := r.In / 1e6
			m5, h1 := c.Provider.cacheWrites()
			prem := float64(m5)*in*(1.25-r.Read) + float64(h1)*in*(2-r.Read)
			if gap <= gapLong {
				short++
				shortPrem += prem
			} else {
				long++
				longPrem += prem
			}
		}
	}

	total := shortPrem + longPrem
	if short+long == 0 || total < 0.01 {
		return finding{}, false
	}

	detail := []string{
		fmt.Sprintf("%d after 5-60 min idle, %d after over an hour", short, long),
	}
	if shortPrem > longPrem {
		detail = append(detail, "CLAUDE_CODE_PROMPT_CACHE_TTL=1h keeps the cache warm for an hour.")
	} else {
		detail = append(detail, "Start a new session instead of resuming, or /compact first.")
	}

	return finding{
		Headline: fmt.Sprintf("Resuming idle sessions rewrote the cache %d times", short+long),
		Save:     total,
		Detail:   detail,
	}, true
}

// maxEffort compares $/call at max and high effort on the same model. Max
// calls may be the harder tasks, so the saving is an upper bound.
func maxEffort(all []Trajectory) (finding, bool) {
	type stats struct {
		n    int
		cost float64
	}
	groups := map[[2]string]*stats{}
	for _, t := range all {
		for _, c := range t.Calls {
			k := [2]string{modelKey(c.Model), c.Effort}
			if groups[k] == nil {
				groups[k] = &stats{}
			}
			groups[k].n++
			groups[k].cost += c.bill().total()
		}
	}
	var best finding
	for k, mx := range groups {
		hi := groups[[2]string{k[0], "high"}]
		if k[1] != "max" || hi == nil || mx.n < 50 || hi.n < 50 {
			continue
		}
		pMax, pHigh := mx.cost/float64(mx.n), hi.cost/float64(hi.n)
		if save := float64(mx.n) * (pMax - pHigh); pMax > pHigh*1.15 && save > best.Save {
			best = finding{
				Headline: fmt.Sprintf("Max effort costs %.0f%% more per call on %s", 100*(pMax/pHigh-1), pretty(k[0])),
				Save:     save,
				Detail: []string{
					fmt.Sprintf("%d calls at max, %s each · %s at high", mx.n, money(pMax), money(pHigh)),
					"Use /effort max for the task that needs it, not as a default.",
				},
			}
		}
	}
	return best, best.Save > 0
}

// failedTools sums the calls that reacted to a tool result marked is_error:
// reading the error and retrying is a full call each time.
func failedTools(all []Trajectory) (finding, bool) {
	cost, count := map[string]float64{}, map[string]int{}
	total, n := 0.0, 0
	for _, t := range all {
		for _, c := range t.Calls {
			if c.AfterError != "" {
				v := c.bill().total()
				cost[c.AfterError] += v
				count[c.AfterError]++
				total += v
				n++
			}
		}
	}
	if total < 0.01 {
		return finding{}, false
	}
	top := slices.SortedFunc(maps.Keys(cost), func(a, b string) int { return cmp.Compare(cost[b], cost[a]) })
	var parts []string
	for _, tl := range top[:min(2, len(top))] {
		parts = append(parts, fmt.Sprintf("%s %d (%s)", clip(tl, 22), count[tl], money(cost[tl])))
	}
	return finding{
		Headline: fmt.Sprintf("%d calls went to recovering from failed tool calls", n),
		Save:     total,
		Detail:   []string{strings.Join(parts, " · "), "Each failure costs a full call to read the error and retry."},
	}, true
}

// concentration says where the money is: how much the priciest tenth of
// sessions cost and what they look like, so nobody optimises quick questions
// when the long sessions are the bill. A session includes its subagent runs.
func concentration(all []Trajectory, since time.Time) {
	type sess struct {
		calls int
		cost  float64
		model string
	}
	by := map[string]*sess{}
	grand := 0.0
	for _, t := range all {
		id := cmp.Or(t.Parent, t.ID)
		if by[id] == nil {
			by[id] = &sess{}
		}
		o := sum(t)
		by[id].cost += o.CostUSD
		grand += o.CostUSD
		if t.Parent == "" {
			by[id].calls, by[id].model = len(t.Calls), modelKey(o.Model)
		}
	}
	ss := slices.SortedFunc(maps.Values(by), func(a, b *sess) int { return cmp.Compare(b.cost, a.cost) })
	if len(ss) < 10 || grand <= 0 {
		return
	}
	bucket := func(n int) string {
		switch {
		case n <= 10:
			return "1-10"
		case n <= 50:
			return "11-50"
		case n <= 200:
			return "51-200"
		}
		return "200+"
	}
	top := ss[:len(ss)/10]
	topCost, shape := 0.0, map[string]float64{}
	for _, s := range top {
		topCost += s.cost
		shape[bucket(s.calls)+" calls on "+pretty(s.model)] += s.cost
	}
	common := slices.SortedFunc(maps.Keys(shape), func(a, b string) int { return cmp.Compare(shape[b], shape[a]) })[0]
	if len(top) == 1 {
		fmt.Printf("\n  Your priciest session, %s, cost\n  %s of %s (%.0f%%).", common, money(topCost), money(grand), 100*topCost/grand)
	} else {
		fmt.Printf("\n  The priciest %d sessions cost %s of %s (%.0f%%), mostly\n  %s.",
			len(top), money(topCost), money(grand), 100*topCost/grand, common)
	}
	// The top-tier model is whatever the priciest session ran on; quick
	// sessions on it are what people cut first.
	quick := 0.0
	for _, s := range ss {
		if s.calls <= 10 && s.model == ss[0].model {
			quick += s.cost
		}
	}
	if quick > 0 && quick < 0.02*grand {
		fmt.Printf(" Quick %s sessions (≤10 calls)\n  cost %s in all — not where the money is.", pretty(ss[0].model), money(quick))
	}
	if rows, mult := depthCurve(all, since); len(rows) > 1 && mult > 1.5 {
		fmt.Printf("\n  A call late in a session costs %.1fx an early one: /clear between tasks.", mult)
	}
	fmt.Println()
}

// habits are the good lines that make the bad ones credible: they print only
// when their threshold holds.
func habits(all []Trajectory) []string {
	var out []string
	sessions, switched, clears := 0, 0, 0
	for _, t := range all {
		if t.Parent != "" {
			continue
		}
		sessions++
		clears += t.Commands["/clear"]
		seen := map[string]bool{}
		for _, c := range t.Calls {
			if k := modelKey(c.Model); k != "" {
				seen[k] = true
			}
		}
		if len(seen) > 1 {
			switched++
		}
	}
	if sessions >= 10 && float64(switched) <= 0.05*float64(sessions) {
		out = append(out, fmt.Sprintf("Model switches are rare — %d of %d sessions", switched, sessions))
	}
	if clears >= 5 {
		out = append(out, fmt.Sprintf("/clear used %d times — context stays short", clears))
	}
	return out
}

// missFix is what to do about each cache-miss reason the provider reports.
var missFix = map[string]string{
	"model_changed":    "new session instead of /model",
	"tools_changed":    "an MCP server connected late or flapped",
	"system_changed":   "CLAUDE.md or a hook changed mid-session",
	"messages_changed": "earlier messages edited or rewound",
}

// cacheMisses prices the misses the provider explained: the missed tokens
// were written at cache-write price instead of read. Older transcripts carry
// no reason; known reports whether any call did.
func cacheMisses(all []Trajectory) (finding, bool) {
	cost, count := map[string]float64{}, map[string]int{}
	known := false
	for _, t := range all {
		for _, c := range t.Calls {
			known = known || c.Miss != ""
			r, ok := rateOf(c.Model)
			if c.Missed == 0 || !ok {
				continue
			}
			write := 1.25
			if c.Provider.CacheTTL.H1 > 0 {
				write = 2
			}
			cost[c.Miss] += float64(c.Missed) * r.In / 1e6 * (write - r.Read)
			count[c.Miss]++
		}
	}
	top := slices.SortedFunc(maps.Keys(cost), func(a, b string) int { return cmp.Compare(cost[b], cost[a]) })
	f := finding{}
	n := 0
	for i, k := range top {
		f.Save += cost[k]
		n += count[k]
		if i < 2 {
			f.Detail = append(f.Detail, fmt.Sprintf("%s ×%d %s: %s", k, count[k], money(cost[k]), cmp.Or(missFix[k], "see diagnostics")))
		}
	}
	f.Headline = fmt.Sprintf("The provider says why the cache missed %d times", n)
	return f, known
}

// setupHealth reports setup defects that cost time rather than tokens: MCP
// servers failing to connect in over 10% of sessions, hooks failing over 5%
// of runs or averaging over 5 s. Worst first, at most five.
func setupHealth(all []Trajectory) []string {
	type line struct {
		text  string
		count int
	}
	var lines []line
	add := func(n int, format string, a ...any) {
		lines = append(lines, line{clip(fmt.Sprintf(format, a...), 63), n})
	}

	fails, sessions := map[string]int{}, 0
	hooks := map[string]*hookStats{}
	for _, t := range all {
		for name, h := range t.Hooks {
			if hooks[name] == nil {
				hooks[name] = &hookStats{}
			}
			hooks[name].Runs += h.Runs
			hooks[name].Failed += h.Failed
			hooks[name].TimedOut += h.TimedOut
			hooks[name].Ms += h.Ms
		}
		if t.Parent != "" || len(t.FailedMCP)+len(t.Listed) == 0 {
			continue // a session that connects to MCP at all
		}
		sessions++
		for srv := range t.FailedMCP {
			fails[srv]++
		}
	}
	for srv, n := range fails {
		if n >= 2 && 10*n > sessions {
			// plugin:x:x is how a plugin names its own server
			if p := strings.Split(srv, ":"); len(p) == 3 && p[0] == "plugin" && p[1] == p[2] {
				srv = p[1]
			}
			add(n, "%s failed to connect in %d of %d sessions", clip(srv, 30), n, sessions)
		}
	}
	for name, h := range hooks {
		if h.Failed >= 2 && 20*h.Failed > h.Runs {
			add(h.Failed, "%s hook failed %d of %d runs", clip(name, 30), h.Failed, h.Runs)
		}
		if avg := float64(h.Ms) / float64(max(h.Runs, 1)) / 1000; avg > 5 {
			timeouts := ""
			if h.TimedOut > 0 {
				timeouts = fmt.Sprintf(", %d timed out", h.TimedOut)
			}
			add(h.Runs, "%s hook averages %.1f s per run%s", clip(name, 25), avg, timeouts)
		}
	}
	sort.Slice(lines, func(i, j int) bool { return lines[i].count > lines[j].count })
	var out []string
	for _, l := range lines[:min(5, len(lines))] {
		out = append(out, l.text)
	}
	return out
}

// pluginOf is the plugin that ships a skill or agent: the part of
// plugin:name before the colon, or "" for a standalone one.
func pluginOf(name string) string {
	p, _, ok := strings.Cut(name, ":")
	if !ok {
		return ""
	}
	return p
}

// idleListings is the MCP lesson for skills and agents: every listed one rides
// every call, used or not, priced by its line's share of its listing. Plugins
// none of whose skills, agents or MCP servers were used are named, since
// disabling one is a single command.
func idleListings(all []Trajectory, into map[string]*group) (finding, bool) {
	skills, agents, used := map[string]int{}, map[string]int{}, map[string]bool{}
	pluginUsed := map[string]bool{}
	for _, t := range all {
		for k, n := range t.Skills {
			skills[k] = max(skills[k], n)
		}
		for k, n := range t.Agents {
			agents[k] = max(agents[k], n)
		}
		for k := range t.Invoked {
			used[k] = true
		}
		for k := range t.Commands {
			used[strings.TrimPrefix(k, "/")] = true
		}
		for srv := range t.UsedMCP {
			if p, ok := strings.CutPrefix(srv, "plugin_"); ok {
				p, _, _ = strings.Cut(p, "_")
				pluginUsed[p] = true
			}
		}
	}
	cost := func(kind string) float64 {
		if g := into[setup+"\x00"+kind]; g != nil {
			return g.Cost
		}
		return 0
	}
	type plugin struct {
		n    int
		cost float64
	}
	plugins := map[string]*plugin{}
	total := 0.0
	idle := func(listed map[string]int, listingCost float64) (n int) {
		tok := 0
		for _, v := range listed {
			tok += v
		}
		for name, v := range listed {
			if p := pluginOf(name); used[name] {
				pluginUsed[p] = true
				continue
			}
			n++
			c := listingCost * float64(v) / float64(max(tok, 1))
			total += c
			if p := pluginOf(name); p != "" {
				if plugins[p] == nil {
					plugins[p] = &plugin{}
				}
				plugins[p].n++
				plugins[p].cost += c
			}
		}
		return n
	}
	idleSkills := idle(skills, cost("skill listing"))
	idleAgents := idle(agents, cost("subagent listing"))
	if total < 0.01 {
		return finding{}, false
	}
	var unused []string
	for p := range plugins {
		if !pluginUsed[p] {
			unused = append(unused, p)
		}
	}
	slices.SortFunc(unused, func(a, b string) int { return cmp.Compare(plugins[b].cost, plugins[a].cost) })
	var names []string
	for _, p := range unused[:min(2, len(unused))] {
		names = append(names, fmt.Sprintf("%s %s", clip(p, 20), money(plugins[p].cost)))
	}
	if len(unused) > 2 {
		names = append(names, fmt.Sprintf("+%d", len(unused)-2))
	}
	detail := []string{"Plugins unused: " + strings.Join(names, ", ")}
	if len(unused) == 0 {
		detail = []string{"Every plugin listing them was used at least once."}
	}
	detail = append(detail, "/plugin disable <name>, or authors: disable-model-invocation.")
	return finding{
		Headline: fmt.Sprintf("%d of %d skills and %d of %d agents were never used", idleSkills, len(skills), idleAgents, len(agents)),
		Save:     total,
		Detail:   detail,
	}, true
}
