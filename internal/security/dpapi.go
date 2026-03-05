//go:build windows

package security

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// cryptProtectLocalMachine causes CryptProtectData to use the machine master
// key rather than the per-user master key. Any process on this machine can
// decrypt the result; the ciphertext is unusable on any other machine.
const cryptProtectLocalMachine uint32 = 0x4

// dataBlob mirrors the Windows DATA_BLOB structure used by CryptProtectData
// and CryptUnprotectData.
type dataBlob struct {
	cbData uint32
	pbData *byte
}

var (
	modCrypt32             = windows.NewLazySystemDLL("crypt32.dll")
	procCryptProtectData   = modCrypt32.NewProc("CryptProtectData")
	procCryptUnprotectData = modCrypt32.NewProc("CryptUnprotectData")
)

// EncryptMachine encrypts plaintext using DPAPI with the machine master key.
// The result is bound to this machine and can be decrypted by any process
// that can read the file (ACL the file to restrict access further).
// Does not zero plaintext — the caller is responsible for that.
func EncryptMachine(plaintext []byte) ([]byte, error) {
	if len(plaintext) == 0 {
		return nil, fmt.Errorf("dpapi: plaintext must not be empty")
	}

	in := dataBlob{cbData: uint32(len(plaintext)), pbData: &plaintext[0]}
	var out dataBlob

	r, _, e := procCryptProtectData.Call(
		uintptr(unsafe.Pointer(&in)),
		0, // szDataDescr — optional label, unused
		0, // pOptionalEntropy — none
		0, // pvReserved
		0, // pPromptStruct — no UI
		uintptr(cryptProtectLocalMachine),
		uintptr(unsafe.Pointer(&out)),
	)
	if r == 0 {
		return nil, fmt.Errorf("dpapi: CryptProtectData: %w", e)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.pbData)))

	result := make([]byte, out.cbData)
	copy(result, unsafe.Slice(out.pbData, out.cbData))
	return result, nil
}

// DecryptMachine decrypts a blob previously encrypted with EncryptMachine.
// The caller must zero the returned slice when done via clear().
func DecryptMachine(ciphertext []byte) ([]byte, error) {
	if len(ciphertext) == 0 {
		return nil, fmt.Errorf("dpapi: ciphertext must not be empty")
	}

	in := dataBlob{cbData: uint32(len(ciphertext)), pbData: &ciphertext[0]}
	var out dataBlob

	r, _, e := procCryptUnprotectData.Call(
		uintptr(unsafe.Pointer(&in)),
		0, // ppszDataDescr — discard label
		0, // pOptionalEntropy
		0, // pvReserved
		0, // pPromptStruct
		uintptr(cryptProtectLocalMachine),
		uintptr(unsafe.Pointer(&out)),
	)
	if r == 0 {
		return nil, fmt.Errorf("dpapi: CryptUnprotectData: %w", e)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.pbData)))

	result := make([]byte, out.cbData)
	copy(result, unsafe.Slice(out.pbData, out.cbData))
	return result, nil
}
