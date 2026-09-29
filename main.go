// claudit counts tokens locally from agent transcripts and compares the result
// with the provider-reported usage embedded in those transcripts.
//
// Buckets and formulas follow docs/plan.md:
//
//	input_n  = P_n + T_n + A_n + X_n
//	output_n = M_text_n + M_tool_n + R_n
package main

import (
	"bytes"
	"cmp"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	tiktoken "github.com/pkoukk/tiktoken-go"
	tiktoken_loader "github.com/pkoukk/tiktoken-go-loader"
)

// ---------- agent-neutral model ----------

// Usage is what the provider reported for one inference call.
type Usage struct {
	Input     int `json:"input_tokens"`
	CacheMake int `json:"cache_creation_input_tokens"`
	CacheRead int `json:"cache_read_input_tokens"`
	Output    int `json:"output_tokens"`
	Details   struct {
		Thinking int `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
	CacheTTL struct {
		M5 int `json:"ephemeral_5m_input_tokens"`
		H1 int `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
}

func (u Usage) InputTotal() int { return u.Input + u.CacheMake + u.CacheRead }
func (u Usage) Empty() bool     { return u.InputTotal() == 0 && u.Output == 0 }

// Call is one inference round. P/T/A are the material newly added before this
// round; X is everything carried over from earlier rounds (replay).
type Call struct {
	P, T, A, X       int
	MText, MTool, R  int
	Provider         Usage
	Model, Effort    string
	At               time.Time
	AfterError       string // label of the tool whose failed result this call reacted to
	Miss             string // why the provider missed the cache, when it said
	Missed           int    // input tokens that miss rewrote
	AfterCompact     bool
	RedactedThinking int // thinking blocks whose content the provider withheld
}

func (c Call) NewInput() int { return c.P + c.T + c.A }
func (c Call) Input() int    { return c.NewInput() + c.X }
func (c Call) Output() int   { return c.MText + c.MTool + c.R }

// item is one thing that entered the context, tagged with what put it there.
// Call is the inference call it first appears in; Out marks model output, which
// only replays from the *following* call onward.
type item struct {
	Kind, Name string
	Tok, Call  int
	Out        bool
}

// Trajectory is one conversation: a sequence of inference calls.
type Trajectory struct {
	ID, Model, CWD string
	Parent, Agent  string // set on a subagent run: the session that spawned it, and its agentType
	Start          time.Time
	Calls          []Call
	Items          []item
	Scaffold       *scaffold
	Listed         map[string]*listing // MCP server -> what it adds to the tool listing
	UsedMCP        map[string]bool     // MCP server -> actually called at least once
	Commands       map[string]int      // slash command name -> times run
	Ledger         float64             // Claude Code's own running cost for the session
	LedgerModels   map[string]float64  // model -> its cost in that ledger
}

// scaffold is the system prompt and tool schemas prepended to every call. The
// client records them in a prompt_snapshot attachment, so on sessions that have
// one they can be measured instead of inferred from the provider's residual.
type scaffold struct {
	System int
	Tools  map[string]int
}

// listing is what one MCP server adds to every call: its deferred-tool lines
// plus its instruction block. Tools are kept to recognise one connector listed
// under two names.
type listing struct {
	Tok   int
	Tools map[string]bool
}

func (t *Trajectory) listed(srv string) *listing {
	if t.Listed == nil {
		t.Listed = map[string]*listing{}
	}
	l := t.Listed[srv]
	if l == nil {
		l = &listing{Tools: map[string]bool{}}
		t.Listed[srv] = l
	}
	return l
}

// noteListing records what each MCP server contributes to the deferred-tool
// listing and the server instructions, so servers that are never called can be
// priced separately.
func (t *Trajectory) noteListing(raw json.RawMessage) {
	var a struct {
		Type   string   `json:"type"`
		Lines  []string `json:"addedLines"`
		Names  []string `json:"addedNames"`
		Blocks []string `json:"addedBlocks"`
	}
	if json.Unmarshal(raw, &a) != nil {
		return
	}
	switch a.Type {
	case "deferred_tools_delta":
		for i, name := range a.Names {
			srv := mcpServer(name)
			if srv == "" {
				continue
			}
			text := name
			if i < len(a.Lines) {
				text = a.Lines[i]
			}
			l := t.listed(srv)
			l.Tok += countTokens(text)
			l.Tools[name[len("mcp__"+srv+"__"):]] = true
		}
	case "mcp_instructions_delta":
		// Instructions name the server as displayed ("claude.ai Notion"); tool
		// names spell it claude_ai_Notion.
		for i, name := range a.Names {
			if i < len(a.Blocks) {
				t.listed(strings.NewReplacer(".", "_", " ", "_").Replace(name)).Tok += countTokens(a.Blocks[i])
			}
		}
	}
}

func parseScaffold(raw json.RawMessage) *scaffold {
	var a struct {
		System []string `json:"systemPrompt"`
		Tools  []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Schema      json.RawMessage `json:"schema"`
		} `json:"tools"`
	}
	if json.Unmarshal(raw, &a) != nil {
		return nil
	}
	sc := &scaffold{Tools: map[string]int{}}
	for _, part := range a.System {
		sc.System += countTokens(part)
	}
	for _, tl := range a.Tools {
		label := "built-in"
		if srv := mcpServer(tl.Name); srv != "" {
			label = "MCP " + srv
		}
		sc.Tools[label] += countTokens(tl.Name + tl.Description + string(tl.Schema))
	}
	return sc
}

