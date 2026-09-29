package main

import (
	"bytes"
	"cmp"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
)

// spinner prints a braille spinner with msg to stderr until the returned stop
// function is called, which erases the line. No-op when stderr is not a terminal.
func spinner(msg string) func() {
	fi, err := os.Stderr.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return func() {} // not a terminal: stay out of redirected output
	}
	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		i := 0
		for {
			select {
			case <-done:
				return
			case <-time.After(80 * time.Millisecond):
				fmt.Fprintf(os.Stderr, "\r%s %s", frames[i%len(frames)], msg)
				i++
			}
		}
	}()
	return func() {
		close(done)
		<-stopped
		fmt.Fprintf(os.Stderr, "\r%s\r", strings.Repeat(" ", len(msg)+3))
	}
}

// credential is a single pattern hit inside a trace file.
type credential struct {
	Project string
	Session string
	Start   time.Time
	Label   string
	Preview string // first 8 chars … last 4 chars
	Len     int
	Source  string // how it got in: "you typed/pasted it", "Read .env", …
	Dest    string // where it went out again, if it did
}

// traceFile carries the raw bytes plus enough metadata for the scanner.
type traceFile struct {
	Project string
	Session string
	Start   time.Time
	CWD     string
	Path    string // absolute path to the .jsonl file
	Data    []byte
}

// printableOnly replaces non-printable runes with '.' so previews and labels
// cannot inject terminal escape sequences.
func printableOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsPrint(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte('.')
		}
	}
	return b.String()
}

// fileLink returns an OSC 8 terminal hyperlink for the trace file.
// Terminals that don't support it render the plain path.
func fileLink(path string) string {
	u := (&url.URL{Scheme: "file", Path: path}).String()
	label := printableOnly(path)
	return fmt.Sprintf("\x1b]8;;%s\x1b\\%s\x1b]8;;\x1b\\", u, label)
}

// ---------- known-format patterns ----------

// credPattern catches secrets with a fixed recognisable shape (prefix, structure).
type credPattern struct {
	Label string
	Re    *regexp.Regexp
}

var credPatterns = []credPattern{
	{"Private Key", regexp.MustCompile(`-----BEGIN (?:[A-Z]+ )?PRIVATE KEY-----(?:\\n|\n)(?:[A-Za-z0-9+/=]|\\n|\n){40,}-----END`)},
	{"AWS access key", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{"OpenAI project key", regexp.MustCompile(`sk-proj-[A-Za-z0-9_-]{40,}`)},
	{"GitHub fine-grained", regexp.MustCompile(`github_pat_[A-Za-z0-9_]{60,}`)},
	{"Slack token", regexp.MustCompile(`xox[abposr]-[A-Za-z0-9-]{10,}`)},
	{"Google API key", regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}`)},
	{"GitLab PAT", regexp.MustCompile(`glpat-[A-Za-z0-9_-]{20,}`)},
	{"npm token", regexp.MustCompile(`\bnpm_[A-Za-z0-9]{36}\b`)},
	{"Atlassian API token", regexp.MustCompile(`ATATT[A-Za-z0-9_=-]{50,}`)},
	{"GitHub Token", regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{36,}`)},
	{"Anthropic Key", regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{40,}`)},
	{"OpenAI Key", regexp.MustCompile(`sk-[A-Za-z0-9]{48,}`)},
	{"Stripe Key", regexp.MustCompile(`(?:sk|rk)_live_[A-Za-z0-9]{24,}`)},
	{"JWT", regexp.MustCompile(`eyJ[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`)},
	{"DB Connection URL", regexp.MustCompile(`(?:postgres|mysql|mongodb|redis)://[^:"\s]+:[^@"'\s]+@[^"'\s]+`)},
	{"Bearer Token", regexp.MustCompile(`[Bb]earer [A-Za-z0-9_.-]{20,}`)},
}

// ---------- entropy-based detection ----------

// secretKeyRe matches JSON key names that suggest the value is a secret.
var secretKeyRe = regexp.MustCompile(`(?i)(password|passwd|secret|token|api[_-]?key|auth|credential|private[_-]?key|access[_-]?key|api[_-]?secret|client[_-]?secret)`)

// keyValueRe extracts "key": "value" pairs from JSON text.
// Min value length 16 to skip obviously short non-secrets.
var keyValueRe = regexp.MustCompile(`"([^"]{2,60})"\s*:\s*"([^"]{16,})"`)

// shannonEntropy returns bits-per-character for s.
func shannonEntropy(s string) float64 {
	if len(s) == 0 {
		return 0
	}
	freq := map[rune]float64{}
	for _, c := range s {
		freq[c]++
	}
	n := float64(len([]rune(s)))
	var h float64
	for _, f := range freq {
		p := f / n
		h -= p * math.Log2(p)
	}
	return h
}

// charsetKind tags a string's alphabet. Confined sets (hex, base64) have a
// lower theoretical entropy ceiling than free text, so isHighEntropy applies
// a tighter threshold to them.
type charsetKind int

const (
	charsetGeneral charsetKind = iota
	charsetHex
	charsetBase64
)

func classifyCharset(s string) charsetKind {
	hex, b64 := true, true
	b64chars := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/=-_"
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			hex = false
		}
		if !strings.ContainsRune(b64chars, c) {
			b64 = false
		}
	}
	switch {
	case hex:
		return charsetHex
	case b64:
		return charsetBase64
	default:
		return charsetGeneral
	}
}

