package csvsafe_test

import (
	"testing"

	"github.com/OneBusAway/sidecar/internal/csvsafe"
)

// TestCell_GuardsEveryTriggerCharacter pins the full trigger set: Excel,
// Numbers and Sheets all start formula evaluation on one of these six
// leading bytes, so a cell that opens with any of them must come back with
// a leading apostrophe.
func TestCell_GuardsEveryTriggerCharacter(t *testing.T) {
	t.Parallel()

	for _, trigger := range []byte{'=', '+', '-', '@', '\t', '\r'} {
		in := string(trigger) + "cmd|' /C calc'!A0"
		want := "'" + in
		if got := csvsafe.Cell(in); got != want {
			t.Errorf("Cell(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCell_EmptyStringIsUnchanged: an empty cell must stay empty, not
// become a lone apostrophe.
func TestCell_EmptyStringIsUnchanged(t *testing.T) {
	t.Parallel()

	if got := csvsafe.Cell(""); got != "" {
		t.Errorf("Cell(\"\") = %q, want \"\"", got)
	}
}

// TestCell_BenignValuePassesThroughUnchanged.
func TestCell_BenignValuePassesThroughUnchanged(t *testing.T) {
	t.Parallel()

	const in = "Great trip, thanks!"
	if got := csvsafe.Cell(in); got != in {
		t.Errorf("Cell(%q) = %q, want unchanged", in, got)
	}
}

// TestCell_TriggerNotFirstIsUnchanged: the guard only cares about the
// leading byte -- a formula-trigger character anywhere else in the cell is
// not the start of a formula and must not be escaped.
func TestCell_TriggerNotFirstIsUnchanged(t *testing.T) {
	t.Parallel()

	const in = "1 + 1 = 2"
	if got := csvsafe.Cell(in); got != in {
		t.Errorf("Cell(%q) = %q, want unchanged", in, got)
	}
}

// TestFloat_NilIsBlank.
func TestFloat_NilIsBlank(t *testing.T) {
	t.Parallel()

	if got := csvsafe.Float(nil); got != "" {
		t.Errorf("Float(nil) = %q, want \"\"", got)
	}
}

// TestFloat_ZeroRendersAsZero is exactly the bug a naive nil-check-plus-
// truthiness would introduce: a stop latitude or longitude of 0.0 is a
// real, present value (Null Island is a valid coordinate in this data),
// not an absent one, so it must render as "0", not blank.
func TestFloat_ZeroRendersAsZero(t *testing.T) {
	t.Parallel()

	zero := 0.0
	if got := csvsafe.Float(&zero); got != "0" {
		t.Errorf("Float(&0.0) = %q, want %q", got, "0")
	}
}

// TestCell_GuardsTriggerAfterLeadingSpaceOrControl (SC-23): spreadsheets
// trim leading whitespace and skip control characters before deciding a
// cell is a formula, and some accept full-width trigger characters, so the
// guard looks past those to the first significant character.
func TestCell_GuardsTriggerAfterLeadingSpaceOrControl(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		"\n=1+1",
		" =1+1",
		"   +SUM(A1)",
		"\u00a0=1+1",     // no-break space
		"\u3000-1",       // ideographic space
		"\ufeff=1+1",     // byte-order mark
		"\u200b@SUM(A1)", // zero-width space
		"\x00=1",         // NUL
		"\uff1d1+1",      // full-width equals
		"\uff0bcmd",      // full-width plus
		"\uff0dcmd",      // full-width hyphen-minus
		"\uff20SUM(A1)",  // full-width commercial at
		"\ufe66cmd",      // small equals sign
		" \t\n\uff1dHYPERLINK(\"x\")",
	} {
		if got := csvsafe.Cell(in); got != "'"+in {
			t.Errorf("Cell(%q) = %q, want a leading apostrophe", in, got)
		}
	}
	for _, in := range []string{" plain", "\u00a0text", "  1 + 1 = 2", "\n"} {
		if got := csvsafe.Cell(in); got != in {
			t.Errorf("Cell(%q) = %q, want unchanged", in, got)
		}
	}
}
