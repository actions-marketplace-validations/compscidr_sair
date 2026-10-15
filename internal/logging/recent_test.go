package logging

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestRingKeepsTheLastLinesInOrderAndFilters(t *testing.T) {
	r := &Ring{max: 3}
	for i := range 5 {
		fmt.Fprintf(r, "line %d serial=%s\n", i, []string{"A", "B"}[i%2])
	}
	if got, want := r.Lines(), []string{"line 2 serial=A", "line 3 serial=B", "line 4 serial=A"}; !slices.Equal(got, want) {
		t.Fatalf("Lines() = %q, want %q", got, want)
	}
	if got, want := r.Lines("serial=B"), []string{"line 3 serial=B"}; !slices.Equal(got, want) {
		t.Fatalf("Lines(B) = %q, want %q", got, want)
	}
	if got := r.Lines(""); got != nil {
		t.Fatalf("an empty match must not match everything, got %q", got)
	}
}

func TestRingRedacts(t *testing.T) {
	r := &Ring{max: 3}
	fmt.Fprintln(r, "x cmd wifi add-suggestion bench wpa2 hunter2")
	if got := r.Lines("x"); len(got) != 1 || strings.Contains(got[0], "hunter2") {
		t.Fatalf("passphrase not redacted: %q", got)
	}
}
