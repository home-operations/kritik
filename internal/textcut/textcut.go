// Package textcut shortens text without splitting a UTF-8 sequence, so a
// cut string stays valid wherever valid UTF-8 is required, as it is in a
// Postgres text column.
package textcut

import "unicode/utf8"

// Prefix is the longest prefix of s at most n bytes long that ends on a
// rune boundary: s itself when it fits.
func Prefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
