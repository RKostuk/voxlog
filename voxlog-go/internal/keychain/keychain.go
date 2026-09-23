// Package keychain stores one secret per service/account pair in the user's
// login keychain. It exists for the LLM API key: settings.json is a plain
// file in Application Support that the user can open, back up and sync, and
// a provider key does not belong in it.
package keychain

import "errors"

// ErrNotFound is returned by Get when nothing has been stored yet. A missing
// key is the normal state of a feature nobody has configured, so callers
// check for this rather than treating it as a failure.
var ErrNotFound = errors.New("keychain: no such item")

// Where the LLM API key lives. One item, not one per provider: the app talks
// to exactly one endpoint at a time, so pasting a key for a new provider
// replaces the old one rather than leaving a keychain full of stale secrets.
const (
	LLMService = "Voxlog LLM API"
	LLMAccount = "api-key"
)