// isHighEntropy returns true if s looks like a secret based on entropy alone.
// Thresholds tuned to minimise false positives on UUIDs and English prose.
func isHighEntropy(s string) bool {
	if len(s) < 16 {
		return false
	}
	e := shannonEntropy(s)
	switch classifyCharset(s) {
	case charsetHex:
		return e > 3.2 && len(s) >= 32 // hex secrets are long; UUIDs pass entropy but fail length with dashes
	case charsetBase64:
		return e > 4.5
	default:
		return e > 4.8
	}
}

// scanEntropy finds "key": "value" pairs where the key name looks secret-like
// and the value has high entropy.
func scanEntropy(text string) []struct{ label, value string } {
	var out []struct{ label, value string }
	for _, m := range keyValueRe.FindAllStringSubmatch(text, -1) {
		key, val := m[1], m[2]
		if !secretKeyRe.MatchString(key) {
			continue
		}
		if !isHighEntropy(val) {
			continue
		}
		out = append(out, struct{ label, value string }{"high-entropy " + key, val})
	}
	return out
}

// envStyleRe matches FOO_TOKEN=value assignments, as they appear in Bash
// output and .env contents, which the JSON key/value pass cannot see.
var envStyleRe = regexp.MustCompile(`\b[A-Z0-9_]*(?:SECRET|TOKEN|PASSWORD|PASSWD|API_?KEY|ACCESS_?KEY|PRIVATE_?KEY)[A-Z0-9_]*\s*=\s*['"]?([A-Za-z0-9+/_=-]{20,})`)

var placeholderRe = regexp.MustCompile(`(?i)example|xxxx|your|dummy|test|…|placeholder|changeme`)

// scanEnvStyle reports an assigned value only when it is random-looking: high
// entropy, both letters and digits, and no placeholder words. The raw regex
// alone hits hundreds of config lines.
func scanEnvStyle(text string) []struct{ label, value string } {
	var out []struct{ label, value string }
	for _, m := range envStyleRe.FindAllStringSubmatch(text, -1) {
		v := m[1]
		if !isHighEntropy(v) || !strings.ContainsAny(v, "0123456789") ||
			strings.IndexFunc(v, unicode.IsLetter) < 0 || placeholderRe.MatchString(v) {
			continue
		}
		out = append(out, struct{ label, value string }{"env-style secret", v})
	}
	return out
}

// ---------- JWT enrichment ----------

// jwtPayload decodes the payload claims of a JWT, or nil on failure.
func jwtPayload(token string) map[string]json.RawMessage {
	parts := strings.SplitN(token, ".", 3)
	if len(parts) < 2 {
		return nil
	}
	raw := parts[1]
	if n := len(raw) % 4; n != 0 {
		raw += strings.Repeat("=", 4-n)
	}
	b, err := base64.URLEncoding.DecodeString(raw)
	if err != nil {
		return nil
	}
	var claims map[string]json.RawMessage
	if json.Unmarshal(b, &claims) != nil {
		return nil
	}
	return claims
}

