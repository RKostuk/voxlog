package hotkey

import (
	"sort"
	"strings"
)

// Modifier names, as they appear in a serialized binding.
const (
	ModCommand = "cmd"
	ModShift   = "shift"
	ModControl = "ctrl"
	ModOption  = "alt"
)

// modOrder is the order modifiers are written and shown in, matching the
// convention macOS uses in its own menus.
var modOrder = map[string]int{ModControl: 0, ModOption: 1, ModShift: 2, ModCommand: 3}

var modLabels = map[string]string{
	ModControl: "Control",
	ModOption:  "Option",
	ModShift:   "Shift",
	ModCommand: "Command",
}

// Binding is one hotkey: a key, optionally with modifiers that must be held
// while it is pressed.
//
// With no modifiers it keeps the original standalone-tap meaning -- press and
// release the key with nothing in between -- which is what makes a bare
// right-Command binding usable at all, since Command alone is otherwise a
// modifier nobody "taps". With modifiers it fires on the key going down while
// they are held, the way every other shortcut on the system behaves.
type Binding struct {
	Mods []string
	Key  KeyID
}

// ParseBinding reads the stored form: "vk:54" for a plain key, or
// "ctrl+alt+vk:8" for a combination. Unparseable input yields a zero Binding,
// which never matches anything, rather than a binding on some arbitrary key.
func ParseBinding(raw string) Binding {
	parts := strings.Split(raw, "+")
	if len(parts) == 0 {
		return Binding{}
	}

	key := ParseKeyID(parts[len(parts)-1])
	if key == (KeyID{}) {
		return Binding{}
	}

	var mods []string
	for _, p := range parts[:len(parts)-1] {
		if _, ok := modOrder[p]; !ok {
			return Binding{} // an unknown modifier would silently widen the binding
		}
		mods = append(mods, p)
	}
	return Binding{Mods: canonicalMods(mods), Key: key}
}

// canonicalMods sorts and de-duplicates, so the same combination always
// serializes to the same string and compares equal.
func canonicalMods(mods []string) []string {
	if len(mods) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(mods))
	for _, m := range mods {
		if _, ok := modOrder[m]; !ok || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return modOrder[out[i]] < modOrder[out[j]] })
	return out
}

func (b Binding) String() string {
	if b.IsZero() {
		return ""
	}
	if len(b.Mods) == 0 {
		return b.Key.String()
	}
	return strings.Join(b.Mods, "+") + "+" + b.Key.String()
}

func (b Binding) IsZero() bool { return b.Key == KeyID{} }

// Label renders the binding for the Settings window, e.g. "Control + C".
func (b Binding) Label() string {
	if b.IsZero() {
		return "Not set"
	}
	parts := make([]string, 0, len(b.Mods)+1)
	for _, m := range b.Mods {
		parts = append(parts, modLabels[m])
	}
	return strings.Join(append(parts, Label(b.Key)), " + ")
}

// HasMods reports whether this binding needs modifiers held, which is what
// decides between chord matching and standalone-tap detection.
func (b Binding) HasMods() bool { return len(b.Mods) > 0 }

// Matches reports whether a key press of kid, with mods currently held,
// triggers this binding. Modifiers must match exactly: Control+C must not
// fire on Control+Shift+C, which is a different shortcut the user may well
// have bound elsewhere.
func (b Binding) Matches(kid KeyID, mods []string) bool {
	if b.IsZero() || kid != b.Key {
		return false
	}
	held := canonicalMods(mods)
	if len(held) != len(b.Mods) {
		return false
	}
	for i := range held {
		if held[i] != b.Mods[i] {
			return false
		}
	}
	return true
}
