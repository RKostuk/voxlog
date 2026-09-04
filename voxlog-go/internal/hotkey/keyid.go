package hotkey

import (
	"fmt"
	"strings"
)

type KeyID struct {
	Kind  string
	Value string
}

func (k KeyID) String() string {
	return fmt.Sprintf("%s:%s", k.Kind, k.Value)
}

func ParseKeyID(raw string) KeyID {
	kind, value, found := strings.Cut(raw, ":")
	if !found || (kind != "vk" && kind != "sym") {
		return KeyID{}
	}
	return KeyID{Kind: kind, Value: value}
}