// isInternalJWT returns true for JWTs that belong to Claude Code itself rather
// than being user credentials — specifically tokens whose payload contains only
// a jti claim (no iss/sub/exp), which is the shape of Anthropic's API key token
// embedded in every trace.
func isInternalJWT(token string) bool {
	claims := jwtPayload(token)
	if claims == nil {
		return false
	}
	_, hasJti := claims["jti"]
	_, hasIss := claims["iss"]
	_, hasSub := claims["sub"]
	_, hasExp := claims["exp"]
	return hasJti && !hasIss && !hasSub && !hasExp
}

func jwtIssuer(token string) string {
	claims := jwtPayload(token)
	if claims == nil {
		return ""
	}
	var iss string
	if v, ok := claims["iss"]; ok {
		json.Unmarshal(v, &iss)
	}
	return iss
}

// jwtLabel returns the label to report a JWT-bearing match under, or "" to
// skip it: Claude Code embeds its own API key as a JWT in every trace, and
// -scanner-ignore-iss suppresses whole issuers.
func jwtLabel(base, token string, ignoreISS []string) string {
	if isInternalJWT(token) {
		return ""
	}
	iss := jwtIssuer(token)
	for _, ig := range ignoreISS {
		if strings.EqualFold(strings.TrimRight(iss, "/"), strings.TrimRight(ig, "/")) {
			return ""
		}
	}
	if iss != "" {
		return fmt.Sprintf("%s (iss: %s)", base, iss)
	}
	return base
}

// ---------- scanner entry point ----------

func scanTraces(files []traceFile, ignoreISS []string) []credential {
	type dedupKey struct{ session, label, preview string }
	seen := map[dedupKey]bool{}

	var values []string // raw value of each hit in this file, for provenance
	add := func(hits *[]credential, tf traceFile, label, value string) {
		preview := redact(value)
		k := dedupKey{tf.Session, label, preview}
		if seen[k] {
			return
		}
		seen[k] = true
		values = append(values, value)
		*hits = append(*hits, credential{
			Project: tf.Project,
			Session: tf.Session,
			Start:   tf.Start,
			Label:   label,
			Preview: preview,
			Len:     len(value),
		})
	}

	var hits []credential
	for _, tf := range files {
		text := string(tf.Data)
		first := len(hits)
		values = values[:0]

		// Pass 1: known-format patterns.
		for _, cp := range credPatterns {
			for _, m := range cp.Re.FindAllString(text, -1) {
				var label string
				switch cp.Label {
				case "JWT":
					label = jwtLabel(cp.Label, m, ignoreISS)
				case "Bearer Token":
					label = jwtLabel(cp.Label, m[7:], ignoreISS) // strip "Bearer " or "bearer "
				case "OpenAI Key":
					if strings.HasPrefix(m, "sk-proj-") {
						continue // reported as an OpenAI project key
					}
					label = cp.Label
				default:
					label = cp.Label
				}
				if label == "" {
					continue
				}
				add(&hits, tf, label, m)
			}
		}

		// Pass 2: entropy — catches secrets with no recognisable prefix.
		for _, e := range scanEntropy(text) {
			add(&hits, tf, e.label, e.value)
		}

		// Pass 3: env-style assignments.
		for _, e := range scanEnvStyle(text) {
			add(&hits, tf, e.label, e.value)
		}

		if len(values) > 0 {
			prov := provenance(tf, values)
			for i, v := range values {
				hits[first+i].Source, hits[first+i].Dest = prov[v][0], prov[v][1]
			}
		}
	}
	return hits
}

