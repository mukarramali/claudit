package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

// makeJWT builds a minimal JWT stub by base64url-encoding the given claims map.
// The header and signature are stubs — scanner functions inspect only the
// payload, so stubs suffice for every test below.
func makeJWT(claims map[string]interface{}) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload, _ := json.Marshal(claims)
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	return header + "." + encoded + ".fakesignature"
}

// ---------- redact ----------

func TestRedactStripsNonPrintable(t *testing.T) {
	// Transcripts can carry raw ESC bytes inside secret-looking values; printing
	// them verbatim is a terminal injection risk.  redact must sanitise first.
	// ESC is placed in the first 8 runes so it lands inside the head of the
	// preview and will survive if printableOnly is ever accidentally skipped.
	s := "\x1b[31mABCDEFGHIJKLMNOPQRST"
	got := redact(s)
	for i, b := range []byte(got) {
		if b == 0x1b {
			t.Errorf("redact left ESC byte at offset %d: %q", i, got)
		}
	}
}

func TestRedactUnicodeSlicing(t *testing.T) {
	// The head (8) and tail (4) must be counted in runes, not bytes.  A naive
	// byte slice of a multi-byte UTF-8 string produces invalid UTF-8.
	// 3 bytes per rune, so a byte slice at 8 and at len-4 both land mid-rune;
	// a 2-byte rune would divide evenly at 8 and let the bug through.
	s := strings.Repeat("\u3042", 20) // 60 bytes, 20 runes
	got := redact(s)
	if !utf8.ValidString(got) {
		t.Errorf("redact produced invalid UTF-8: %q", got)
	}
}

