package main

import "testing"

func TestQueryFromMKVPath(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"/staging/Revenge of the Sith_t00.mkv", "Revenge of the Sith"},
		{"/staging/Revenge of the Sith_t02.mkv", "Revenge of the Sith"},
		{"/output/THE_DARK_KNIGHT_t00.mkv", "THE DARK KNIGHT"},
		{"/output/Oppenheimer_t00.mkv", "Oppenheimer"},
		// No _tNN suffix — use name as-is.
		{"/output/Oppenheimer.mkv", "Oppenheimer"},
		// _t not followed by digits — leave alone.
		{"/output/District_t9000_extra.mkv", "District t 9000 extra"},
	}
	for _, c := range cases {
		got := queryFromMKVPath(c.in)
		if got != c.want {
			t.Errorf("queryFromMKVPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