// rounds is how many inference calls carried this item in their input. An input
// item entering before call c rides calls c..N-1; output produced at call c
// rides c+1..N-1.
func (t Trajectory) rounds(i item) int {
	end := len(t.Calls)
	for c := i.Call + 1; c < len(t.Calls); c++ {
		if t.Calls[c].AfterCompact {
			end = c // a compaction drops everything before it
			break
		}
	}
	n := end - i.Call
	if i.Out {
		n--
	}
	return max(n, 0)
}

// ---------- tokenizer ----------

// ponytail: o200k_base is the wrong tokenizer family for Claude and no public
// Claude BPE exists. The systematic bias is why the report prints the
// local/provider ratio instead of pretending the local number is truth.
// The table is embedded via go:embed so nothing is fetched at runtime.
var encoding = sync.OnceValue(func() *tiktoken.Tiktoken {
	tiktoken.SetBpeLoader(tiktoken_loader.NewOfflineLoader())
	enc, err := tiktoken.GetEncoding("o200k_base")
	if err != nil {
		fmt.Fprintf(os.Stderr, "tokenizer: %v\n", err)
		os.Exit(1)
	}
	return enc
})

// countTokens is a var so tests can swap in a cheap offline estimator.
var countTokens = func(s string) int {
	if s == "" {
		return 0
	}
	return len(encoding().Encode(s, nil, nil))
}

// ---------- Claude transcript parsing ----------

type block struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	Thinking string          `json:"thinking"`
	Name     string          `json:"name"`
	Input    json.RawMessage `json:"input"`
	Content  json.RawMessage `json:"content"` // tool_result payload
	Source   struct {
		Data string `json:"data"`
	} `json:"source"`
	ID        string `json:"id"`          // tool_use
	ToolUseID string `json:"tool_use_id"` // tool_result
	IsError   bool   `json:"is_error"`    // tool_result
}

func (b block) tokens() int {
	switch b.Type {
	case "text":
		return countTokens(b.Text)
	case "thinking":
		return countTokens(b.Thinking)
	case "tool_use":
		return countTokens(b.Name + string(b.Input))
	case "tool_result":
		return tokensIn(b.Content)
	case "image":
		return imageTokens(b.Source.Data)
	}
	return 0
}

// imageTokens applies Anthropic's documented (w*h)/750 estimate. Screenshots
// arrive base64-encoded inside tool results and are pure replay afterwards, so
// scoring them zero skews the whole trajectory.
func imageTokens(b64 string) int {
	if b64 == "" {
		return 0
	}
	cfg, _, err := image.DecodeConfig(base64.NewDecoder(base64.StdEncoding, strings.NewReader(b64)))
	if err != nil {
		return 0
	}
	n := (cfg.Width*cfg.Height + 749) / 750
	return min(n, 1600) // provider downsizes anything larger
}

