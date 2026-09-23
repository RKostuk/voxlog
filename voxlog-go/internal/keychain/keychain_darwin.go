//go:build darwin

package keychain

/*
#cgo LDFLAGS: -framework CoreFoundation -framework Security
#include <stdlib.h>
#include <string.h>
#include <CoreFoundation/CoreFoundation.h>
#include <Security/Security.h>

// The CoreFoundation dance lives here rather than in Go because every one of
// these calls needs CFDictionaries of CF constants, and building those
// through cgo one CFTypeRef at a time is where the leaks and the mismatched
// releases come from.

static CFDictionaryRef vox_query(const char *service, const char *account) {
	CFStringRef svc = CFStringCreateWithCString(NULL, service, kCFStringEncodingUTF8);
	CFStringRef acc = CFStringCreateWithCString(NULL, account, kCFStringEncodingUTF8);
	const void *keys[] = { kSecClass, kSecAttrService, kSecAttrAccount };
	const void *vals[] = { kSecClassGenericPassword, svc, acc };
	CFDictionaryRef q = CFDictionaryCreate(NULL, keys, vals, 3,
		&kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	CFRelease(svc);
	CFRelease(acc);
	return q;
}

// vox_set writes the secret, replacing whatever was under the same
// service/account. Update-then-add rather than delete-then-add: a delete
// followed by a failed add would lose a working key.
static OSStatus vox_set(const char *service, const char *account, const void *secret, int len) {
	CFDictionaryRef q = vox_query(service, account);
	CFDataRef data = CFDataCreate(NULL, (const UInt8 *)secret, len);

	const void *ukeys[] = { kSecValueData };
	const void *uvals[] = { data };
	CFDictionaryRef upd = CFDictionaryCreate(NULL, ukeys, uvals, 1,
		&kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);

	OSStatus st = SecItemUpdate(q, upd);
	if (st == errSecItemNotFound) {
		CFMutableDictionaryRef add = CFDictionaryCreateMutableCopy(NULL, 0, q);
		CFDictionarySetValue(add, kSecValueData, data);
		st = SecItemAdd(add, NULL);
		CFRelease(add);
	}

	CFRelease(upd);
	CFRelease(data);
	CFRelease(q);
	return st;
}

// vox_get copies the secret into *out, which the caller frees. Returns the
// OSStatus; *out is only set on success.
static OSStatus vox_get(const char *service, const char *account, void **out, int *outLen) {
	CFDictionaryRef q = vox_query(service, account);
	CFMutableDictionaryRef get = CFDictionaryCreateMutableCopy(NULL, 0, q);
	CFDictionarySetValue(get, kSecReturnData, kCFBooleanTrue);
	CFDictionarySetValue(get, kSecMatchLimit, kSecMatchLimitOne);

	CFTypeRef result = NULL;
	OSStatus st = SecItemCopyMatching(get, &result);
	if (st == errSecSuccess && result != NULL) {
		CFDataRef data = (CFDataRef)result;
		CFIndex n = CFDataGetLength(data);
		void *buf = malloc((size_t)n);
		if (buf == NULL) {
			st = errSecAllocate;
		} else {
			memcpy(buf, CFDataGetBytePtr(data), (size_t)n);
			*out = buf;
			*outLen = (int)n;
		}
	}
	if (result != NULL) {
		CFRelease(result);
	}
	CFRelease(get);
	CFRelease(q);
	return st;
}

static OSStatus vox_delete(const char *service, const char *account) {
	CFDictionaryRef q = vox_query(service, account);
	OSStatus st = SecItemDelete(q);
	CFRelease(q);
	return st;
}
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// Set stores secret under service/account, replacing anything already there.
func Set(service, account, secret string) error {
	cSvc, cAcc := C.CString(service), C.CString(account)
	defer C.free(unsafe.Pointer(cSvc))
	defer C.free(unsafe.Pointer(cAcc))

	// An empty secret cannot be handed to CFDataCreate as a nil pointer, and
	// storing one is meaningless anyway -- that is what Delete is for.
	if secret == "" {
		return Delete(service, account)
	}
	raw := []byte(secret)
	st := C.vox_set(cSvc, cAcc, unsafe.Pointer(&raw[0]), C.int(len(raw)))
	return statusError("storing", st)
}

// Get returns the stored secret, or ErrNotFound if there is none.
func Get(service, account string) (string, error) {
	cSvc, cAcc := C.CString(service), C.CString(account)
	defer C.free(unsafe.Pointer(cSvc))
	defer C.free(unsafe.Pointer(cAcc))

	var (
		buf unsafe.Pointer
		n   C.int
	)
	st := C.vox_get(cSvc, cAcc, &buf, &n)
	if st == C.errSecItemNotFound {
		return "", ErrNotFound
	}
	if err := statusError("reading", st); err != nil {
		return "", err
	}
	defer C.free(buf)
	return string(C.GoBytes(buf, n)), nil
}

// Delete removes the secret. Deleting one that is not there is not an error:
// the caller wanted it gone, and it is gone.
func Delete(service, account string) error {
	cSvc, cAcc := C.CString(service), C.CString(account)
	defer C.free(unsafe.Pointer(cSvc))
	defer C.free(unsafe.Pointer(cAcc))

	st := C.vox_delete(cSvc, cAcc)
	if st == C.errSecItemNotFound {
		return nil
	}
	return statusError("deleting", st)
}

func statusError(verb string, st C.OSStatus) error {
	if st == C.errSecSuccess {
		return nil
	}
	// The OSStatus is worth surfacing verbatim: -25308 (user cancelled the
	// keychain prompt) and -34018 (no keychain access, i.e. an unsigned
	// build) are the two failures a user actually hits, and they mean
	// completely different things.
	return fmt.Errorf("keychain: %s item: OSStatus %d", verb, int(st))
}
