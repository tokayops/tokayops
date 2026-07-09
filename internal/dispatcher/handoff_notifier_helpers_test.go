package dispatcher

import "sort"

// staticDmProviders is a tiny dmProviderLookup implementation for tests:
// the slice is the registered "dm"-capable provider set.
type staticDmProviders []string

func (s staticDmProviders) ProvidersSupporting(targetKind string) []string {
	if targetKind != "dm" {
		return nil
	}
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}