type record struct {
	Type             string          `json:"type"`
	SessionID        string          `json:"sessionId"`
	CWD              string          `json:"cwd"`
	SourceToolUseID  string          `json:"sourceToolUseID"`
	Timestamp        string          `json:"timestamp"`
	IsSidechain      bool            `json:"isSidechain"`
	IsMeta           bool            `json:"isMeta"`
	IsCompactSummary bool            `json:"isCompactSummary"`
	Effort           string          `json:"effort"`
	Attachment       json.RawMessage `json:"attachment"`
	HookContext      json.RawMessage `json:"hookAdditionalContext"`
	TotalCostUSD     float64         `json:"totalCostUSD"` // cost-state
	ModelUsage       map[string]struct {
		CostUSD float64 `json:"costUSD"`
	} `json:"modelUsage"`
	Message struct {
		ID      string          `json:"id"`
		Role    string          `json:"role"`
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
		Usage   *Usage          `json:"usage"`
		Diag    struct {
			Miss struct {
				Type   string `json:"type"`
				Missed int    `json:"cache_missed_input_tokens"`
			} `json:"cache_miss_reason"`
		} `json:"diagnostics"`
	} `json:"message"`
}

// tokensIn counts a value that may be a bare string, an array of content
// blocks, or an arbitrary object.
func tokensIn(raw json.RawMessage) int {
	if len(raw) == 0 || string(raw) == "null" {
		return 0
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return countTokens(s)
	}
	var bs []block
	if json.Unmarshal(raw, &bs) == nil {
		n := 0
		for _, b := range bs {
			n += b.tokens()
		}
		return n
	}
	// ponytail: attachments have ~10 shapes with no common content field; prefer
	// the text-bearing keys, else count the serialized object.
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) == nil {
		for _, k := range []string{"content", "text", "prompt"} {
			if v, ok := obj[k]; ok {
				return tokensIn(v)
			}
		}
	}
	return countTokens(string(raw))
}

// Item kinds. setup is everything the user configured; the rest is traffic.
const (
	snapshot   = "\x00snapshot"
	setup      = "setup"
	toolKind   = "tool"
	promptKind = "prompt"
	outKind    = "output"
)

// attachmentLabel names an attachment by what produced it, so hooks and MCP
// listings can be told apart from each other in the insights report.
func attachmentLabel(raw json.RawMessage) string {
	var a struct {
		Type      string `json:"type"`
		HookName  string `json:"hookName"`
		HookEvent string `json:"hookEvent"`
	}
	if json.Unmarshal(raw, &a) != nil {
		return "attachment"
	}
	switch a.Type {
	case "hook_success", "hook_additional_context":
		if a.HookName != "" {
			return "hook " + a.HookName
		}
		return "hook " + cmp.Or(a.HookEvent, "(unnamed)")
	case "skill_listing":
		return "skill listing"
	case "deferred_tools_delta":
		return "tool listings (MCP)"
	case "mcp_instructions_delta":
		return "MCP server instructions"
	case "agent_listing_delta":
		return "subagent listing"
	case "total_tokens_reminder":
		return "token-usage reminders"
	case "queued_command":
		return "queued commands"
	case "nested_memory":
		return "CLAUDE.md (nested)"
	case "prompt_snapshot":
		return snapshot
	case "":
		return "attachment"
	}
	return strings.ReplaceAll(a.Type, "_", " ")
}

// metaLabel attributes injected context to whatever pulled it in — a skill body
// carries the id of the Skill call that loaded it.
func metaLabel(r record, s *state) string {
	if l := s.tools[r.SourceToolUseID]; l != "" {
		return l + " (loaded)"
	}
	return "session bookkeeping"
}

// mcpServer extracts the server from an mcp__<server>__<tool> name, or "".
func mcpServer(name string) string {
	if parts := strings.Split(name, "__"); len(parts) >= 3 && parts[0] == "mcp" {
		return parts[1]
	}
	return ""
}

// toolLabel names a tool call: MCP tools by their server, Skill calls by the
// skill invoked, everything else by the tool name.
func toolLabel(b block) string {
	if b.Name == "Skill" {
		var in struct {
			Skill string `json:"skill"`
		}
		if json.Unmarshal(b.Input, &in) == nil && in.Skill != "" {
			return "skill " + in.Skill
		}
	}
	if srv := mcpServer(b.Name); srv != "" {
		return "MCP " + srv
	}
	return cmp.Or(b.Name, "tool")
}

