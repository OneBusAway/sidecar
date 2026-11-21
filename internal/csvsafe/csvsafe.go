// Package csvsafe holds the cell formatting every CSV export in this repo
// shares. It exists as its own package because the two exports now live in
// internal/surveys and internal/ghostbus, neither of which may import the
// other -- and two copies of a formula-injection guard is how one of them
// quietly loses a character from its trigger set.
package csvsafe

import (
	"strconv"
	"unicode"
)

// Cell renders a text cell, defusing spreadsheet formula injection.
//
// Excel, Numbers and Sheets treat a leading '=', '+', '-', '@', tab or
// carriage return as the start of a formula, so a rider-supplied comment can
// become executable in whichever spreadsheet an agency opens the export in.
// They also look past leading whitespace and control characters before
// deciding, and some accept the full-width and small-form variants of the
// trigger characters, so the guard skips leading whitespace, control, and
// invisible format characters (a no-break space, a byte-order mark, a
// zero-width space) and checks the first significant character against
// both sets. A single leading apostrophe forces the cell to be read as
// literal text in every one of those tools while leaving the visible value
// -- and a re-import through this same reader -- unchanged for every other
// cell.
func Cell(s string) string {
	if s == "" {
		return s
	}
	if s[0] == '\t' || s[0] == '\r' {
		return "'" + s
	}
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		if isTrigger(r) {
			return "'" + s
		}
		break
	}
	return s
}

// isTrigger reports whether r starts a formula: the ASCII triggers and
// their full-width (U+FF1D, U+FF0B, U+FF0D, U+FF20) and small-form
// (U+FE66, U+FE62, U+FE63, U+FE6B) variants.
func isTrigger(r rune) bool {
	switch r {
	case '=', '+', '-', '@',
		'\uff1d', '\uff0b', '\uff0d', '\uff20',
		'\ufe66', '\ufe62', '\ufe63', '\ufe6b':
		return true
	}
	return false
}

// Float renders an optional float64 cell: blank when absent, else the
// shortest decimal that round-trips exactly.
func Float(v *float64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatFloat(*v, 'f', -1, 64)
}