// provenance says, for each value, how it first entered the file and whether
// it later went out in a tool call to an MCP server or a WebFetch URL. It
// reads records only for files that had a hit: one JSON decode per line, no
// tokenising.
func provenance(tf traceFile, values []string) map[string][2]string {
	out := map[string][2]string{}
	switch base := filepath.Base(filepath.Dir(tf.Path)); {
	case base == "tool-results":
		return fill(out, values, "large tool output")
	case base == "paste-cache":
		return fill(out, values, "you pasted it")
	case filepath.Base(filepath.Dir(filepath.Dir(tf.Path))) == "file-history":
		return fill(out, values, "edit backup")
	}
	tools := map[string]block{}
	for _, line := range bytes.Split(tf.Data, []byte("\n")) {
		var r record
		if json.Unmarshal(line, &r) != nil {
			continue
		}
		var bs []block
		json.Unmarshal(r.Message.Content, &bs)
		for _, b := range bs {
			if b.Type == "tool_use" {
				tools[b.ID] = b
			}
		}
		for _, v := range values {
			if !bytes.Contains(line, []byte(v)) {
				continue
			}
			p := out[v]
			if p[0] == "" {
				p[0] = sourceOf(r, bs, v, tools)
			}
			for _, b := range bs {
				if p[1] == "" && b.Type == "tool_use" && bytes.Contains(b.Input, []byte(v)) {
					if srv := mcpServer(b.Name); srv != "" {
						p[1] = "sent to MCP " + srv
					} else if b.Name == "WebFetch" {
						p[1] = "sent in a WebFetch URL"
					}
				}
			}
			out[v] = p
		}
	}
	return out
}

func fill(out map[string][2]string, values []string, source string) map[string][2]string {
	for _, v := range values {
		out[v] = [2]string{source, ""}
	}
	return out
}

// sourceOf names what put a value into the record it first appears in. A
// command line or file path can carry secrets of its own, so only the tool,
// the file's extension or the command's first word are named.
func sourceOf(r record, bs []block, v string, tools map[string]block) string {
	switch r.Type {
	case "attachment":
		return "attachment"
	case "assistant":
		return "Claude wrote it"
	case "user":
		if len(bs) == 0 {
			if r.IsMeta {
				return "attachment"
			}
			return "you typed/pasted it"
		}
		for _, b := range bs {
			if b.Type == "tool_result" && bytes.Contains(b.Content, []byte(v)) {
				return toolSource(tools[b.ToolUseID])
			}
		}
		return "you typed/pasted it"
	}
	return "transcript"
}

func toolSource(b block) string {
	var in struct {
		Path    string `json:"file_path"`
		Command string `json:"command"`
	}
	json.Unmarshal(b.Input, &in)
	switch {
	case mcpServer(b.Name) != "":
		return "MCP " + mcpServer(b.Name) + " result"
	case b.Name == "Read" && in.Path != "":
		return "Read " + cmp.Or(filepath.Ext(in.Path), filepath.Base(in.Path))
	case b.Name == "Bash":
		if w := strings.Fields(in.Command); len(w) > 0 && !strings.ContainsAny(w[0], "=$`'\"") {
			return "Bash " + filepath.Base(w[0])
		}
		return "Bash"
	}
	return cmp.Or(b.Name, "a tool") + " result"
}

// ---------- output ----------

func redact(s string) string {
	r := []rune(printableOnly(s))
	if len(r) <= 12 {
		return strings.Repeat("*", len(r))
	}
	return string(r[:8]) + "…" + string(r[len(r)-4:])
}