// parseClaude reads one ~/.claude/projects/**/*.jsonl transcript. Adding another
// agent means adding another parse function, not an interface.
func parseClaude(data []byte, id string) []Trajectory {
	main, side := &Trajectory{ID: id}, &Trajectory{ID: id + " (subagents)"}
	// ponytail: every sidechain record lands in one subagent trajectory; telling
	// concurrent subagents apart needs parentUuid chain-walking. Add if it matters.
	st := map[*Trajectory]*state{
		main: {tools: map[string]string{}},
		side: {tools: map[string]string{}},
	}

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var r record
		if json.Unmarshal([]byte(line), &r) != nil {
			continue // client bookkeeping we do not model
		}
		t := main
		if r.IsSidechain {
			t = side
		}
		t.apply(st[t], r)
	}

	var out []Trajectory
	for _, t := range []*Trajectory{main, side} {
		if len(t.Calls) > 0 {
			t.replay()
			out = append(out, *t)
		}
	}
	return out
}

type state struct {
	P, T, A   int               // new material pending since the last inference call
	curID     string            // message.id of the call being accumulated
	compacted bool              // a compaction happened; next call resets replay
	failed    string            // label of a tool whose result came back is_error
	tools     map[string]string // tool_use id -> label, to name tool results
}

var commandRe = regexp.MustCompile(`<command-name>(/[^<\s]+)</command-name>`)

// noteCommand counts a slash command by name. The rest of the record is
// what was typed, and is not kept.
func (t *Trajectory) noteCommand(raw json.RawMessage) {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return
	}
	if m := commandRe.FindStringSubmatch(s); m != nil {
		if t.Commands == nil {
			t.Commands = map[string]int{}
		}
		t.Commands[m[1]]++
	}
}

// add records a named contribution as well as its bucket total.
func (t *Trajectory) add(kind, name string, n int, out bool) {
	if n == 0 {
		return
	}
	c := len(t.Calls)
	if out {
		c--
	}
	t.Items = append(t.Items, item{Kind: kind, Name: name, Tok: n, Call: c, Out: out})
}

func (t *Trajectory) apply(s *state, r record) {
	if r.IsCompactSummary {
		s.compacted = true
	}
	if t.CWD == "" && r.CWD != "" {
		t.CWD = r.CWD
	}
	if t.Start.IsZero() && r.Timestamp != "" {
		t.Start, _ = time.Parse(time.RFC3339, r.Timestamp)
	}
	switch r.Type {
	case "cost-state":
		// The ledger total only grows within a session, so the last one wins.
		t.Ledger, t.LedgerModels = r.TotalCostUSD, map[string]float64{}
		for m, u := range r.ModelUsage {
			t.LedgerModels[m] = u.CostUSD
		}
	case "attachment":
		// Handle instructions attachments specially: count each file separately.
		var a struct {
			Type  string `json:"type"`
			Files []struct {
				Type    string `json:"type"`
				Content string `json:"content"`
			} `json:"files"`
		}
		if json.Unmarshal(r.Attachment, &a) == nil && a.Type == "instructions" {
			for _, f := range a.Files {
				n := countTokens(f.Content)
				if n == 0 {
					continue
				}
				label := "memory (" + strings.ToLower(f.Type) + ")"
				switch f.Type {
				case "User":
					label = "CLAUDE.md (user)"
				case "Project":
					label = "CLAUDE.md (project)"
				case "Local":
					label = "CLAUDE.md (local)"
				case "AutoMem":
					label = "memory (auto)"
				}
				s.A += n
				t.add(setup, label, n, false)
			}
			return
		}
		if label := attachmentLabel(r.Attachment); label == snapshot {
			if sc := parseScaffold(r.Attachment); sc != nil && (sc.System > 0 || len(sc.Tools) > 0) {
				t.Scaffold = sc // latest snapshot wins
			}
			return
		}
		n := tokensIn(r.Attachment)
		s.A += n
		t.add(setup, attachmentLabel(r.Attachment), n, false)
		t.noteListing(r.Attachment)
	case "system":
		n := tokensIn(r.HookContext)
		s.A += n
		t.add(setup, "hook output", n, false)
	case "user":
		// tool_result blocks are model input produced by tools, not by the user.
		// toolUseResult on the same record duplicates them; ignore it.
		var bs []block
		if json.Unmarshal(r.Message.Content, &bs) == nil {
			for _, b := range bs {
				n := b.tokens()
				switch {
				case b.Type == "tool_result":
					s.T += n
					t.add(toolKind, cmp.Or(s.tools[b.ToolUseID], "tool"), n, false)
					if b.IsError {
						s.failed = cmp.Or(s.tools[b.ToolUseID], "tool")
					}
				case r.IsMeta || r.IsCompactSummary:
					s.A += n
					t.add(setup, metaLabel(r, s), n, false)
				default:
					s.P += n
					t.add(promptKind, "what you typed", n, false)
				}
			}
			return
		}
		t.noteCommand(r.Message.Content)
		n := tokensIn(r.Message.Content)
		if r.IsMeta || r.IsCompactSummary {
			s.A += n
			t.add(setup, metaLabel(r, s), n, false)
		} else {
			s.P += n
			t.add(promptKind, "what you typed", n, false)
		}
	case "assistant":
		// Claude writes one record per content block, all sharing message.id and
		// the identical usage object. Group by id or both counts inflate.
		if r.Message.ID != s.curID {
			s.curID = r.Message.ID
			c := Call{P: s.P, T: s.T, A: s.A, AfterCompact: s.compacted, Model: r.Message.Model,
				Effort: r.Effort, AfterError: s.failed}
			c.At, _ = time.Parse(time.RFC3339, r.Timestamp)
			if r.Message.Usage != nil {
				c.Provider = *r.Message.Usage
			}
			t.Calls = append(t.Calls, c)
			s.P, s.T, s.A, s.compacted, s.failed = 0, 0, 0, false, ""
			if r.Message.Model != "" && r.Message.Model != "<synthetic>" {
				t.Model = r.Message.Model // latest real model; pricing is per call
			}
		}
		c := &t.Calls[len(t.Calls)-1]
		if m := r.Message.Diag.Miss; m.Type != "" {
			c.Miss, c.Missed = m.Type, m.Missed // on any record of the message, not only the first
		}
		var bs []block
		if json.Unmarshal(r.Message.Content, &bs) != nil {
			return
		}
		for _, b := range bs {
			n := b.tokens()
			switch b.Type {
			case "text":
				c.MText += n
				t.add(outKind, "text you read", n, true)
			case "tool_use":
				c.MTool += n
				label := toolLabel(b)
				s.tools[b.ID] = label
				if srv := mcpServer(b.Name); srv != "" {
					if t.UsedMCP == nil {
						t.UsedMCP = map[string]bool{}
					}
					t.UsedMCP[srv] = true
				}
				t.add(outKind, "asking for "+label, n, true)
			case "thinking":
				c.R += n
				if b.Thinking == "" {
					c.RedactedThinking++
				}
				t.add(outKind, "reasoning", n, true)
			}
		}
	}
}

