package main

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// One fixture exercising the three things that are easy to get wrong: Claude
// splits one assistant message across several records, replay accumulates
// across rounds, and a compaction throws that accumulation away.
const fixture = `
{"type":"user","message":{"role":"user","content":"aa bb"}}
{"type":"attachment","attachment":{"type":"hook_success","content":"cc"}}
{"type":"assistant","message":{"id":"m1","model":"test","role":"assistant","content":[{"type":"thinking","thinking":"dd ee"}],"usage":{"input_tokens":1,"cache_creation_input_tokens":10,"cache_read_input_tokens":0,"output_tokens":5}}}
{"type":"assistant","message":{"id":"m1","model":"test","role":"assistant","content":[{"type":"text","text":"ff"}],"usage":{"input_tokens":1,"cache_creation_input_tokens":10,"cache_read_input_tokens":0,"output_tokens":5}}}
{"type":"assistant","message":{"id":"m1","model":"test","role":"assistant","content":[{"type":"tool_use","name":"g","input":{}}],"usage":{"input_tokens":1,"cache_creation_input_tokens":10,"cache_read_input_tokens":0,"output_tokens":5}}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"ii jj kk"}]},"toolUseResult":{"stdout":"ii jj kk"}}
{"type":"assistant","message":{"id":"m2","model":"test","role":"assistant","content":[{"type":"text","text":"ll"}],"usage":{"input_tokens":2,"cache_creation_input_tokens":0,"cache_read_input_tokens":11,"output_tokens":7}}}
{"type":"user","isCompactSummary":true,"message":{"role":"user","content":"mm"}}
{"type":"assistant","message":{"id":"m3","model":"test","role":"assistant","content":[{"type":"text","text":"nn"}],"usage":{"input_tokens":0,"cache_creation_input_tokens":3,"cache_read_input_tokens":0,"output_tokens":1}}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":[{"type":"image","source":{"type":"base64","data":"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="}}]}]}}
{"type":"assistant","message":{"id":"m4","model":"test","role":"assistant","content":[{"type":"thinking","thinking":"","signature":"opaque"}],"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":9,"output_tokens_details":{"thinking_tokens":9}}}}
{"type":"user","isSidechain":true,"message":{"role":"user","content":"oo"}}
{"type":"assistant","isSidechain":true,"message":{"id":"s1","model":"test","role":"assistant","content":[{"type":"text","text":"pp"}],"usage":{"input_tokens":4,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1}}}
{"type":"file-history-snapshot","messageId":"ignored"}
`

func parseFixture(t *testing.T) []Trajectory {
	t.Helper()
	old := countTokens
	countTokens = func(s string) int { return len(strings.Fields(s)) } // words, offline
	t.Cleanup(func() { countTokens = old })
	return parseClaude([]byte(fixture), "fix")
}

func TestParse(t *testing.T) {
	got := parseFixture(t)
	if len(got) != 2 {
		t.Fatalf("want main + sidechain trajectories, got %d", len(got))
	}
	main, side := got[0], got[1]

	// Three records share message.id m1: one inference call, not three.
	if len(main.Calls) != 4 {
		t.Fatalf("inference calls = %d, want 4 (m1 split across 3 records)", len(main.Calls))
	}
	if len(side.Calls) != 1 {
		t.Errorf("sidechain calls = %d, want 1", len(side.Calls))
	}

	want := []Call{
		{P: 2, T: 0, A: 1, X: 0, MText: 1, MTool: 1, R: 2},
		{P: 0, T: 3, A: 0, X: 7, MText: 1}, // X = (2+0+1) + (1+1+2)
		{P: 0, T: 0, A: 1, X: 0, MText: 1}, // compaction cleared the carry
		{P: 0, T: 1, A: 0, X: 2, R: 9},     // 1x1 image; reasoning taken from provider
	}
	for i, w := range want {
		c := main.Calls[i]
		if c.P != w.P || c.T != w.T || c.A != w.A || c.X != w.X ||
			c.MText != w.MText || c.MTool != w.MTool || c.R != w.R {
			t.Errorf("call %d = %+v, want %+v", i+1, c, w)
		}
	}

	// Usage must be taken once per message.id, not once per record.
	o := sum(main)
	if o.ProviderInput != 3 || o.ProviderCacheMake != 13 || o.ProviderCacheRead != 11 || o.ProviderOutput != 22 {
		t.Errorf("provider usage counted wrong: %+v", o)
	}
}

