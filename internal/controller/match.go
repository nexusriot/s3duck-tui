package controller

import (
	"fmt"
	"regexp"
	"strings"
)

// Query syntax shared by the in-listing filter, the recursive search and the
// sync exclude list. The mode comes from the query itself, so there is no
// toggle to discover:
//
//	report        substring, case-insensitive
//	*.log         glob, anchored (a full match), case-insensitive
//	re:^2024-\d+  regular expression, case-insensitive unless (?-i) is given
//
// The glob dialect is two metacharacters wide: "*" (any run, including "/")
// and "?" (exactly one character). "*" crossing path separators is the point —
// the search matches full keys, so "*.log" must find "logs/2024/app.log".
// Character classes are left to "re:" rather than half-implemented here.
const reQueryPrefix = "re:"

type matchKind int

const (
	matchSubstring matchKind = iota
	matchGlob
	matchRegex
	// matchNone matches nothing. It is what an unparseable query degrades to,
	// so a half-typed regex in the live filter empties the listing (visibly,
	// with the reason in the title) instead of breaking the render.
	matchNone
)

// matcher is a compiled query. The zero value matches everything, which is
// what an empty filter must do.
type matcher struct {
	raw  string
	kind matchKind
	// lower is the case-folded query for substring/glob comparison.
	lower string
	re    *regexp.Regexp
}

// newMatcher compiles a query. It returns an error only for a malformed
// regular expression; every other input is a valid substring or glob.
func newMatcher(q string) (matcher, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return matcher{}, nil
	}
	if rest, ok := cutPrefixFold(q, reQueryPrefix); ok {
		if rest == "" {
			return matcher{raw: q, kind: matchNone}, fmt.Errorf("empty regular expression")
		}
		// Case-insensitive by default, matching the other two modes. An
		// explicit (?-i) in the pattern still wins — the flag is a prefix, not
		// a wrapper, so the user keeps control.
		re, err := regexp.Compile("(?i)" + rest)
		if err != nil {
			return matcher{raw: q, kind: matchNone}, err
		}
		return matcher{raw: q, kind: matchRegex, re: re}, nil
	}
	if strings.ContainsAny(q, "*?") {
		return matcher{raw: q, kind: matchGlob, lower: strings.ToLower(q)}, nil
	}
	return matcher{raw: q, kind: matchSubstring, lower: strings.ToLower(q)}, nil
}

// mustMatcher compiles a query, degrading a broken one to "matches nothing".
// For the live filter, where every keystroke is a query and half of them are
// incomplete.
func mustMatcher(q string) matcher {
	m, err := newMatcher(q)
	if err != nil {
		return matcher{raw: q, kind: matchNone}
	}
	return m
}

// cutPrefixFold is strings.CutPrefix with a case-insensitive prefix test.
func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return s, false
	}
	return s[len(prefix):], true
}

// empty reports whether this matcher accepts everything (no query).
func (m matcher) empty() bool { return m.kind == matchSubstring && m.lower == "" }

// broken reports whether the query failed to compile.
func (m matcher) broken() bool { return m.kind == matchNone }

// match tests one candidate string.
func (m matcher) match(s string) bool {
	switch m.kind {
	case matchNone:
		return false
	case matchRegex:
		return m.re.MatchString(s)
	case matchGlob:
		return globMatch(m.lower, strings.ToLower(s))
	default:
		if m.lower == "" {
			return true
		}
		return strings.Contains(strings.ToLower(s), m.lower)
	}
}

// globMatch reports whether s matches pattern in full. Both arguments are
// expected pre-folded by the caller.
//
// The algorithm is the classic two-cursor scan with one backtrack point rather
// than recursion: a pattern like "*a*b*c" over a long key would otherwise
// branch exponentially, and keys here come from listings that can hold a
// hundred thousand of them.
func globMatch(pattern, s string) bool {
	p := []rune(pattern)
	str := []rune(s)
	var pi, si int
	star, backtrack := -1, 0
	for si < len(str) {
		switch {
		case pi < len(p) && (p[pi] == '?' || p[pi] == str[si]):
			pi++
			si++
		case pi < len(p) && p[pi] == '*':
			star = pi
			pi++
			backtrack = si
		case star >= 0:
			// Mismatch, but an earlier "*" can absorb one more character.
			pi = star + 1
			backtrack++
			si = backtrack
		default:
			return false
		}
	}
	// Trailing "*"s in the pattern are free; anything else is unmatched.
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}

// matchLabel names the mode for the UI, so a user who typed a glob can see
// that it was read as one.
func (m matcher) matchLabel() string {
	switch m.kind {
	case matchRegex:
		return "regex"
	case matchGlob:
		return "glob"
	case matchNone:
		return "bad pattern"
	default:
		return "text"
	}
}
