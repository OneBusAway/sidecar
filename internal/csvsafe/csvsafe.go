// Package csvsafe holds the cell formatting every CSV export in this repo
// shares. It exists as its own package because the two exports now live in
// internal/surveys and internal/ghostbus, neither of which may import the
// other -- and two copies of a formula-injection guard is how one of them
// quietly loses a character from its trigger set.
package csvsafe

import "strconv"

// Cell renders a text cell, defusing spreadsheet formula injection.
//
// Excel, Numbers and Sheets treat a leading '=', '+', '-', '@', tab or
// carriage return as the start of a formula, so a rider-supplied comment can
// become executable in whichever spreadsheet an agency opens the export in.
// A single leading apostrophe forces the cell to be read as literal text in
// every one of those tools while leaving the visible value -- and a
// re-import through this same reader -- unchanged for every other cell.
func Cell(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	default:
		return s
	}
}

// Float renders an optional float64 cell: blank when absent, else the
// shortest decimal that round-trips exactly.
func Float(v *float64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatFloat(*v, 'f', -1, 64)
}
