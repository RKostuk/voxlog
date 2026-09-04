package hotkey

import "testing"

func TestKeyIDStringRoundTrip(t *testing.T) {
	cases := []KeyID{
		{Kind: "vk", Value: "54"},
		{Kind: "sym", Value: "shift_r"},
	}
	for _, k := range cases {
		s := k.String()
		got := ParseKeyID(s)
		if got != k {
			t.Errorf("ParseKeyID(%q) = %+v, want %+v", s, got, k)
		}
	}
}

func TestParseKeyIDMalformed(t *testing.T) {
	got := ParseKeyID("garbage")
	if got != (KeyID{}) {
		t.Errorf("ParseKeyID(garbage) = %+v, want zero value", got)
	}
}
