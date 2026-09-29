package main

import (
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