func printScanReport(hits []credential, files []traceFile) {
	var earliest, latest time.Time
	for _, tf := range files {
		if tf.Start.IsZero() {
			continue
		}
		if earliest.IsZero() || tf.Start.Before(earliest) {
			earliest = tf.Start
		}
		if tf.Start.After(latest) {
			latest = tf.Start
		}
	}
	days := 0
	if !earliest.IsZero() {
		days = int(latest.Sub(earliest).Hours()/24) + 1
	}
	rangeStr := ""
	if !earliest.IsZero() {
		rangeStr = fmt.Sprintf(" · %s – %s (%d day%s)",
			earliest.Format("2006-01-02"), latest.Format("2006-01-02"), days, plural(days))
	}

	if len(hits) == 0 {
		fmt.Printf("No credential patterns found%s.\n", rangeStr)
		return
	}

	// build indexes: session id → file path, project slug → cwd
	pathOf := map[string]string{}
	cwdOf := map[string]string{}
	for _, tf := range files {
		pathOf[tf.Session] = tf.Path
		if tf.CWD != "" && cwdOf[tf.Project] == "" {
			cwdOf[tf.Project] = tf.CWD
		}
	}

	type sessionGroup struct {
		session string
		start   time.Time
		byLabel map[string][]credential
	}
	type projectGroup struct {
		sessions map[string]*sessionGroup
		order    []string
	}

	projects := map[string]*projectGroup{}
	var projOrder []string

	for _, h := range hits {
		pg, ok := projects[h.Project]
		if !ok {
			pg = &projectGroup{sessions: map[string]*sessionGroup{}}
			projects[h.Project] = pg
			projOrder = append(projOrder, h.Project)
		}
		sg, ok := pg.sessions[h.Session]
		if !ok {
			sg = &sessionGroup{session: h.Session, start: h.Start, byLabel: map[string][]credential{}}
			pg.sessions[h.Session] = sg
			pg.order = append(pg.order, h.Session)
		}
		sg.byLabel[h.Label] = append(sg.byLabel[h.Label], h)
	}

	sort.Strings(projOrder)
	total := len(hits)

	matches := "matches"
	if total == 1 {
		matches = "match"
	}
	fmt.Printf("\n  Credential scan — %d %s across %d project%s%s\n",
		total, matches, len(projOrder), plural(len(projOrder)), rangeStr)
	fmt.Println(rule)

	for _, proj := range projOrder {
		pg := projects[proj]
		fmt.Printf("\n  project  %s\n", projectDisplayName(proj, cwdOf[proj]))

		sort.Slice(pg.order, func(i, j int) bool {
			return pg.sessions[pg.order[i]].start.Before(pg.sessions[pg.order[j]].start)
		})

		for _, sess := range pg.order {
			sg := pg.sessions[sess]
			ts := ""
			if !sg.start.IsZero() {
				ts = sg.start.Format("2006-01-02")
			}
			link := fileLink(pathOf[sg.session])
			fmt.Printf("  session  %s  %s\n  %s\n", sg.session[:min(len(sg.session), 8)], ts, link)

			labels := make([]string, 0, len(sg.byLabel))
			for l := range sg.byLabel {
				labels = append(labels, l)
			}
			sort.Strings(labels)

			for _, label := range labels {
				creds := sg.byLabel[label]
				if len(creds) > 3 {
					creds = creds[:3]
				}
				for i, c := range creds {
					if i > 0 {
						label = ""
					}
					from := printableOnly(c.Source)
					if c.Dest != "" {
						from += " → " + printableOnly(c.Dest)
					}
					fmt.Printf("    %-30s %s (%dc)  %s\n", label, c.Preview, c.Len, from)
				}
			}
		}
	}

	fmt.Println()
	fmt.Println(rule)
	fmt.Printf("\n  Action: rotate any non-expired credentials listed above.\n")
	for _, h := range hits {
		if strings.HasPrefix(h.Source, "Read .env") {
			fmt.Printf("  A .env file was read: add \"Read(**/.env)\" to permissions.deny;\n" +
				"  it also blocks cat, head and tail on that path.\n")
			break
		}
	}
	fmt.Printf("  The matched files keep the secret until cleanupPeriodDays\n" +
		"  (default 30). Delete them after rotating; claudit deletes nothing.\n\n")
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// projectDisplayName prefers the cwd the transcript recorded. The directory
// slug cannot be reversed — Claude Code maps /, . and _ all to - — so it is
// only a fallback.
func projectDisplayName(slug, cwd string) string {
	if cwd == "" {
		return slug
	}
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(cwd, home) {
		return "~" + strings.TrimPrefix(cwd, home)
	}
	return cwd
}

// ---------- personal data ----------
//
// Counts only. Nothing from a match — not a digit, not a redacted preview —
// reaches the output.

// validIBAN checks ISO 13616: country letters, then mod 97 == 1 over the
// rearranged string with letters as 10..35.
func validIBAN(s string) bool {
	s = strings.ToUpper(strings.ReplaceAll(s, " ", ""))
	if len(s) < 15 || len(s) > 34 || s[0] < 'A' || s[0] > 'Z' || s[1] < 'A' || s[1] > 'Z' {
		return false
	}
	mod := 0
	for _, c := range s[4:] + s[:4] {
		switch {
		case c >= '0' && c <= '9':
			mod = (mod*10 + int(c-'0')) % 97
		case c >= 'A' && c <= 'Z':
			mod = (mod*100 + int(c-'A') + 10) % 97
		default:
			return false
		}
	}
	return mod == 1
}

// validSteuerID checks a German tax id: 11 digits, no leading 0, in the first
// ten exactly one digit repeats (2 or 3 times), and an ISO 7064 MOD 11,10
// check digit.
func validSteuerID(s string) bool {
	if len(s) != 11 || s[0] == '0' || strings.Trim(s, "0123456789") != "" {
		return false
	}
	var count [10]int
	repeats := 0
	for _, c := range s[:10] {
		if count[c-'0']++; count[c-'0'] == 2 {
			repeats++
		} else if count[c-'0'] > 3 {
			return false
		}
	}
	product := 10
	for _, c := range s[:10] {
		sum := (int(c-'0') + product) % 10
		if sum == 0 {
			sum = 10
		}
		product = sum * 2 % 11
	}
	return repeats == 1 && (11-product)%10 == int(s[10]-'0')
}

// cardIIN is the issuer prefix of the card networks. Random digit runs pass
// Luhn one time in ten — epoch-millisecond timestamps are 13 digits — so a
// number must also start like a real card.
var cardIIN = regexp.MustCompile(`^(?:4|5[1-5]|2[2-7]|3[47]|6(?:011|5))`)

// validCard checks a 13-19 digit number against an issuer prefix and Luhn.
func validCard(s string) bool {
	s = strings.NewReplacer(" ", "", "-", "").Replace(s)
	if len(s) < 13 || len(s) > 19 || strings.Trim(s, "0123456789") != "" || !cardIIN.MatchString(s) {
		return false
	}
	sum := 0
	for i := range len(s) {
		d := int(s[len(s)-1-i] - '0')
		if i%2 == 1 {
			if d *= 2; d > 9 {
				d -= 9
			}
		}
		sum += d
	}
	return sum%10 == 0
}

// publicExamples are the documented sample numbers every test suite uses.
var publicExamples = map[string]bool{"DE89370400440532013000": true, "4111111111111111": true, "86095742719": true}

var personalPatterns = []struct {
	kind  string
	re    *regexp.Regexp
	valid func(string) bool
}{
	{"IBAN", regexp.MustCompile(`\b[A-Z]{2}\d{2}(?: ?[A-Z0-9]{4}){2,7}(?: ?[A-Z0-9]{1,4})?\b`), validIBAN},
	{"tax id", regexp.MustCompile(`\b[1-9]\d{10}\b`), validSteuerID},
	{"card", regexp.MustCompile(`\b(?:\d[ -]?){12,18}\d\b`), validCard},
}

// scanPersonal counts distinct checksum-valid personal numbers per session.
// ponytail: counts by kind only; by source (local tool vs MCP server) needs the
// per-record scan in docs/plan-next.md 3.4.
func scanPersonal(files []traceFile) map[string]map[string]int {
	out := map[string]map[string]int{}
	seen := map[string]bool{}
	for _, tf := range files {
		text := string(tf.Data)
		for _, p := range personalPatterns {
			for _, m := range p.re.FindAllString(text, -1) {
				norm := strings.NewReplacer(" ", "", "-", "").Replace(m)
				key := tf.Session + "\x00" + norm
				if seen[key] || publicExamples[norm] || !p.valid(m) {
					continue
				}
				seen[key] = true
				if out[tf.Session] == nil {
					out[tf.Session] = map[string]int{}
				}
				out[tf.Session][p.kind]++
			}
		}
	}
	return out
}

// personalLines renders the counts, one line per session.
func personalLines(counts map[string]map[string]int) []string {
	var out []string
	for _, sess := range slices.Sorted(maps.Keys(counts)) {
		var parts []string
		for _, kind := range slices.Sorted(maps.Keys(counts[sess])) {
			parts = append(parts, fmt.Sprintf("%s %d", kind, counts[sess][kind]))
		}
		out = append(out, fmt.Sprintf("  session  %s  %s", sess[:min(len(sess), 8)], strings.Join(parts, " · ")))
	}
	return out
}
