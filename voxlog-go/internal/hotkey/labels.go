package hotkey

import "fmt"

// vkLabels maps macOS virtual keycodes to human-readable names, so the
// Settings window can show "Right Command" instead of the raw "vk:54" the
// KeyID serialization uses. Covers the keys a standalone-tap hotkey is
// actually plausible on (modifiers, function keys, a few specials) rather
// than the whole keyboard -- anything unlisted falls back to its number,
// which is still bindable, just less pretty.
var vkLabels = map[string]string{
	// Letters and digits, so a binding on an ordinary key reads as that key
	// instead of "Key 38". The layout is the physical ANSI one: these codes
	// describe positions, not the characters a Ukrainian or Dvorak layout
	// produces there, which is exactly what a layout-independent hotkey wants.
	"0": "A", "1": "S", "2": "D", "3": "F", "4": "H", "5": "G",
	"6": "Z", "7": "X", "8": "C", "9": "V", "11": "B", "12": "Q",
	"13": "W", "14": "E", "15": "R", "16": "Y", "17": "T", "31": "O",
	"32": "U", "34": "I", "35": "P", "37": "L", "38": "J", "40": "K",
	"45": "N", "46": "M",
	"18": "1", "19": "2", "20": "3", "21": "4", "22": "6", "23": "5",
	"25": "9", "26": "7", "28": "8", "29": "0",

	// Punctuation, named by what sits there on ANSI.
	"24": "=", "27": "-", "30": "]", "33": "[", "39": "'", "41": ";",
	"42": "\\", "43": ",", "44": "/", "47": ".", "50": "`",

	// Modifiers.
	"54": "Right Command", "55": "Left Command",
	"58": "Left Option", "61": "Right Option",
	"59": "Left Control", "62": "Right Control",
	"56": "Left Shift", "60": "Right Shift",
	"57": "Caps Lock", "63": "Fn",

	// Function row and the extended F-keys.
	"122": "F1", "120": "F2", "99": "F3", "118": "F4", "96": "F5",
	"97": "F6", "98": "F7", "100": "F8", "101": "F9", "109": "F10",
	"103": "F11", "111": "F12", "105": "F13", "107": "F14", "113": "F15",
	"106": "F16", "64": "F17", "79": "F18", "80": "F19", "90": "F20",

	// Editing and navigation.
	"49": "Space", "48": "Tab", "53": "Escape", "36": "Return",
	"51": "Delete", "117": "Forward Delete", "114": "Help",
	"115": "Home", "119": "End", "116": "Page Up", "121": "Page Down",
	"123": "Left Arrow", "124": "Right Arrow", "125": "Down Arrow", "126": "Up Arrow",

	// Numeric keypad, prefixed so it is never confused with the top row.
	"82": "Keypad 0", "83": "Keypad 1", "84": "Keypad 2", "85": "Keypad 3",
	"86": "Keypad 4", "87": "Keypad 5", "88": "Keypad 6", "89": "Keypad 7",
	"91": "Keypad 8", "92": "Keypad 9",
	"65": "Keypad .", "67": "Keypad *", "69": "Keypad +", "71": "Keypad Clear",
	"75": "Keypad /", "76": "Keypad Enter", "78": "Keypad -", "81": "Keypad =",
}

// Label returns a human-readable name for k, e.g. "Right Command" for
// KeyID{"vk", "54"}. Unknown keys fall back to a still-identifiable form
// rather than an empty string.
func Label(k KeyID) string {
	if k.Kind == "" {
		return "Not set"
	}
	if k.Kind == "vk" {
		if name, ok := vkLabels[k.Value]; ok {
			return name
		}
		return fmt.Sprintf("Key %s", k.Value)
	}
	return k.Value
}
