package providers

import (
	"net/url"
	"strings"
	"unicode"
)

// Linkable reports whether raw may be the target of a link a person follows
// out of an alert: a dashboard, a runbook, or Alertmanager's own URL.
//
// The raw string is judged before it is parsed. A browser rewrites what it is
// given: it drops tabs and newlines, it reads "\" as "/", and for http and
// https it accepts forms this check does not, such as a scheme with no "//"
// or an empty authority. Go's parser does not do those rewrites, so a check
// that only parsed would let one channel link an address another refuses.
// Slack has a second reason to look at the raw string. A link is written
// <url|label>, and a ">", a "<" or a "|" ends it early; whatever follows is
// read as markup.
//
// An address that fails is left out. Escaping it instead would print a broken
// address, and a link that is not one is worth less than no link.
func Linkable(raw string) bool {
	if raw == "" || strings.ContainsAny(raw, "<>|\\") || strings.ContainsFunc(raw, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) {
		return false
	}
	if !httpForm(raw) {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// httpForm reports whether raw is http:// or https://, then a non-empty
// authority, then optionally a path, query or fragment. Authority is the run
// of characters after "://" and before the first "/", "?" or "#".
func httpForm(raw string) bool {
	rest, ok := strings.CutPrefix(raw, "https://")
	if !ok {
		rest, ok = strings.CutPrefix(raw, "http://")
	}
	if !ok || rest == "" {
		return false
	}
	authority := rest
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		authority = rest[:i]
	}
	return authority != ""
}
