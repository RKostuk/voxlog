//go:build !darwin

package keychain

import "errors"

// Voxlog is a macOS app; these exist so the package still builds when
// something is cross-compiled for a look.
var errUnsupported = errors.New("keychain: only implemented on macOS")

func Set(service, account, secret string) error   { return errUnsupported }
func Get(service, account string) (string, error) { return "", errUnsupported }
func Delete(service, account string) error        { return errUnsupported }