// replay fills in X: everything sent before this round is resent with it.
// ponytail: assumes nothing is dropped from context between rounds. Providers
// do strip stale reasoning; refine if the residual ever points there.
func (t *Trajectory) replay() {
	carry := 0
	for i := range t.Calls {
		c := &t.Calls[i]
		// Reasoning the provider withheld is unreadable but billed, and it is
		// replayed like everything else. Its own count is the only one there is.
		if c.RedactedThinking > 0 && c.R == 0 {
			c.R = c.Provider.Details.Thinking
			t.Items = append(t.Items, item{Kind: outKind, Name: "reasoning", Tok: c.R, Call: i, Out: true})
		}
		if c.AfterCompact {
			carry = 0 // context is now the summary, not the history
		}
		c.X = carry
		carry += c.NewInput() + c.Output()
	}
}

// ---------- pricing ----------

// Per-million-token list prices, and what a cache read costs as a share of
// input. Cache writes cost 1.25x input at the 5-minute TTL and 2x at one hour.
type rate struct{ In, Out, Read float64 }

var prices = map[string]rate{
	"claude-fable-5-1":  {10, 50, 0.025},
	"claude-mythos-5-1": {10, 50, 0.025},
	"claude-fable-5":    {10, 50, 0.1},
	"claude-opus-5-5":   {4, 20, 0.05},
	"claude-opus-5":     {5, 25, 0.1},
	"claude-opus-4-8":   {5, 25, 0.1},
	"claude-opus-4-7":   {5, 25, 0.1},
	"claude-opus-4-6":   {5, 25, 0.1},
	"claude-sonnet-5":   {2, 10, 0.1},
	"claude-sonnet-4-6": {3, 15, 0.1},
	"claude-haiku-4-5":  {1, 5, 0.1},
}

// rateOf finds a model's price. Ids carry date or context suffixes
// (claude-haiku-4-5-20251001, claude-opus-5[1m]), so the longest table key that
// prefixes the id wins; shortest-first would price opus-5-5 as opus-5.
func rateOf(model string) (rate, bool) {
	r, ok := prices[modelKey(model)]
	return r, ok
}

