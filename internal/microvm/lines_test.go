package microvm

import (
	"strings"
	"testing"
)

type emitted struct {
	line      string
	truncated bool
}

func collect(input string, limit int) []emitted {
	var out []emitted
	splitLines(strings.NewReader(input), limit, func(l string, tr bool) { out = append(out, emitted{l, tr}) })
	return out
}

func TestSplitLinesResumesAfterLongLine(t *testing.T) {
	long := strings.Repeat("x", 200*1024)
	got := collect("first\n"+long+"\nnext\n", maxLogLine)
	if len(got) != 3 {
		t.Fatalf("got %d lines, want 3", len(got))
	}
	if got[0] != (emitted{"first", false}) {
		t.Errorf("line 1: %+v", got[0])
	}
	if len(got[1].line) != maxLogLine || !got[1].truncated {
		t.Errorf("long line: %d bytes, truncated=%v; want %d, true", len(got[1].line), got[1].truncated, maxLogLine)
	}
	if got[2] != (emitted{"next", false}) {
		t.Errorf("output after the long line was lost: %+v", got[2])
	}
}

func TestSplitLinesEdgeCases(t *testing.T) {
	cases := []struct {
		in    string
		limit int
		want  []emitted
	}{
		{"", maxLogLine, nil},
		{"a\nb", maxLogLine, []emitted{{"a", false}, {"b", false}}},       // final line without newline
		{"a\r\nb\r\n", maxLogLine, []emitted{{"a", false}, {"b", false}}}, // CRLF
		{"a\n\nb\n", maxLogLine, []emitted{{"a", false}, {"", false}, {"b", false}}},
		{"abcdef\ngh\n", 4, []emitted{{"abcd", true}, {"gh", false}}},
		{"abcd\n", 4, []emitted{{"abcd", false}}},  // exactly the limit
		{"abcdefgh", 4, []emitted{{"abcd", true}}}, // long final line
	}
	for _, c := range cases {
		got := collect(c.in, c.limit)
		if len(got) != len(c.want) {
			t.Errorf("%q: got %+v, want %+v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%q line %d: got %+v, want %+v", c.in, i, got[i], c.want[i])
			}
		}
	}
}