func TestFormulas(t *testing.T) {
	o := sum(parseFixture(t)[0])
	if o.Chatting != o.P+o.MText {
		t.Errorf("chatting = %d", o.Chatting)
	}
	if o.AgentVisible != o.P+o.T+o.MText+o.MTool {
		t.Errorf("agent_visible = %d", o.AgentVisible)
	}
	if o.TrajectoryTotal != o.P+o.T+o.A+o.X+o.MText+o.MTool+o.R {
		t.Errorf("trajectory_total = %d", o.TrajectoryTotal)
	}
}

func TestImageTokens(t *testing.T) {
	// 1x1 PNG: (1*1)/750 rounds up to 1. Garbage must not panic or invent tokens.
	const px = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
	if got := imageTokens(px); got != 1 {
		t.Errorf("imageTokens(1x1) = %d, want 1", got)
	}
	if got := imageTokens("not-base64"); got != 0 {
		t.Errorf("imageTokens(garbage) = %d, want 0", got)
	}
}

// The cost split is only honest if every token's replay weight adds up to the
// input actually sent. If this drifts, the insights percentages are fiction.
func TestReplayWeightsSumToInput(t *testing.T) {
	tr := parseFixture(t)[0]
	w := 0
	for _, it := range tr.Items {
		w += it.Tok * tr.rounds(it)
	}
	if o := sum(tr); w != o.TrajectoryInput {
		t.Errorf("item weights = %d, trajectory_input = %d (compaction handled?)", w, o.TrajectoryInput)
	}
}

func TestItemLabels(t *testing.T) {
	var got []string
	for _, it := range parseFixture(t)[0].Items {
		got = append(got, it.Kind+"/"+it.Name)
	}
	for _, w := range []string{"prompt/what you typed", "setup/hook (unnamed)", "output/reasoning"} {
		if !slices.Contains(got, w) {
			t.Errorf("no item labelled %q; got %v", w, got)
		}
	}
}

func TestSubagentFiles(t *testing.T) {
	fsys := fstest.MapFS{
		"s1.jsonl":                        {},
		"s1/subagents/agent-a1.jsonl":     {},
		"s1/subagents/agent-a1.meta.json": {},
		"s1/tool-results/big.txt":         {},
	}
	got, err := sessionFiles(fsys)
	if err != nil || !slices.Equal(got, []string{"s1.jsonl", "s1/subagents/agent-a1.jsonl"}) {
		t.Fatalf("sessionFiles = %v, %v", got, err)
	}
	if s, p, sub := sessionOf("/x/proj/s1/subagents/agent-a1.jsonl"); s != "s1" || p != "proj" || !sub {
		t.Errorf("subagent file -> %q %q %v, want s1 proj true", s, p, sub)
	}
	if s, p, sub := sessionOf("/x/proj/s1.jsonl"); s != "s1" || p != "proj" || sub {
		t.Errorf("session file -> %q %q %v, want s1 proj false", s, p, sub)
	}
}

func TestInstructionsAttachment(t *testing.T) {
	const tr = `
{"type":"attachment","attachment":{"type":"instructions","files":[{"type":"User","content":"user one"},{"type":"Project","content":"proj one two"},{"type":"AutoMem","content":"auto"}]}}
{"type":"assistant","message":{"id":"m1","model":"test","role":"assistant","content":[{"type":"text","text":"reply"}],"usage":{"input_tokens":1,"output_tokens":1}}}
`
	old := countTokens
	countTokens = func(s string) int { return len(strings.Fields(s)) } // words, offline
	t.Cleanup(func() { countTokens = old })

	got := parseClaude([]byte(tr), "test")[0]
	items := map[string]int{}
	for _, it := range got.Items {
		items[it.Kind+"/"+it.Name] += it.Tok
	}
	for k, v := range map[string]int{
		"setup/CLAUDE.md (user)":    2,
		"setup/CLAUDE.md (project)": 3,
		"setup/memory (auto)":       1,
		"output/text you read":      1,
	} {
		if items[k] != v {
			t.Errorf("item %q = %d, want %d", k, items[k], v)
		}
	}
}

