package scom

import (
	"regexp"
	"slices"
	"strings"
)

// searchMatcher is searchLikePattern's in-memory twin, for lists the plugin
// caches and filters itself instead of searching in SQL: a case-insensitive
// contains-match where '*' matches any run of characters and '?' any single
// character. Every other character is literal.
func searchMatcher(search string) func(string) bool {
	if search == "" {
		return func(string) bool { return true }
	}
	var b strings.Builder
	b.WriteString("(?is)")
	for _, r := range search {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	return regexp.MustCompile(b.String()).MatchString
}

// searchRank orders matches for display: an exact match first, then a
// prefix match, then a match anywhere.
func searchRank(search string, values ...string) int {
	s := strings.ToLower(search)
	best := 2
	for _, v := range values {
		v = strings.ToLower(v)
		switch {
		case v == s:
			return 0
		case strings.HasPrefix(v, s):
			best = 1
		}
	}
	return best
}

// searchOptions filters a cached option list the way the SQL pickers search
// (see searchMatcher), matching on the label or the description. Exact and
// prefix matches come first; within each tier the list keeps its original
// (alphabetical) order. limit caps the result; 0 means no cap. The input is
// never modified.
func searchOptions(opts []Option, search string, limit int) []Option {
	match := searchMatcher(search)
	out := make([]Option, 0)
	for _, o := range opts {
		if match(o.Label) || (o.Description != "" && match(o.Description)) {
			out = append(out, o)
		}
	}
	if search != "" {
		slices.SortStableFunc(out, func(a, b Option) int {
			return searchRank(search, a.Label, a.Description) - searchRank(search, b.Label, b.Description)
		})
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}