// modelKey is the price-table key for a model id, or "".
func modelKey(model string) string {
	model, _, _ = strings.Cut(model, "[")
	best := ""
	for k := range prices {
		if strings.HasPrefix(model, k) && len(k) > len(best) {
			best = k
		}
	}
	return best
}

type bill struct {
	FreshIn, CacheWrite, CacheRead, Out float64
	Priced                              bool
}

func (b bill) total() float64 { return b.FreshIn + b.CacheWrite + b.CacheRead + b.Out }
func (b bill) in() float64    { return b.FreshIn + b.CacheWrite + b.CacheRead }

// cacheWrites splits a call's cache creation by TTL.
func (u Usage) cacheWrites() (m5, h1 int) {
	m5, h1 = u.CacheTTL.M5, u.CacheTTL.H1
	if m5+h1 == 0 {
		m5 = u.CacheMake // older transcripts omit the TTL split
	}
	return m5, h1
}

// priceAt prices one call's usage at a model's list rate.
func priceAt(model string, u Usage) bill {
	p, ok := rateOf(model)
	if !ok {
		return bill{}
	}
	in, out := p.In/1e6, p.Out/1e6
	m5, h1 := u.cacheWrites()
	return bill{
		FreshIn:    float64(u.Input) * in,
		CacheWrite: float64(m5)*in*1.25 + float64(h1)*in*2,
		CacheRead:  float64(u.CacheRead) * in * p.Read,
		Out:        float64(u.Output) * out,
		Priced:     true,
	}
}

func (c Call) bill() bill { return priceAt(c.Model, c.Provider) }

func (b *bill) plus(o bill) {
	b.FreshIn += o.FreshIn
	b.CacheWrite += o.CacheWrite
	b.CacheRead += o.CacheRead
	b.Out += o.Out
	b.Priced = b.Priced || o.Priced
}

// billOf is a trajectory's bill, every call at its own model's rate.
func billOf(t Trajectory) bill {
	var b bill
	for _, c := range t.Calls {
		b.plus(c.bill())
	}
	return b
}

// ---------- totals ----------

type totals struct {
	Session            string             `json:"session"`
	ParentSession      string             `json:"parent_session,omitempty"`
	AgentType          string             `json:"agent_type,omitempty"`
	Model              string             `json:"model"`
	Calls              int                `json:"inference_calls"`
	Turns              int                `json:"user_turns"`
	P                  int                `json:"prompt"`
	T                  int                `json:"tool_output"`
	A                  int                `json:"attachment"`
	X                  int                `json:"replay"`
	MText              int                `json:"model_output_text"`
	MTool              int                `json:"model_output_tool_calls"`
	R                  int                `json:"reasoning"`
	Chatting           int                `json:"chatting"`
	AgentVisible       int                `json:"agent_visible"`
	TrajectoryInput    int                `json:"trajectory_input"`
	TrajectoryOutput   int                `json:"trajectory_output"`
	TrajectoryTotal    int                `json:"trajectory_total"`
	ProviderInput      int                `json:"provider_input"`
	ProviderCacheMake  int                `json:"provider_cache_creation"`
	CacheWrite5m       int                `json:"provider_cache_creation_5m"`
	CacheWrite1h       int                `json:"provider_cache_creation_1h"`
	ProviderCacheRead  int                `json:"provider_cache_read"`
	ProviderInputTotal int                `json:"provider_input_total"`
	ProviderOutput     int                `json:"provider_output"`
	ProviderThinking   int                `json:"provider_thinking"`
	RedactedThinking   int                `json:"redacted_thinking_blocks"`
	CallsWithoutUsage  int                `json:"calls_without_usage"`
	CostUSD            float64            `json:"cost_usd"`
	Models             map[string]float64 `json:"models"`             // model -> cost
	Unpriced           map[string]int     `json:"unpriced,omitempty"` // model -> calls not in cost_usd
	HarnessCostUSD     float64            `json:"harness_cost_usd,omitempty"`
	HarnessModels      map[string]float64 `json:"harness_models,omitempty"`
}

