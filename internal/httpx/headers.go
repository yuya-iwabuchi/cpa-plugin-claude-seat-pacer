// Package httpx holds the HTTP-map helpers the plugin's packages share. It
// imports nothing else here.
package httpx

import "strings"

// Index is a case-insensitive view of an HTTP header map, holding one value
// per name.
//
// A header map reaches this plugin either canonicalized by net/http or spelled
// however the peer sent it, so every lookup folds case. A name's first
// non-empty value wins and is trimmed; a value that is empty or blank reads as
// absent, so a header the peer sends blank never stands in for one it omitted.
// Two names that differ only in case collapse to one entry, and the spelling
// that sorts first supplies it: a map range visits a request carrying both
// Session-Id and session-id in a different order every time, so without a
// tie-break the key flips per request and affinity loses the conversation.
type Index map[string]string

// NewIndex builds the index for a header map. A nil map yields an empty index.
func NewIndex(headers map[string][]string) Index {
	idx := make(Index, len(headers))
	spelling := make(map[string]string, len(headers))
	for name, values := range headers {
		for _, v := range values {
			if v = strings.TrimSpace(v); v != "" {
				key := strings.ToLower(name)
				if prior, seen := spelling[key]; seen && prior <= name {
					break
				}
				spelling[key], idx[key] = name, v
				break
			}
		}
	}
	return idx
}

// Get is the value for name, empty when the header is absent or blank.
func (i Index) Get(name string) string {
	return i[strings.ToLower(name)]
}

// Lookup is Get with presence reported separately, for a caller that must tell
// an absent header from one it would read as empty anyway.
func (i Index) Lookup(name string) (string, bool) {
	v, ok := i[strings.ToLower(name)]
	return v, ok
}