func TestPerCallPricing(t *testing.T) {
	const tr = `
{"type":"assistant","message":{"id":"a","model":"claude-opus-5","content":[],"usage":{"input_tokens":1000000,"output_tokens":0}}}
{"type":"assistant","message":{"id":"b","model":"claude-sonnet-5","content":[],"usage":{"input_tokens":1000000,"output_tokens":0}}}
{"type":"assistant","message":{"id":"c","model":"claude-haiku-4-5-20251001","content":[],"usage":{"input_tokens":1000000,"output_tokens":0}}}
{"type":"assistant","message":{"id":"d","model":"<synthetic>","content":[],"usage":{"input_tokens":0,"output_tokens":0}}}
`
	o := sum(parseClaude([]byte(tr), "x")[0])
	if o.CostUSD != 5+2+1 {
		t.Errorf("cost = %v, want 8 (opus 5 + sonnet 2 + haiku 1)", o.CostUSD)
	}
	if o.Model != "claude-opus-5" {
		t.Errorf("model = %q, want the one with most spend", o.Model)
	}
	if r, _ := rateOf("claude-opus-5-5[1m]"); r.In != 4 {
		t.Errorf("opus-5-5 priced at %v, want 4 (longest prefix, suffix stripped)", r.In)
	}
}

func TestIdleMCP(t *testing.T) {
	into := map[string]*group{
		"a": {Name: "built-in tool schemas", Cost: 50},
		"b": {Name: "tool listings (MCP)", Cost: 8},
		"c": {Name: "MCP server instructions", Cost: 2},
	}
	if c := mcpListingCost(into); c != 10 {
		t.Fatalf("listing cost = %v, want 10 (built-in schemas excluded)", c)
	}
	tools := func(ns ...string) map[string]bool {
		m := map[string]bool{}
		for _, n := range ns {
			m[n] = true
		}
		return m
	}
	trs := []Trajectory{
		{Listed: map[string]*listing{"claude_ai_Notion": {Tok: 30, Tools: tools("search", "fetch")}, "idle": {Tok: 10, Tools: tools("x")}},
			UsedMCP: map[string]bool{"claude_ai_Notion": true}},
		{Listed: map[string]*listing{"7dedc1d6-06bc-4a1e": {Tok: 30, Tools: tools("search", "fetch")}}},
	}
	idle, total, listed, used := idleMCP(trs, time.Time{}, 10)
	if listed != 2 || used != 1 || len(idle) != 1 || idle[0].Name != "idle" {
		t.Fatalf("listed %d used %d idle %v; want the UUID alias merged into claude_ai_Notion", listed, used, idle)
	}
	if total != 2.5 { // 10 of 40 listed tokens
		t.Errorf("idle cost = %v, want 2.5", total)
	}
}

func TestLedger(t *testing.T) {
	const tr = `
{"type":"cost-state","totalCostUSD":1.0,"modelUsage":{"claude-sonnet-5":{"costUSD":1.0}}}
{"type":"assistant","message":{"id":"a","model":"claude-sonnet-5","content":[],"usage":{"input_tokens":1000000,"output_tokens":0}}}
{"type":"cost-state","totalCostUSD":2.5,"modelUsage":{"claude-sonnet-5":{"costUSD":2.5}}}
`
	tr0 := parseClaude([]byte(tr), "s")[0]
	if o := sum(tr0); o.HarnessCostUSD != 2.5 || o.HarnessModels["claude-sonnet-5"] != 2.5 {
		t.Fatalf("ledger = %v %v, want the later total 2.5", o.HarnessCostUSD, o.HarnessModels)
	}
	child := Trajectory{ID: "s · Explore", Parent: "s", Calls: []Call{{Model: "claude-sonnet-5", Provider: Usage{Input: 250000}}}}
	if l, gap := ledgerGap([]Trajectory{tr0, child}); l != 2.5 || gap != 0 { // 2.5 - (2 + 0.5)
		t.Errorf("ledgerGap = %v, %v; want 2.5, 0 (subagent counted against its parent's ledger)", l, gap)
	}
}