func sum(t Trajectory) totals {
	o := totals{Session: t.ID, ParentSession: t.Parent, AgentType: t.Agent, Model: t.Model, Calls: len(t.Calls),
		Models: map[string]float64{}, Unpriced: map[string]int{}, HarnessCostUSD: t.Ledger, HarnessModels: t.LedgerModels}
	for _, c := range t.Calls {
		o.P += c.P
		o.T += c.T
		o.A += c.A
		o.X += c.X
		o.MText += c.MText
		o.MTool += c.MTool
		o.R += c.R
		o.ProviderInput += c.Provider.Input
		o.ProviderCacheMake += c.Provider.CacheMake
		o.ProviderCacheRead += c.Provider.CacheRead
		o.ProviderOutput += c.Provider.Output
		o.ProviderThinking += c.Provider.Details.Thinking
		o.RedactedThinking += c.RedactedThinking
		if c.P > 0 {
			o.Turns++
		}
		m5, h1 := c.Provider.cacheWrites()
		o.CacheWrite5m += m5
		o.CacheWrite1h += h1
		if c.Provider.Empty() {
			o.CallsWithoutUsage++
		}
		b := c.bill()
		o.CostUSD += b.total()
		if !b.Priced && !c.Provider.Empty() && c.Model != "<synthetic>" {
			o.Unpriced[c.Model]++
		} else if b.Priced {
			o.Models[c.Model] += b.total()
		}
	}
	best := 0.0
	for m, v := range o.Models {
		if v > best {
			o.Model, best = m, v // the model that did the most spend
		}
	}
	o.Chatting = o.P + o.MText
	o.AgentVisible = o.P + o.T + o.MText + o.MTool
	o.TrajectoryInput = o.P + o.T + o.A + o.X
	o.TrajectoryOutput = o.MText + o.MTool + o.R
	o.TrajectoryTotal = o.TrajectoryInput + o.TrajectoryOutput
	o.ProviderInputTotal = o.ProviderInput + o.ProviderCacheMake + o.ProviderCacheRead
	return o
}

func comma(n int) string {
	s := fmt.Sprint(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		return "-" + s
	}
	return s
}

func money(v float64) string {
	switch {
	case v == 0:
		return "-"
	case v < 0.01:
		return "<$0.01"
	case v < 10:
		return fmt.Sprintf("$%.2f", v)
	}
	return "$" + comma(int(v+0.5))
}

var rule = "  " + strings.Repeat("─", 68)

// ---------- main ----------

// firstMeta returns the first timestamp and the first cwd a transcript records.
// They do not always share a line — a queue-operation record carries a
// timestamp and no cwd — so each is taken from wherever it first appears. The
// scanner only wants them for a date label and a display name, so this beats
// paying for parseClaude's tokenisation.
func firstMeta(data []byte) (ts time.Time, cwd string) {
	for len(data) > 0 && (ts.IsZero() || cwd == "") {
		var line []byte
		line, data, _ = bytes.Cut(data, []byte("\n"))
		var r struct {
			Timestamp string `json:"timestamp"`
			CWD       string `json:"cwd"`
		}
		if json.Unmarshal(line, &r) != nil {
			continue
		}
		if ts.IsZero() && r.Timestamp != "" {
			ts, _ = time.Parse(time.RFC3339, r.Timestamp)
		}
		if cwd == "" {
			cwd = r.CWD
		}
	}
	return ts, cwd
}

// allProjects is every project directory Claude Code has recorded.
func allProjects() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	dirs, _ := filepath.Glob(filepath.Join(home, ".claude", "projects", "*"))
	return dirs
}

// sessionFiles lists a project directory's transcripts: one per session, plus
// one per subagent run under <session>/subagents/.
func sessionFiles(fsys fs.FS) ([]string, error) {
	top, err := fs.Glob(fsys, "*.jsonl")
	if err != nil {
		return nil, err
	}
	sub, err := fs.Glob(fsys, "*/subagents/*.jsonl")
	return append(top, sub...), err
}

// sessionOf names the session and project a transcript belongs to. A subagent
// run lives at <project>/<session>/subagents/agent-<id>.jsonl, so both sit one
// level further up.
func sessionOf(path string) (session, project string, sub bool) {
	dir := filepath.Dir(path)
	if filepath.Base(dir) == "subagents" {
		sess := filepath.Dir(dir)
		return filepath.Base(sess), filepath.Base(filepath.Dir(sess)), true
	}
	return strings.TrimSuffix(filepath.Base(path), ".jsonl"), filepath.Base(dir), false
}

// agentType reads the agentType from the .meta.json next to a subagent run.
// The file also holds the task description, which is prompt text: not read.
func agentType(path string) string {
	b, err := os.ReadFile(strings.TrimSuffix(path, ".jsonl") + ".meta.json")
	if err != nil {
		return ""
	}
	var m struct {
		AgentType string `json:"agentType"`
	}
	json.Unmarshal(b, &m)
	return m.AgentType
}

