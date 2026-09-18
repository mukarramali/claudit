package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
)

var errOut = os.Stderr

// spinner prints a braille spinner with msg to stderr until the returned stop
// function is called, which erases the line.
func spinner(msg string) func() {
	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	done := make(chan struct{})
	go func() {
		i := 0
		for {
			select {
			case <-done:
				return
			case <-time.After(80 * time.Millisecond):
				fmt.Fprintf(errOut, "\r%s %s", frames[i%len(frames)], msg)
				i++
			}
		}
	}()
	return func() {
		close(done)
		fmt.Fprintf(errOut, "\r%s\r", strings.Repeat(" ", len(msg)+3))
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
}

// traceFile carries the raw bytes plus enough metadata for the scanner.
type traceFile struct {
	Project string
	Session string
	Start   time.Time
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
	{"Private Key", regexp.MustCompile(`-----BEGIN (?:[A-Z]+ )?PRIVATE KEY-----\n[A-Za-z0-9+/=\n]{40,}-----END`)},
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

// charsetEntropy returns the entropy and whether the string is confined to a
// known high-density alphabet (base64 or hex). Confined strings need a lower
// threshold because their theoretical max is already lower than free text.
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

	add := func(hits *[]credential, tf traceFile, label, value string) {
		preview := redact(value)
		k := dedupKey{tf.Session, label, preview}
		if seen[k] {
			return
		}
		seen[k] = true
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

		// Pass 1: known-format patterns.
		for _, cp := range credPatterns {
			for _, m := range cp.Re.FindAllString(text, -1) {
				var label string
				switch cp.Label {
				case "JWT":
					label = jwtLabel(cp.Label, m, ignoreISS)
				case "Bearer Token":
					label = jwtLabel(cp.Label, m[7:], ignoreISS) // strip "Bearer " or "bearer "
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
	}
	return hits
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

	// build a path index: session id → file path
	pathOf := map[string]string{}
	for _, tf := range files {
		pathOf[tf.Session] = tf.Path
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

	fmt.Printf("\n  Credential scan — %d match%s across %d project%s%s\n",
		total, plural(total), len(projOrder), plural(len(projOrder)), rangeStr)
	fmt.Println(rule)

	for _, proj := range projOrder {
		pg := projects[proj]
		fmt.Printf("\n  project  %s\n", projectDisplayName(proj))

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
			fmt.Printf("  session  %s  %s\n  %s\n", sg.session[:8], ts, link)

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
				fmt.Printf("    %-30s", label)
				previews := make([]string, len(creds))
				for i, c := range creds {
					previews[i] = fmt.Sprintf("%s (%dc)", c.Preview, c.Len)
				}
				fmt.Println(strings.Join(previews, "  "))
			}
		}
	}

	fmt.Println()
	fmt.Println(rule)
	fmt.Printf("\n  Action: rotate any non-expired credentials listed above.\n\n")
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// projectDisplayName converts the slug "-Users-mukarram-work-foo" → "~/work/foo".
func projectDisplayName(slug string) string {
	parts := strings.Split(slug, "-")
	for i, p := range parts {
		if p == "work" || p == "Documents" || p == "Desktop" || p == "src" || p == "home" {
			return "~/" + strings.Join(parts[i:], "/")
		}
	}
	return slug
}
