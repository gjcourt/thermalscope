package gpu

import (
	"testing"
)

func TestParseFloat(t *testing.T) {
	cases := []struct {
		in   string
		want float64
		ok   bool
	}{
		{"75", 75, true},
		{"75.5", 75.5, true},
		{"[N/A]", 0, false},
		{"N/A", 0, false},
		{"", 0, false},
		{"  80  ", 80, true},
	}
	for _, tc := range cases {
		got, ok := parseFloat(tc.in)
		if ok != tc.ok {
			t.Errorf("parseFloat(%q): ok=%v want %v", tc.in, ok, tc.ok)
		}
		if ok && got != tc.want {
			t.Errorf("parseFloat(%q): got %v want %v", tc.in, got, tc.want)
		}
	}
}