func TestModelFindings(t *testing.T) {
	mtok := Usage{Input: 1000000}
	call := func(m string) Call { return Call{Model: m, Provider: mtok} }
	fs := newerModels([]Trajectory{{Calls: []Call{call("claude-sonnet-4-6"), call("claude-sonnet-5")}}})
	if len(fs) != 1 || fs[0].Save != 1 { // $3 -> $2
		t.Errorf("newerModels = %+v, want one finding saving $1 (Sonnet 5 has no successor)", fs)
	}
	parent := Trajectory{ID: "s", Calls: []Call{call("claude-opus-5")}}
	child := Trajectory{ID: "s · general-purpose", Parent: "s", Agent: "general-purpose", Calls: []Call{call("claude-opus-5")}}
	fork := Trajectory{ID: "s · fork", Parent: "s", Agent: "fork", Calls: []Call{call("claude-opus-5")}}
	f, ok, _ := inheritedSubagents([]Trajectory{parent, child, fork})
	if !ok || f.Save != 4 || !strings.Contains(f.Headline, "$5.00 of $5.00") { // $5 on Opus -> $1 on Haiku; fork excluded
		t.Errorf("inheritedSubagents = %+v %v, want 100%% inherited saving $4, fork excluded", f, ok)
	}
}

func TestIdleRebuilds(t *testing.T) {
	const epsilon = 0.0001
	t1 := time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
	t2 := t1.Add(2 * time.Hour)

	// Two calls 2 h apart with CacheMake > 20k should trigger one rebuild.
	u1 := Usage{CacheMake: 100000}
	u1.CacheTTL.M5 = 100000
	u2 := Usage{CacheMake: 100000}
	u2.CacheTTL.M5 = 100000

	f, ok := idleRebuilds([]Trajectory{
		{
			Calls: []Call{
				{At: t1, Model: "claude-sonnet-5", Provider: u1},
				{At: t2, Model: "claude-sonnet-5", Provider: u2},
			},
		},
	})
	if !ok {
		t.Fatalf("expected finding for 2h gap")
	}
	// For sonnet-5: In=2, Read=0.1; so premium = 100000 * (2/1e6) * (1.25 - 0.1) = 0.23
	want := 100000.0 * (2.0 / 1e6) * (1.25 - 0.1)
	if f.Save < want-epsilon || f.Save > want+epsilon {
		t.Errorf("Save = %.4f, want %.4f", f.Save, want)
	}

	// Two calls 30 s apart should not trigger.
	u3 := Usage{CacheMake: 100000}
	u3.CacheTTL.M5 = 100000
	u4 := Usage{CacheMake: 100000}
	u4.CacheTTL.M5 = 100000

	f2, ok2 := idleRebuilds([]Trajectory{
		{
			Calls: []Call{
				{At: t1, Model: "claude-sonnet-5", Provider: u3},
				{At: t1.Add(30 * time.Second), Model: "claude-sonnet-5", Provider: u4},
			},
		},
	})
	if ok2 {
		t.Errorf("expected no finding for 30s gap, got %+v", f2)
	}
}

func TestMaxEffort(t *testing.T) {
	// 60 calls at max with 1000 output tokens, 60 at high with 500 output tokens.
	// All on claude-opus-5 ($5 in, $25 out per million).
	max60 := make([]Call, 60)
	high60 := make([]Call, 60)
	for i := range max60 {
		max60[i] = Call{Provider: Usage{Output: 1000}, Model: "claude-opus-5", Effort: "max"}
		high60[i] = Call{Provider: Usage{Output: 500}, Model: "claude-opus-5", Effort: "high"}
	}
	all := []Trajectory{{Calls: append(max60, high60...)}}
	f, ok := maxEffort(all)
	if !ok {
		t.Fatal("maxEffort should return true with 60 calls each")
	}
	// Cost per call: max = 1000 * 25 / 1e6, high = 500 * 25 / 1e6
	pMax := 1000 * 25.0 / 1e6
	pHigh := 500 * 25.0 / 1e6
	wantSave := 60 * (pMax - pHigh)
	if f.Save < wantSave*0.99 || f.Save > wantSave*1.01 {
		t.Errorf("Save = %.8f, want ≈ %.8f", f.Save, wantSave)
	}
	// Same test with 40 calls each: should fail the >= 50 threshold.
	max40 := make([]Call, 40)
	high40 := make([]Call, 40)
	for i := range max40 {
		max40[i] = Call{Provider: Usage{Output: 1000}, Model: "claude-opus-5", Effort: "max"}
		high40[i] = Call{Provider: Usage{Output: 500}, Model: "claude-opus-5", Effort: "high"}
	}
	all = []Trajectory{{Calls: append(max40, high40...)}}
	_, ok = maxEffort(all)
	if ok {
		t.Fatal("maxEffort should return false with only 40 calls each")
	}
}

