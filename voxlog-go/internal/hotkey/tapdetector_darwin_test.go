package hotkey

import "testing"

var (
	dictateKey = KeyID{Kind: "vk", Value: "54"} // right Command
	otherKey   = KeyID{Kind: "vk", Value: "8"}  // "c"
)

// newCounter returns a detector for dictateKey and a pointer to its fire count.
func newCounter() (*tapDetector, *int) {
	fired := 0
	d := &tapDetector{target: dictateKey, callback: func() { fired++ }}
	return d, &fired
}

func TestTapFiresOnStandalonePressRelease(t *testing.T) {
	d, fired := newCounter()
	d.onPress(dictateKey)
	d.onRelease(dictateKey)
	if *fired != 1 {
		t.Fatalf("fired %d times, want 1", *fired)
	}
}

func TestTapSuppressedWhenCombinedWithAnotherKey(t *testing.T) {
	// Cmd+C must paste-nothing and start-nothing: the binding is a standalone
	// tap, so any other key going down while it is held cancels the toggle.
	d, fired := newCounter()
	d.onPress(dictateKey)
	d.onPress(otherKey)
	d.onRelease(otherKey)
	d.onRelease(dictateKey)
	if *fired != 0 {
		t.Fatalf("fired %d times, want 0 -- the tap was part of a chord", *fired)
	}
}

func TestTapRearmsAfterASuppressedChord(t *testing.T) {
	d, fired := newCounter()
	d.onPress(dictateKey)
	d.onPress(otherKey)
	d.onRelease(dictateKey)
	if *fired != 0 {
		t.Fatalf("chord should not fire, got %d", *fired)
	}

	// A fresh press clears the "other key seen" flag; the next clean tap works.
	d.onPress(dictateKey)
	d.onRelease(dictateKey)
	if *fired != 1 {
		t.Fatalf("fired %d times after re-press, want 1", *fired)
	}
}

func TestTapIgnoresReleaseOfOtherKeys(t *testing.T) {
	d, fired := newCounter()
	d.onRelease(otherKey)
	if *fired != 0 {
		t.Fatalf("fired %d times on an unrelated release, want 0", *fired)
	}
}

func TestTapIgnoresReleaseWithoutPress(t *testing.T) {
	// A key-up with no preceding key-down happens for real: the tap can start
	// while the key was already held (app launch, permission grant mid-hold).
	d, fired := newCounter()
	d.onRelease(dictateKey)
	if *fired != 1 {
		t.Fatalf("fired %d times, want 1 -- a bare release still reads as a tap", *fired)
	}
}

func TestTapOtherKeyPressedWhileNotHeldDoesNotArmSuppression(t *testing.T) {
	d, fired := newCounter()
	d.onPress(otherKey) // typing before touching the binding
	d.onRelease(otherKey)
	d.onPress(dictateKey)
	d.onRelease(dictateKey)
	if *fired != 1 {
		t.Fatalf("fired %d times, want 1 -- earlier typing must not block the next tap", *fired)
	}
}

func TestTapNilCallbackDoesNotPanic(t *testing.T) {
	// NewListener's callers may pass nil (e.g. no escape handler wired).
	d := &tapDetector{target: dictateKey}
	d.onPress(dictateKey)
	d.onRelease(dictateKey)
}

func TestTapRepeatedPressesStillFireOnce(t *testing.T) {
	// macOS auto-repeat delivers many key-downs for one physical hold.
	d, fired := newCounter()
	d.onPress(dictateKey)
	d.onPress(dictateKey)
	d.onPress(dictateKey)
	d.onRelease(dictateKey)
	if *fired != 1 {
		t.Fatalf("fired %d times, want 1", *fired)
	}
}

func TestListenerDetectorsAreIndependent(t *testing.T) {
	dictateFired, historyFired := 0, 0
	l := NewListener(
		Binding{Key: dictateKey},
		Binding{Key: KeyID{Kind: "vk", Value: "60"}},
		Binding{},
		Binding{},
		Callbacks{
			Dictate: func() { dictateFired++ },
			History: func() { historyFired++ },
		},
	)

	l.dictate.onPress(dictateKey)
	l.history.onPress(dictateKey) // the same event reaches both detectors
	l.dictate.onRelease(dictateKey)
	l.history.onRelease(dictateKey)

	if dictateFired != 1 {
		t.Fatalf("dictate fired %d times, want 1", dictateFired)
	}
	if historyFired != 0 {
		t.Fatalf("history fired %d times, want 0 -- it is bound to a different key", historyFired)
	}
}
