//go:build darwin && cgo

package core

/*
#cgo LDFLAGS: -framework Security -framework CoreFoundation
#cgo CFLAGS: -Wno-deprecated-declarations
#include <Security/Security.h>
#include <CoreFoundation/CoreFoundation.h>
#include <stdlib.h>
#include <string.h>

static void aiuAppend(char **buf, size_t *len, const char *prefix, const char *value, size_t valueLen) {
    size_t add = strlen(prefix) + valueLen + 1;
    char *next = realloc(*buf, *len + add + 1);
    if (!next) return;
    *buf = next;
    memcpy(*buf + *len, prefix, strlen(prefix));
    memcpy(*buf + *len + strlen(prefix), value, valueLen);
    (*buf)[*len + add - 1] = '\n';
    *len += add;
    (*buf)[*len] = 0;
}

static void aiuAppendString(char **buf, size_t *len, const char *prefix, CFStringRef s) {
    if (!s) return;
    CFIndex max = CFStringGetMaximumSizeForEncoding(CFStringGetLength(s), kCFStringEncodingUTF8) + 1;
    char *tmp = malloc(max);
    if (!tmp) return;
    if (CFStringGetCString(s, tmp, max, kCFStringEncodingUTF8)) aiuAppend(buf, len, prefix, tmp, strlen(tmp));
    free(tmp);
}

static int aiuHasAuth(CFArrayRef auths, CFStringRef want) {
    for (CFIndex i = 0; auths && i < CFArrayGetCount(auths); i++) {
        if (CFEqual(CFArrayGetValueAtIndex(auths, i), want)) return 1;
    }
    return 0;
}

// Describes the item's access control without reading its secret, so it never
// prompts: "decrypt-any" (any app may read), "decrypt-app:<path>" per trusted
// app, and "partition:<description>" for the partition list.
static OSStatus aiuDescribeAccess(const void *service, UInt32 serviceLen,
    const void *account, UInt32 accountLen, char **out) {
    SecKeychainItemRef item = NULL;
    OSStatus status = SecKeychainFindGenericPassword(NULL, serviceLen, service, accountLen, account, NULL, NULL, &item);
    if (status != errSecSuccess) return status;
    SecAccessRef access = NULL;
    status = SecKeychainItemCopyAccess(item, &access);
    CFRelease(item);
    if (status != errSecSuccess) return status;
    CFArrayRef acls = NULL;
    status = SecAccessCopyACLList(access, &acls);
    CFRelease(access);
    if (status != errSecSuccess) return status;
    size_t len = 0;
    *out = calloc(1, 1);
    for (CFIndex i = 0; i < CFArrayGetCount(acls); i++) {
        SecACLRef acl = (SecACLRef)CFArrayGetValueAtIndex(acls, i);
        CFArrayRef auths = SecACLCopyAuthorizations(acl);
        CFArrayRef apps = NULL;
        CFStringRef desc = NULL;
        SecKeychainPromptSelector selector = 0;
        if (SecACLCopyContents(acl, &apps, &desc, &selector) == errSecSuccess) {
            if (aiuHasAuth(auths, kSecACLAuthorizationDecrypt)) {
                if (!apps) aiuAppend(out, &len, "decrypt-any", "", 0);
                for (CFIndex a = 0; apps && a < CFArrayGetCount(apps); a++) {
                    CFDataRef data = NULL;
                    if (SecTrustedApplicationCopyData((SecTrustedApplicationRef)CFArrayGetValueAtIndex(apps, a), &data) == errSecSuccess && data) {
                        size_t n = (size_t)CFDataGetLength(data);
                        const char *bytes = (const char *)CFDataGetBytePtr(data);
                        while (n && bytes[n - 1] == 0) n--;
                        aiuAppend(out, &len, "decrypt-app:", bytes, n);
                        CFRelease(data);
                    }
                }
            }
            if (aiuHasAuth(auths, kSecACLAuthorizationPartitionID)) aiuAppendString(out, &len, "partition:", desc);
        }
        if (auths) CFRelease(auths);
        if (apps) CFRelease(apps);
        if (desc) CFRelease(desc);
    }
    CFRelease(acls);
    return errSecSuccess;
}

// Reads an item with Keychain prompts disabled: success means AIU reads it
// without asking; a refusal means macOS would have asked.
static OSStatus aiuSilentRead(const void *service, UInt32 serviceLen, const void *account, UInt32 accountLen) {
    Boolean allowed = true;
    SecKeychainGetUserInteractionAllowed(&allowed);
    SecKeychainSetUserInteractionAllowed(false);
    UInt32 length = 0;
    void *data = NULL;
    OSStatus status = SecKeychainFindGenericPassword(NULL, serviceLen, service, accountLen, account, &length, &data, NULL);
    if (data) {
        memset(data, 0, length);
        SecKeychainItemFreeContent(NULL, data);
    }
    SecKeychainSetUserInteractionAllowed(allowed);
    return status;
}
*/
import "C"

import (
	"fmt"
	"strings"
	"unsafe"
)

// claudeItemTrustsSecurity says whether /usr/bin/security, the tool Claude Code
// and AIU both read through, may read the item without asking. It inspects the
// item readClaudeKeychain would read: currentUser()'s, else the first.
func claudeItemTrustsSecurity(service string) (found, trusted bool, err error) {
	s := C.CBytes([]byte(service))
	defer C.free(s)
	user := currentUser()
	a := C.CBytes([]byte(user))
	defer C.free(a)
	var out *C.char
	status := C.aiuDescribeAccess(s, C.UInt32(len(service)), a, C.UInt32(len(user)), &out)
	if status == C.errSecItemNotFound {
		status = C.aiuDescribeAccess(s, C.UInt32(len(service)), nil, 0, &out)
	}
	if out != nil {
		defer C.free(unsafe.Pointer(out))
	}
	if status == C.errSecItemNotFound {
		return false, false, nil
	}
	if status != C.errSecSuccess {
		return true, false, fmt.Errorf("could not read the item's access control (OSStatus %d)", int(status))
	}
	app, partitioned, partitionOK := false, false, false
	for _, line := range strings.Split(C.GoString(out), "\n") {
		switch {
		case line == "decrypt-any" || line == "decrypt-app:"+securityBin:
			app = true
		case strings.HasPrefix(line, "partition:"):
			partitioned = true
			partitionOK = partitionOK || partitionsAllowAppleTools(strings.TrimPrefix(line, "partition:"))
		}
	}
	return true, app && (!partitioned || partitionOK), nil
}

func silentItemAccess(service, account string) (string, string) {
	s, a := C.CBytes([]byte(service)), C.CBytes([]byte(account))
	defer C.free(s)
	defer C.free(a)
	switch status := C.aiuSilentRead(s, C.UInt32(len(service)), a, C.UInt32(len(account))); status {
	case C.errSecSuccess:
		return AccessGranted, ""
	case C.errSecItemNotFound:
		return AccessMissing, ""
	case C.errSecInteractionNotAllowed, C.errSecAuthFailed:
		return AccessNeedsApproval, "macOS will ask before this build of AIU can read its tokens"
	default:
		return AccessUnknown, fmt.Sprintf("Keychain check failed (OSStatus %d)", int(status))
	}
}