func TestFailedTools(t *testing.T) {
	// Fixture: tool_use, tool_result with is_error, assistant call at sonnet rate
	const tr = `
{"type":"assistant","message":{"id":"t1","model":"claude-sonnet-5","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{}}],"usage":{"input_tokens":0,"output_tokens":0}}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","is_error":true,"content":"boom"}]}}
{"type":"assistant","message":{"id":"t2","model":"claude-sonnet-5","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":0,"output_tokens":1000000}}}
`
	old := countTokens
	countTokens = func(s string) int { return len(strings.Fields(s)) }
	t.Cleanup(func() { countTokens = old })

	trs := parseClaude([]byte(tr), "test")
	f, ok := failedTools(trs)
	if !ok {
		t.Fatalf("failedTools returned ok=false, want true")
	}
	if f.Save != 10.0 {
		t.Errorf("Save = %v, want 10 (1M output tokens at sonnet $10/M)", f.Save)
	}
	if !strings.Contains(f.Detail[0], "Bash") {
		t.Errorf("detail %q missing Bash", f.Detail[0])
	}
	if !strings.Contains(f.Headline, "1") {
		t.Errorf("headline %q missing call count", f.Headline)
	}

	// Fixture without is_error
	const noErr = `
{"type":"assistant","message":{"id":"t1","model":"claude-sonnet-5","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{}}],"usage":{"input_tokens":0,"output_tokens":0}}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","is_error":false,"content":"ok"}]}}
`
	trs2 := parseClaude([]byte(noErr), "test")
	_, ok2 := failedTools(trs2)
	if ok2 {
		t.Errorf("failedTools on non-error fixture returned ok=true, want false")
	}
}

func TestHabits(t *testing.T) {
	var all []Trajectory
	for i := range 20 {
		tr := Trajectory{ID: fmt.Sprint(i), Calls: []Call{{Model: "claude-sonnet-5"}}, Commands: map[string]int{"/clear": 1}}
		if i == 0 {
			tr.Calls = append(tr.Calls, Call{Model: "claude-opus-5"})
		}
		all = append(all, tr)
	}
	if got := habits(all); len(got) != 2 || !strings.Contains(got[0], "1 of 20") || !strings.Contains(got[1], "20 times") {
		t.Errorf("habits = %q, want rare switches (1 of 20) and /clear 20 times", got)
	}
	if got := habits(all[:3]); len(got) != 0 {
		t.Errorf("habits on 3 sessions = %q, want none (below thresholds)", got)
	}
}

func TestCacheMisses(t *testing.T) {
	const tr = `
{"type":"assistant","message":{"id":"a","model":"claude-sonnet-5","content":[],"usage":{"input_tokens":1}}}
{"type":"assistant","message":{"id":"a","model":"claude-sonnet-5","content":[],"usage":{"input_tokens":1},"diagnostics":{"cache_miss_reason":{"type":"model_changed","cache_missed_input_tokens":1000000}}}}
`
	f, known := cacheMisses(parseClaude([]byte(tr), "s"))
	if !known || math.Abs(f.Save-2*(1.25-0.1)) > 1e-9 || !strings.Contains(f.Detail[0], "model_changed") {
		t.Errorf("cacheMisses = %+v %v, want model_changed priced at 1M × $2 × (1.25-0.1)", f, known)
	}
}

