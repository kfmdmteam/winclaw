//go:build windows

package security

import (
	"errors"
	"fmt"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Sentinel errors returned by credential operations.
var (
	ErrSecretNotFound = errors.New("credman: secret not found")
	ErrAccessDenied   = errors.New("credman: access denied")
)

// targetName builds the Windows Credential Manager target name for a given key.
// Format: "WinClaw/<key>"
func targetName(key string) string {
	return "WinClaw/" + key
}

// Windows Credential Manager constants.
const (
	credTypeGeneric    = 1  // CRED_TYPE_GENERIC
	credPersistSession = 1  // CRED_PERSIST_SESSION
	credPersistLocal   = 2  // CRED_PERSIST_LOCAL
)

// credential mirrors the Windows CREDENTIAL structure used by CredReadW /
// CredWriteW. Only the fields we need are included; the layout must match the
// Windows ABI exactly.
//
//	typedef struct _CREDENTIAL {
//	    DWORD  Flags;
//	    DWORD  Type;
//	    LPWSTR TargetName;
//	    LPWSTR Comment;
//	    FILETIME LastWritten;
//	    DWORD  CredentialBlobSize;
//	    LPBYTE CredentialBlob;
//	    DWORD  Persist;
//	    DWORD  AttributeCount;
//	    PCREDENTIAL_ATTRIBUTE Attributes;
//	    LPWSTR TargetAlias;
//	    LPWSTR UserName;
//	} CREDENTIAL;
type credential struct {
	Flags              uint32
	Type               uint32
	TargetName         *uint16
	Comment            *uint16
	LastWrittenLow     uint32
	LastWrittenHigh    uint32
	CredentialBlobSize uint32
	CredentialBlob     *byte
	Persist            uint32
	AttributeCount     uint32
	Attributes         uintptr
	TargetAlias        *uint16
	UserName           *uint16
}

var (
	modadvapi32   = windows.NewLazySystemDLL("advapi32.dll")
	procCredReadW = modadvapi32.NewProc("CredReadW")
	procCredWriteW = modadvapi32.NewProc("CredWriteW")
	procCredDeleteW = modadvapi32.NewProc("CredDeleteW")
	procCredFree    = modadvapi32.NewProc("CredFree")
)

// encodeUTF16Bytes converts a Go string to a little-endian UTF-16 byte slice
// suitable for use as a CredentialBlob.
func encodeUTF16Bytes(s string) []byte {
	runes := utf16.Encode([]rune(s))
	buf := make([]byte, len(runes)*2)
	for i, r := range runes {
		buf[i*2] = byte(r)
		buf[i*2+1] = byte(r >> 8)
	}
	return buf
}

// decodeUTF16Bytes converts a little-endian UTF-16 byte slice back to a Go string.
func decodeUTF16Bytes(b []byte) string {
	if len(b)%2 != 0 {
		b = b[:len(b)-1]
	}
	u16 := make([]uint16, len(b)/2)
	for i := range u16 {
		u16[i] = uint16(b[i*2]) | uint16(b[i*2+1])<<8
	}
	return string(utf16.Decode(u16))
}

// classifyError maps a Windows error code to a typed sentinel where possible.
func classifyError(err error) error {
	var errno windows.Errno
	if errors.As(err, &errno) {
		switch errno {
		case windows.ERROR_NOT_FOUND:
			return ErrSecretNotFound
		case windows.ERROR_ACCESS_DENIED:
			return ErrAccessDenied
		}
	}
	return err
}

// StoreSecret writes value under key in the Windows Credential Manager.
// The value is encoded as UTF-16LE, matching the convention used by most
// Windows applications.
func StoreSecret(key, value string) error {
	targetPtr, err := windows.UTF16PtrFromString(targetName(key))
	if err != nil {
		return fmt.Errorf("credman: encode target name: %w", err)
	}

	blob := encodeUTF16Bytes(value)

	cred := credential{
		Type:               credTypeGeneric,
		TargetName:         targetPtr,
		CredentialBlobSize: uint32(len(blob)),
		Persist:            credPersistLocal,
	}
	if len(blob) > 0 {
		cred.CredentialBlob = &blob[0]
	}

	r, _, e := procCredWriteW.Call(uintptr(unsafe.Pointer(&cred)), 0)
	if r == 0 {
		return fmt.Errorf("credman: CredWriteW: %w", classifyError(e))
	}
	return nil
}

// ReadSecret retrieves the value stored under key. Returns ErrSecretNotFound
// if no credential with that key exists.
func ReadSecret(key string) (string, error) {
	targetPtr, err := windows.UTF16PtrFromString(targetName(key))
	if err != nil {
		return "", fmt.Errorf("credman: encode target name: %w", err)
	}

	var pcred *credential
	r, _, e := procCredReadW.Call(
		uintptr(unsafe.Pointer(targetPtr)),
		credTypeGeneric,
		0,
		uintptr(unsafe.Pointer(&pcred)),
	)
	if r == 0 {
		return "", fmt.Errorf("credman: CredReadW: %w", classifyError(e))
	}
	defer procCredFree.Call(uintptr(unsafe.Pointer(pcred)))

	if pcred.CredentialBlobSize == 0 || pcred.CredentialBlob == nil {
		return "", nil
	}

	blob := unsafe.Slice(pcred.CredentialBlob, pcred.CredentialBlobSize)
	// Make a copy before CredFree runs.
	blobCopy := make([]byte, len(blob))
	copy(blobCopy, blob)

	return decodeUTF16Bytes(blobCopy), nil
}

// DeleteSecret removes the credential stored under key. Returns
// ErrSecretNotFound if the credential did not exist.
func DeleteSecret(key string) error {
	targetPtr, err := windows.UTF16PtrFromString(targetName(key))
	if err != nil {
		return fmt.Errorf("credman: encode target name: %w", err)
	}

	r, _, e := procCredDeleteW.Call(
		uintptr(unsafe.Pointer(targetPtr)),
		credTypeGeneric,
		0,
	)
	if r == 0 {
		return fmt.Errorf("credman: CredDeleteW: %w", classifyError(e))
	}
	return nil
}

// HasSecret reports whether a credential is stored under key. It returns false
// for any error, including permission errors, to give a safe boolean answer.
func HasSecret(key string) bool {
	_, err := ReadSecret(key)
	return err == nil
}