func TestRedactShortValuesFullyStarred(t *testing.T) {
	// Values of ≤ 12 runes must be fully masked; surfacing 8+4 of a 12-char
	// value reveals virtually everything.
	cases := []struct {
		in   string
		want string
	}{
		{"short", "*****"},
		{"twelvechars!", "************"}, // exactly 12 runes → all stars
		{"abc", "***"},
	}
	for _, tc := range cases {
		got := redact(tc.in)
		if got != tc.want {
			t.Errorf("redact(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ---------- isInternalJWT ----------

func TestIsInternalJWT(t *testing.T) {
	cases := []struct {
		name    string
		claims  map[string]interface{}
		wantInt bool
	}{
		{
			// Claude Code embeds its own API key as a JWT whose payload is exactly
			// {"jti":"ApiKey:1"} with no iss, sub, or exp.
			name:    "ApiKey token jti-only",
			claims:  map[string]interface{}{"jti": "ApiKey:1"},
			wantInt: true,
		},
		{
			// Real user tokens carry iss/sub/exp and must never be suppressed.
			name:    "real user token with iss sub exp",
			claims:  map[string]interface{}{"iss": "https://auth.example.com", "sub": "user:42", "exp": 9999999999},
			wantInt: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tok := makeJWT(tc.claims)
			if got := isInternalJWT(tok); got != tc.wantInt {
				t.Errorf("isInternalJWT = %v, want %v", got, tc.wantInt)
			}
		})
	}
}

// ---------- jwtIssuer ----------

func TestJWTIssuer(t *testing.T) {
	iss := "https://accounts.example.com"
	tok := makeJWT(map[string]interface{}{"iss": iss, "sub": "u1"})
	if got := jwtIssuer(tok); got != iss {
		t.Errorf("jwtIssuer = %q, want %q", got, iss)
	}
}

func TestJWTIssuerNonJWT(t *testing.T) {
	// A plain opaque string that is not a JWT must return "".
	if got := jwtIssuer("notajwtatall"); got != "" {
		t.Errorf("jwtIssuer(non-JWT) = %q, want \"\"", got)
	}
}

// ---------- jwtLabel ----------

func TestJWTLabel(t *testing.T) {
	internalTok := makeJWT(map[string]interface{}{"jti": "ApiKey:1"})
	realTok := makeJWT(map[string]interface{}{"iss": "https://auth.example.com", "sub": "u1", "exp": 9999999999})
	ignoredIss := "https://login.corp.example"
	ignoredTok := makeJWT(map[string]interface{}{"iss": ignoredIss, "sub": "u2"})
	trailingSlashTok := makeJWT(map[string]interface{}{"iss": ignoredIss + "/", "sub": "u3"})
	upperCaseTok := makeJWT(map[string]interface{}{"iss": strings.ToUpper(ignoredIss), "sub": "u4"})

	ignoreList := []string{ignoredIss}

	cases := []struct {
		name  string
		base  string
		token string
		want  string
	}{
		{
			// Claude Code's own API key must always be suppressed.
			name:  "internal JWT → skip",
			base:  "JWT",
			token: internalTok,
			want:  "",
		},
		{
			// Exact issuer match against the ignore list.
			name:  "ignored issuer exact → skip",
			base:  "JWT",
			token: ignoredTok,
			want:  "",
		},
		{
			// TrimRight normalises both sides: a trailing slash on the token's
			// issuer must still match an ignore-list entry without one.
			name:  "ignored issuer trailing slash → skip",
			base:  "JWT",
			token: trailingSlashTok,
			want:  "",
		},
		{
			// EqualFold makes the ignore-list comparison case-insensitive.
			name:  "ignored issuer uppercase → skip",
			base:  "JWT",
			token: upperCaseTok,
			want:  "",
		},
		{
			// A token with an issuer NOT in the ignore list must surface with
			// the issuer appended to the label.
			name:  "unlisted issuer → label with iss",
			base:  "JWT",
			token: realTok,
			want:  "JWT (iss: https://auth.example.com)",
		},
		{
			// An opaque non-JWT token has no parseable payload; the base label
			// must be returned unchanged.
			name:  "opaque non-JWT → base label",
			base:  "Bearer Token",
			token: "opaqueOpaqueOpaque1234567890",
			want:  "Bearer Token",
		},
		{
			// Fix for the bug where Bearer-labelled matches bypassed suppression:
			// scanTraces strips "Bearer " before calling jwtLabel, so passing the
			// internal token directly simulates a bearer-wrapped internal API key.
			name:  "Bearer wrapping internal JWT → skip",
			base:  "Bearer Token",
			token: internalTok,
			want:  "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := jwtLabel(tc.base, tc.token, ignoreList)
			if got != tc.want {
				t.Errorf("jwtLabel(%q, ...) = %q, want %q", tc.base, got, tc.want)
			}
		})
	}
}

// ---------- isHighEntropy ----------

func TestIsHighEntropy(t *testing.T) {
	cases := []struct {
		name string
		s    string
		want bool
	}{
		{
			// A 32-char lowercase hex string with good character variety clears
			// the hex threshold (entropy > 3.2, len >= 32).
			name: "32-char hex string",
			s:    "deadbeef1234567890abcdef01234567",
			want: true,
		},
		{
			// English prose falls into charsetGeneral, whose threshold (> 4.8)
			// is well above the ~3.5 bits typical of natural language.
			name: "English prose",
			s:    "the quick brown fox jumps over the",
			want: false,
		},
		{
			// A UUID contains dashes which push it out of charsetHex and into
			// charsetBase64; its actual entropy (~3.4 bits) is below the base64
			// threshold of 4.5, so it returns false despite its length.
			name: "UUID",
			s:    "550e8400-e29b-41d4-a716-446655440000",
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isHighEntropy(tc.s); got != tc.want {
				t.Errorf("isHighEntropy(%q) = %v, want %v", tc.s, got, tc.want)
			}
		})
	}
}

// ---------- scanTraces ----------

func TestScanTraces(t *testing.T) {
	// The key prefixes are assembled rather than written as literals. They are
	// fabricated, but a literal sk_live_ string is exactly what a secret scanner
	// is built to find — GitHub push protection rejects the commit, and this
	// repo's own scanner would flag its own test file. Only the runtime value
	// reaches scanTraces, so the assertions are unaffected.
	secret := "sk_" + "live_" + "ABCDEFGHIJKLMNOPQRSTUVWXYZ" // 26 alphanum after the prefix
	publishable := "pk_" + "live_" + "ABCDEFGHIJKLMNOPQRSTUVWX"
	testKey := "sk_" + "test_" + "ABCDEFGHIJKLMNOPQRSTUVWX"
	data := strings.Join([]string{
		`{"role":"assistant","content":"` + secret + `"}`,
		`{"role":"assistant","content":"` + secret + `"}`, // duplicate → must be deduped
		`{"role":"user","content":"` + publishable + `"}`, // publishable, not a secret → no match
		`{"role":"user","content":"` + testKey + `"}`,     // test key → pattern requires _live_
	}, "\n")

	tf := traceFile{
		Project: "proj",
		Session: "sess1234567890abcdef",
		Data:    []byte(data),
	}
	hits := scanTraces([]traceFile{tf}, nil)

	// Exactly one hit: sk_live_ found, pk_live_ and sk_test_ not found, and
	// the duplicate sk_live_ collapsed to one entry by the dedup map.
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit (deduped sk_live_), got %d: %v", len(hits), hits)
	}
	if hits[0].Label != "Stripe Key" {
		t.Errorf("label = %q, want \"Stripe Key\"", hits[0].Label)
	}
}

// ---------- projectDisplayName ----------

func TestProjectDisplayName(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot determine home directory:", err)
	}

	cases := []struct {
		name string
		slug string
		cwd  string
		want string
	}{
		{
			// A recorded cwd is preferred over the opaque slug.
			name: "prefers cwd over slug",
			slug: "my-project-slug",
			cwd:  home + "/work/myproject",
			want: "~/work/myproject",
		},
		{
			// Falls back to the raw slug when no cwd was recorded.
			name: "falls back to slug when cwd empty",
			slug: "fallback-slug",
			cwd:  "",
			want: "fallback-slug",
		},
		{
			// A cwd outside the home directory is displayed verbatim; this case
			// is home-dir-agnostic because /opt/... never starts with ~ on any
			// supported platform.
			name: "cwd outside home displayed verbatim",
			slug: "external",
			cwd:  "/opt/projects/external",
			want: "/opt/projects/external",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := projectDisplayName(tc.slug, tc.cwd)
			if got != tc.want {
				t.Errorf("projectDisplayName(%q, %q) = %q, want %q", tc.slug, tc.cwd, got, tc.want)
			}
		})
	}
}