func TestSetupHealth(t *testing.T) {
	// Server failing in 2 of 3 sessions (>10%, >=2 failures) should print
	t1 := Trajectory{ID: "s1", FailedMCP: map[string]bool{"plugin:arch:arch": true}}
	t2 := Trajectory{ID: "s2", FailedMCP: map[string]bool{"plugin:arch:arch": true}}
	t3 := Trajectory{ID: "s3", Listed: map[string]*listing{"other": {}}}
	lines := setupHealth([]Trajectory{t1, t2, t3})
	if !slices.ContainsFunc(lines, func(s string) bool {
		return strings.Contains(s, "arch") && strings.Contains(s, "2") && strings.Contains(s, "3")
	}) {
		t.Errorf("setupHealth missing arch failure; got %v", lines)
	}

	// Server failing in 1 of 20 sessions (<10%) should not print
	var t20 []Trajectory
	t20 = append(t20, Trajectory{ID: "fail", FailedMCP: map[string]bool{"plugin:rare:rare": true}})
	for i := 0; i < 19; i++ {
		t20 = append(t20, Trajectory{ID: fmt.Sprintf("s%d", i)})
	}
	lines = setupHealth(t20)
	if slices.ContainsFunc(lines, func(s string) bool { return strings.Contains(s, "rare") }) {
		t.Errorf("setupHealth printed rare failure at 1/20; got %v", lines)
	}

	// Hook with 20 runs and 2 failures (10%, >=2) should print
	lines = setupHealth([]Trajectory{{ID: "h1", Hooks: map[string]*hookStats{"Start": {Runs: 20, Failed: 2}}}})
	if !slices.ContainsFunc(lines, func(s string) bool { return strings.Contains(s, "Start") && strings.Contains(s, "failed") }) {
		t.Errorf("setupHealth missing hook failure; got %v", lines)
	}

	// Hook with 100 runs and 1 failure (1%, <5%) should not print
	lines = setupHealth([]Trajectory{{ID: "h2", Hooks: map[string]*hookStats{"End": {Runs: 100, Failed: 1}}}})
	if slices.ContainsFunc(lines, func(s string) bool { return strings.Contains(s, "End") && strings.Contains(s, "failed") }) {
		t.Errorf("setupHealth printed 1/100 hook failure; got %v", lines)
	}
}

func TestIdleListings(t *testing.T) {
	old := countTokens
	countTokens = func(s string) int { return len(strings.Fields(s)) }
	t.Cleanup(func() { countTokens = old })
	const tr = `
{"type":"attachment","attachment":{"type":"skill_listing","content":"- p:a: one two\n- p:b: one two three four five six\n- solo: x"}}
{"type":"assistant","message":{"id":"m","model":"claude-sonnet-5","content":[{"type":"tool_use","id":"t","name":"Skill","input":{"skill":"solo"}}],"usage":{"input_tokens":1}}}
`
	tr0 := parseClaude([]byte(tr), "s")[0]
	into := map[string]*group{setup + "\x00skill listing": {Cost: 15}}
	f, ok := idleListings([]Trajectory{tr0}, into)
	// lines: "- p:a: one two" 4 words, "- p:b: ..." 8, "- solo: x" 3; p:a and p:b idle = 12/15 of $15
	if !ok || f.Save != 12 || !strings.HasPrefix(f.Headline, "2 of 3 skills") || !strings.Contains(f.Detail[0], "unused: p $12") {
		t.Errorf("idleListings = %+v %v, want 2 of 3 idle at $12, plugin p unused", f, ok)
	}
}

func TestPRParsing(t *testing.T) {
	const tr = `
{"type":"pr-link","prNumber":3252,"prRepository":"org/repo","gitBranch":"feature/main"}
{"type":"user","gitBranch":"feature/main","message":{"role":"user","content":"hi"}}
{"type":"assistant","message":{"id":"m1","model":"test","role":"assistant","content":[{"type":"text","text":"hello"}],"usage":{"input_tokens":1,"output_tokens":1}}}
`
	old := countTokens
	countTokens = func(s string) int { return len(strings.Fields(s)) }
	t.Cleanup(func() { countTokens = old })

	tr0 := parseClaude([]byte(tr), "test")[0]
	if !slices.Equal(tr0.PRs, []string{"org/repo#3252"}) {
		t.Errorf("PRs = %v, want [org/repo#3252]", tr0.PRs)
	}
	if tr0.Branch != "feature/main" {
		t.Errorf("Branch = %q, want feature/main", tr0.Branch)
	}
}

