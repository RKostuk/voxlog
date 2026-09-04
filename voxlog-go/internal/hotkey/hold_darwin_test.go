package hotkey

import "testing"

var (
	ctrlKey = KeyID{Kind: "vk", Value: "59"} // left Control
	dKey    = KeyID{Kind: "vk", Value: "2"}  // "d"
)

// holdListener builds a listener in hold mode and records the edges it reports.
func holdListener(binding Binding) (*Listener, *[]string) {
	var events []string
	l := NewListener(binding, Binding{}, Binding{}, Binding{}, Callbacks{
		DictateDown: func() { events = append(events, "down") },
		DictateUp:   func() { events = append(events, "up") },
		Hold:        func() bool { return true },
	})
	return l, &events
}

func TestHoldReportsBothEdgesOfABareModifier(t *testing.T) {
	// The shipped default binding is right Command on its own: it never
	// generates a key-down at all, only a flags change, and hold mode has to
	// see both edges of it.
	l, events := holdListener(Binding{Key: dictateKey})

	l.handleDictateHold(dictateKey, []string{ModCommand}, true)
	l.handleDictateHold(dictateKey, nil, false)

	if len(*events) != 2 || (*events)[0] != "down" || (*events)[1] != "up" {
		t.Fatalf("got %v, want down then up", *events)
	}
}

func TestHoldIgnoresAutoRepeat(t *testing.T) {
	// macOS delivers a key-down every few tens of milliseconds for one
	// physical hold; each extra "down" would start a second recording.
	l, events := holdListener(Binding{Key: dKey})

	l.handleDictateHold(dKey, nil, true)
	l.handleDictateHold(dKey, nil, true)
	l.handleDictateHold(dKey, nil, true)
	l.handleDictateHold(dKey, nil, false)

	if len(*events) != 2 {
		t.Fatalf("got %v, want exactly one down and one up", *events)
	}
}

func TestHoldIgnoresOtherKeys(t *testing.T) {
	// Unlike a tap, holding a key to talk does not mean the hands stop --
	// typing while the key is down must not end the take.
	l, events := holdListener(Binding{Key: dictateKey})

	l.handleDictateHold(dictateKey, []string{ModCommand}, true)
	l.handleDictateHold(otherKey, []string{ModCommand}, true)
	l.handleDictateHold(otherKey, []string{ModCommand}, false)

	if len(*events) != 1 {
		t.Fatalf("got %v, want the take still running", *events)
	}
}

func TestHoldChordEndsWhenTheKeyIsReleased(t *testing.T) {
	l, events := holdListener(Binding{Mods: []string{ModControl}, Key: dKey})

	l.handleDictateHold(dKey, []string{ModControl}, true)
	l.handleDictateHold(dKey, []string{ModControl}, false)

	if len(*events) != 2 || (*events)[1] != "up" {
		t.Fatalf("got %v, want down then up", *events)
	}
}

func TestHoldChordEndsWhenTheModifierIsReleasedFirst(t *testing.T) {
	// Letting go of Control while still holding D is how a chord usually ends
	// in practice; waiting for the letter would leave the microphone open.
	l, events := holdListener(Binding{Mods: []string{ModControl}, Key: dKey})

	l.handleDictateHold(dKey, []string{ModControl}, true)
	l.handleDictateHold(ctrlKey, nil, false) // Control up, D still down

	if len(*events) != 2 || (*events)[1] != "up" {
		t.Fatalf("got %v, want the take to end with the modifier", *events)
	}
}

func TestHoldChordNeedsTheWholeCombination(t *testing.T) {
	// D on its own is not the binding, and must not start anything.
	l, events := holdListener(Binding{Mods: []string{ModControl}, Key: dKey})

	l.handleDictateHold(dKey, nil, true)
	l.handleDictateHold(dKey, nil, false)

	if len(*events) != 0 {
		t.Fatalf("got %v, want nothing -- the modifier was not held", *events)
	}
}

func TestHoldReleaseWithoutPressDoesNothing(t *testing.T) {
	// Escape can cancel a take mid-hold; the release that follows must not
	// start or stop anything.
	l, events := holdListener(Binding{Key: dictateKey})

	l.handleDictateHold(dictateKey, nil, false)

	if len(*events) != 0 {
		t.Fatalf("got %v, want nothing", *events)
	}
}

func TestHoldUnboundKeyDoesNothing(t *testing.T) {
	l, events := holdListener(Binding{})
	l.handleDictateHold(dictateKey, nil, true)
	if len(*events) != 0 {
		t.Fatalf("got %v, want nothing from an unset binding", *events)
	}
}

func TestHoldModeIsAskedPerEvent(t *testing.T) {
	// The setting has to apply to the very next press, without a restart.
	hold := false
	l := NewListener(Binding{Key: dictateKey}, Binding{}, Binding{}, Binding{}, Callbacks{
		Hold: func() bool { return hold },
	})
	if l.holdMode() {
		t.Fatal("hold mode is on before the setting says so")
	}
	hold = true
	if !l.holdMode() {
		t.Fatal("changing the setting did not reach the listener")
	}
}

func TestMeetingTapDetectorOnlyExistsWhenBound(t *testing.T) {
	l := NewListener(Binding{Key: dictateKey}, Binding{}, Binding{}, Binding{}, Callbacks{})
	if l.meeting != nil {
		t.Fatal("an unbound meeting key still watches for taps")
	}

	l = NewListener(Binding{Key: dictateKey}, Binding{}, Binding{Key: dKey}, Binding{}, Callbacks{})
	if l.meeting == nil {
		t.Fatal("a bound meeting key is not being watched")
	}
}
