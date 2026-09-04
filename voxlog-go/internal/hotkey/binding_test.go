package hotkey

import "testing"

func TestParseBindingRoundTrip(t *testing.T) {
	for _, raw := range []string{"vk:54", "ctrl+vk:8", "ctrl+alt+shift+cmd+vk:49"} {
		if got := ParseBinding(raw).String(); got != raw {
			t.Errorf("ParseBinding(%q).String() = %q", raw, got)
		}
	}
}

func TestParseBindingNormalizesModifierOrder(t *testing.T) {
	// Two spellings of the same shortcut must compare equal, or a binding
	// saved by the capture path would not match the one loaded at startup.
	a := ParseBinding("cmd+shift+vk:8")
	b := ParseBinding("shift+cmd+vk:8")
	if a.String() != b.String() {
		t.Fatalf("%q vs %q", a.String(), b.String())
	}
}

func TestParseBindingRejectsGarbage(t *testing.T) {
	// An unknown modifier must not be dropped silently: that would widen the
	// binding to fire without it.
	for _, raw := range []string{"", "garbage", "hyper+vk:8", "ctrl+nonsense"} {
		if got := ParseBinding(raw); !got.IsZero() {
			t.Errorf("ParseBinding(%q) = %+v, want zero", raw, got)
		}
	}
}

func TestBindingMatchesRequiresExactModifiers(t *testing.T) {
	b := ParseBinding("ctrl+vk:8")
	key := KeyID{Kind: "vk", Value: "8"}

	if !b.Matches(key, []string{ModControl}) {
		t.Error("Control+C should match Control held")
	}
	// Control+Shift+C is a different shortcut the user may have bound
	// elsewhere; firing on it too would make both go off at once.
	if b.Matches(key, []string{ModControl, ModShift}) {
		t.Error("Control+C must not match with Shift also held")
	}
	if b.Matches(key, nil) {
		t.Error("Control+C must not match the bare key")
	}
	if b.Matches(KeyID{Kind: "vk", Value: "9"}, []string{ModControl}) {
		t.Error("matched the wrong key")
	}
}

func TestBindingLabel(t *testing.T) {
	if got := ParseBinding("ctrl+alt+vk:8").Label(); got != "Control + Option + C" {
		t.Errorf("got %q", got)
	}
	if got := ParseBinding("vk:54").Label(); got != "Right Command" {
		t.Errorf("got %q", got)
	}
	if got := (Binding{}).Label(); got != "Not set" {
		t.Errorf("got %q", got)
	}
}

func TestBindingHasModsPicksTheDetector(t *testing.T) {
	// A plain key keeps tap semantics (press and release with nothing in
	// between); anything with modifiers fires on key-down instead.
	if ParseBinding("vk:54").HasMods() {
		t.Error("a bare key should not be treated as a combination")
	}
	if !ParseBinding("cmd+vk:54").HasMods() {
		t.Error("a combination should be")
	}
}