func TestPRCosts(t *testing.T) {
	// Session with two PRs should split cost equally between them
	// Session with one PR should contribute its full cost
	sess1 := Trajectory{
		ID:  "s1",
		PRs: []string{"org/repo#1", "org/repo#2"},
		Calls: []Call{
			{Model: "claude-sonnet-5", Provider: Usage{Input: 1000000, Output: 0}},
		},
	}
	// Second session with one of the same PRs
	sess2 := Trajectory{
		ID:  "s2",
		PRs: []string{"org/repo#1"},
		Calls: []Call{
			{Model: "claude-sonnet-5", Provider: Usage{Input: 1000000, Output: 0}},
		},
	}

	line := prCosts([]Trajectory{sess1, sess2})
	// sess1: $2 split into $1 each for PRs 1 and 2
	// sess2: $2 for PR 1
	// PR#1 total: $1 + $2 = $3
	// PR#2 total: $1
	// median should be ($1 + $3) / 2 = $2, max should be $3

	if !strings.Contains(line, "2 PRs") {
		t.Errorf("line %q missing PR count", line)
	}
	if !strings.Contains(line, "median") {
		t.Errorf("line %q missing median", line)
	}
	if !strings.Contains(line, "priciest") {
		t.Errorf("line %q missing priciest", line)
	}

	// No result when fewer than 2 PRs
	noResult := prCosts([]Trajectory{{ID: "s1", PRs: []string{"org/repo#1"}, Calls: []Call{{Model: "claude-sonnet-5", Provider: Usage{Input: 1000000}}}}})
	if noResult != "" {
		t.Errorf("prCosts with 1 PR returned %q, want empty", noResult)
	}
}

func TestWorkDriven(t *testing.T) {
	// Two calls with attributionPlugin "p1", one with attributionSkill "solo" (no plugin),
	// one with no attribution. Each sonnet call is $2 (1M input at $2/M).
	const tr = `
{"type":"assistant","attributionPlugin":"p1","message":{"id":"p1","model":"claude-sonnet-5","content":[],"usage":{"input_tokens":1000000,"output_tokens":0}}}
{"type":"assistant","attributionPlugin":"p1","message":{"id":"p2","model":"claude-sonnet-5","content":[],"usage":{"input_tokens":1000000,"output_tokens":0}}}
{"type":"assistant","attributionSkill":"solo","message":{"id":"s1","model":"claude-sonnet-5","content":[],"usage":{"input_tokens":1000000,"output_tokens":0}}}
{"type":"assistant","message":{"id":"none","model":"claude-sonnet-5","content":[],"usage":{"input_tokens":1000000,"output_tokens":0}}}
`
	trajs := parseClaude([]byte(tr), "test")
	gs := workDriven(trajs)
	if len(gs) != 2 {
		t.Fatalf("workDriven returned %d groups, want 2 (p1 and skill solo)", len(gs))
	}
	by := map[string]*group{}
	for _, g := range gs {
		by[g.Name] = g
	}
	if p1 := by["p1"]; p1 == nil || p1.Cost < 3.99 || p1.Cost > 4.01 || p1.N != 2 {
		t.Errorf("p1 group = %+v, want Cost ~$4 N 2", p1)
	}
	if solo := by["skill solo"]; solo == nil || solo.Cost < 1.99 || solo.Cost > 2.01 || solo.N != 1 {
		t.Errorf("skill solo group = %+v, want Cost ~$2 N 1", solo)
	}
	if unattributed := by[""]; unattributed != nil {
		t.Errorf("workDriven included unattributed call, want to skip it")
	}
}

func TestScanExtras(t *testing.T) {
	fsys := fstest.MapFS{
		"projects/p/s1.jsonl":                {},
		"projects/p/s1/tool-results/big.txt": {},
		"file-history/s1/abc@v1":             {},
		"history.jsonl":                      {},
		"paste-cache/x.txt":                  {},
	}
	if got := scanExtras(fsys, []string{"projects/p"}, false); !slices.Equal(got, []string{"projects/p/s1/tool-results/big.txt"}) {
		t.Errorf("one project: %v, want its tool-results only", got)
	}
	if got := scanExtras(fsys, []string{"projects/p"}, true); len(got) != 4 {
		t.Errorf("everything: %v, want tool-results, file-history, history, paste-cache", got)
	}
	for path, want := range map[string]string{
		"/h/.claude/projects/p/s1/tool-results/big.txt": "s1 p",
		"/h/.claude/file-history/s1/abc@v1":             "s1 ",
		"/h/.claude/paste-cache/x.txt":                  "paste-cache ~/.claude",
	} {
		if s, p, _ := sessionOf(path); s+" "+p != want {
			t.Errorf("sessionOf(%s) = %q %q, want %q", path, s, p, want)
		}
	}
}