func transcripts(args []string) ([]string, error) {
	if len(args) == 0 {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		slug := strings.NewReplacer("/", "-", ".", "-", "_", "-").Replace(cwd)
		args = []string{filepath.Join(home, ".claude", "projects", slug)}
	}
	var files []string
	for _, a := range args {
		fi, err := os.Stat(a)
		if err != nil {
			return nil, err
		}
		if fi.IsDir() {
			m, err := sessionFiles(os.DirFS(a))
			if err != nil {
				return nil, err
			}
			for _, f := range m {
				files = append(files, filepath.Join(a, f))
			}
			continue
		}
		files = append(files, a)
	}
	sort.Strings(files)
	return files, nil
}

func main() {
	hereFlag := flag.Bool("here", false, "only the project in the current directory")
	asJSON := flag.Bool("json", false, "machine-readable totals per session")
	days := flag.Int("days", 1, "how far back to look (0 = all time)")
	scanFlag := flag.Bool("scanner", false, "scan traces for accidentally shared credentials")
	ignoreISS := flag.String("scanner-ignore-iss", "", "comma-separated JWT issuers to suppress (e.g. https://sso.example.com)")
	flag.Parse()

	var args []string
	scope := "all projects"
	if *hereFlag {
		args = nil // transcripts(nil) resolves cwd slug
		scope = "this project"
	} else if rest := flag.Args(); len(rest) > 0 {
		args = rest
		scope = "this project"
	} else {
		args = allProjects()
		if len(args) == 0 {
			fmt.Fprintln(os.Stderr, "no transcripts found (is Claude Code installed and has it been used?)")
			os.Exit(1)
		}
	}

	paths, err := transcripts(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "no transcripts found")
		os.Exit(1)
	}
	if len(paths) == 0 {
		fmt.Fprintln(os.Stderr, "no transcripts found")
		os.Exit(1)
	}

	var ignoreList []string
	if *scanFlag {
		for _, s := range strings.Split(*ignoreISS, ",") {
			if s = strings.TrimSpace(s); s != "" {
				ignoreList = append(ignoreList, s)
			}
		}
	}

	// Compute the look-back window once; the mtime check, scanner, and insights
	// all derive from it.
	since := time.Now().AddDate(0, 0, -*days)
	if *days <= 0 {
		since = time.Time{}
	}

	stopSpinner := spinner("Loading... Classic!")

	var all []Trajectory
	var tfiles []traceFile
	skipped := 0
	for _, f := range paths {
		// ponytail: mtime is the ceiling on a file's newest record — a file
		// unmodified since before the window cannot contain an in-window session.
		// (A file with future-dated records could be wrongly skipped, but that
		// doesn't occur in practice.)
		if !since.IsZero() {
			fi, err := os.Stat(f)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				continue
			}
			if fi.ModTime().Before(since) {
				skipped++
				continue
			}
		}
		data, err := os.ReadFile(f)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			continue
		}
		id, proj, sub := sessionOf(f)
		if *scanFlag {
			ts, cwd := firstMeta(data)
			tfiles = append(tfiles, traceFile{Project: proj, Session: id, Start: ts, CWD: cwd, Path: f, Data: data})
			continue
		}
		trajs := parseClaude(data, id)
		if sub {
			// Every record in a subagent file is a sidechain, so it all parses
			// into one trajectory; name it after its parent and agent type.
			agent := cmp.Or(agentType(f), "subagent")
			for i := range trajs {
				trajs[i].ID, trajs[i].Parent, trajs[i].Agent = id+" · "+agent, id, agent
			}
		}
		all = append(all, trajs...)
	}

	if *scanFlag {
		hits := scanTraces(tfiles, ignoreList)
		stopSpinner()
		printScanReport(hits, tfiles)
		if skipped > 0 {
			fmt.Printf("(%d older file%s skipped; -days 0 to scan everything)\n", skipped, plural(skipped))
		}
		return
	}

	stopSpinner()

	if *asJSON {
		out := make([]totals, 0, len(all))
		for _, t := range all {
			if !t.Start.IsZero() && t.Start.Before(since) {
				continue // same window rule insights applies
			}
			out = append(out, sum(t))
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(out)
		return
	}

	insights(all, since, *days, scope)
}
