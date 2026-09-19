//go:build darwin && cgo

package core

/*
#cgo LDFLAGS: -framework Security -framework CoreFoundation
#cgo CFLAGS: -Wno-deprecated-declarations
#include <Security/Security.h>
#include <CoreFoundation/CoreFoundation.h>
#include <stdlib.h>
#include <string.h>

static OSStatus aiuWriteGeneric(const void *service, UInt32 serviceLen,
    const void *account, UInt32 accountLen, const void *secret, UInt32 secretLen) {
    SecKeychainItemRef item = NULL;
    OSStatus status = SecKeychainFindGenericPassword(NULL, serviceLen, service,
        accountLen, account, NULL, NULL, &item);
    if (status == errSecItemNotFound) {
        status = SecKeychainAddGenericPassword(NULL, serviceLen, service,
            accountLen, account, secretLen, secret, NULL);
        if (status != errSecDuplicateItem) return status;
        status = SecKeychainFindGenericPassword(NULL, serviceLen, service,
            accountLen, account, NULL, NULL, &item);
    }
    if (status != errSecSuccess) return status;
    status = SecKeychainItemModifyAttributesAndData(item, NULL, secretLen, secret);
    CFRelease(item);
    return status;
}

static OSStatus aiuDeleteGeneric(const void *service, UInt32 serviceLen,
    const void *account, UInt32 accountLen) {
    SecKeychainItemRef item = NULL;
    OSStatus status = SecKeychainFindGenericPassword(NULL, serviceLen, service,
        accountLen, account, NULL, NULL, &item);
    if (status == errSecItemNotFound) return errSecSuccess;
    if (status != errSecSuccess) return status;
    status = SecKeychainItemDelete(item);
    CFRelease(item);
    return status;
}

static OSStatus aiuReadGeneric(const void *service, UInt32 serviceLen,
    const void *account, UInt32 accountLen, void **secret, UInt32 *secretLen,
    void **foundAccount, UInt32 *foundAccountLen) {
    SecKeychainItemRef item = NULL;
    void *password = NULL;
    UInt32 passwordLen = 0;
    OSStatus status = SecKeychainFindGenericPassword(NULL, serviceLen, service,
        accountLen, account, &passwordLen, &password, &item);
    if (status != errSecSuccess) return status;
    *secret = malloc(passwordLen ? passwordLen : 1);
    if (!*secret) {
        SecKeychainItemFreeContent(NULL, password);
        CFRelease(item);
        return errSecAllocate;
    }
    if (passwordLen) memcpy(*secret, password, passwordLen);
    *secretLen = passwordLen;
    SecKeychainItemFreeContent(NULL, password);

    UInt32 tag = kSecAccountItemAttr;
    SecKeychainAttributeInfo info = {1, &tag, NULL};
    SecKeychainAttributeList *attributes = NULL;
    status = SecKeychainItemCopyAttributesAndData(item, &info, NULL,
        &attributes, NULL, NULL);
    if (status != errSecSuccess) {
        if (attributes) SecKeychainItemFreeAttributesAndData(attributes, NULL);
        CFRelease(item);
        free(*secret);
        *secret = NULL;
        return status;
    }
    if (!attributes || attributes->count != 1) {
        if (attributes) SecKeychainItemFreeAttributesAndData(attributes, NULL);
        CFRelease(item);
        free(*secret);
        *secret = NULL;
        return errSecDataNotAvailable;
    }
    {
        UInt32 length = attributes->attr[0].length;
        if (length && !attributes->attr[0].data) {
            SecKeychainItemFreeAttributesAndData(attributes, NULL);
            CFRelease(item);
            free(*secret);
            *secret = NULL;
            return errSecDataNotAvailable;
        }
        *foundAccount = malloc(length ? length : 1);
        if (*foundAccount) {
            if (length) memcpy(*foundAccount, attributes->attr[0].data, length);
            *foundAccountLen = length;
        } else {
            SecKeychainItemFreeAttributesAndData(attributes, NULL);
            CFRelease(item);
            free(*secret);
            *secret = NULL;
            return errSecAllocate;
        }
    }
    if (attributes) SecKeychainItemFreeAttributesAndData(attributes, NULL);
    CFRelease(item);
    return errSecSuccess;
}
*/
import "C"

import (
	"fmt"
	"unsafe"
)

func readKeychainItem(service, account string) (string, string, bool, error) {
	s := C.CBytes([]byte(service))
	defer C.free(s)
	var a unsafe.Pointer
	if account != "" {
		a = C.CBytes([]byte(account))
		defer C.free(a)
	}
	var secret, foundAccount unsafe.Pointer
	var secretLen, accountLen C.UInt32
	status := C.aiuReadGeneric(s, C.UInt32(len(service)), a, C.UInt32(len(account)), &secret, &secretLen, &foundAccount, &accountLen)
	if secret != nil {
		defer C.free(secret)
	}
	if foundAccount != nil {
		defer C.free(foundAccount)
	}
	if status == C.errSecItemNotFound {
		return "", "", false, nil
	}
	if status != C.errSecSuccess {
		return "", "", false, fmt.Errorf("keychain read failed (OSStatus %d)", int(status))
	}
	return string(C.GoBytes(secret, C.int(secretLen))), string(C.GoBytes(foundAccount, C.int(accountLen))), true, nil
}

func keychainRead(service, account string) (string, bool, error) {
	secret, _, ok, err := readKeychainItem(service, account)
	return secret, ok, err
}

// Security.framework receives the secret in memory. Updating an existing item
// preserves its access control and other attributes.
func keychainWrite(service, account, secret string) error {
	s, a, p := C.CBytes([]byte(service)), C.CBytes([]byte(account)), C.CBytes([]byte(secret))
	defer C.free(s)
	defer C.free(a)
	defer C.free(p)
	status := C.aiuWriteGeneric(s, C.UInt32(len(service)), a, C.UInt32(len(account)), p, C.UInt32(len(secret)))
	if status != C.errSecSuccess {
		return fmt.Errorf("keychain write failed (OSStatus %d)", int(status))
	}
	return nil
}

func keychainDelete(service, account string) error {
	s, a := C.CBytes([]byte(service)), C.CBytes([]byte(account))
	defer C.free(s)
	defer C.free(a)
	status := C.aiuDeleteGeneric(s, C.UInt32(len(service)), a, C.UInt32(len(account)))
	if status != C.errSecSuccess {
		return fmt.Errorf("keychain delete failed (OSStatus %d)", int(status))
	}
	return nil
}
