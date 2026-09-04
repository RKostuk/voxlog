package hotkey

import "testing"

func TestLabelKnownVirtualKeycodes(t *testing.T) {
	cases := map[string]string{
		"54":  "Right Command",
		"60":  "Right Shift",
		"105": "F13",
		"53":  "Escape",
	}
	for vk, want := range cases {
		if got := Label(KeyID{Kind: "vk", Value: vk}); got != want {
			t.Errorf("Label(vk:%s) = %q, want %q", vk, got, want)
		}
	}
}

func TestLabelUnknownVirtualKeycodeStaysIdentifiable(t *testing.T) {
	// Any keycode is bindable, listed or not -- an unlisted one must still
	// show something the user can recognize, never an empty label.
	got := Label(KeyID{Kind: "vk", Value: "222"})
	if got != "Key 222" {
		t.Fatalf("got %q, want %q", got, "Key 222")
	}
}

func TestLabelUnsetKey(t *testing.T) {
	if got := Label(KeyID{}); got != "Not set" {
		t.Fatalf("got %q, want %q", got, "Not set")
	}
}

func TestLabelSymbolicKeyFallsBackToItsValue(t *testing.T) {
	if got := Label(KeyID{Kind: "sym", Value: "shift_r"}); got != "shift_r" {
		t.Fatalf("got %q, want %q", got, "shift_r")
	}
}

func TestLabelOfDefaultBindingsIsHumanReadable(t *testing.T) {
	// The Settings window shows these on first run; a raw "Key 54" there
	// means vkLabels drifted away from the defaults in settings.go.
	for raw, want := range map[string]string{
		"vk:54": "Right Command",
		"vk:60": "Right Shift",
	} {
		if got := Label(ParseKeyID(raw)); got != want {
			t.Errorf("Label(%s) = %q, want %q", raw, got, want)
		}
	}
}

func TestLabelCoversOrdinaryKeys(t *testing.T) {
	// "Key 38" in the Settings window was the whole complaint: letters,
	// digits and punctuation all fell through to the numeric fallback.
	for vk, want := range map[string]string{
		"38": "J", "0": "A", "46": "M", "29": "0", "18": "1",
		"49": "Space", "36": "Return", "126": "Up Arrow", "44": "/",
	} {
		if got := Label(KeyID{Kind: "vk", Value: vk}); got != want {
			t.Errorf("Label(vk:%s) = %q, want %q", vk, got, want)
		}
	}
}

func TestLabelHasNoDuplicateNames(t *testing.T) {
	// Two keycodes sharing a label make the Settings window show the same
	// binding for two different physical keys.
	seen := map[string]string{}
	for vk, name := range vkLabels {
		if other, dup := seen[name]; dup {
			t.Errorf("vk:%s and vk:%s are both labeled %q", vk, other, name)
		}
		seen[name] = vk
	}
}
