//go:build windows

package security

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows security information constants used when calling SetNamedSecurityInfoW.
const (
	seFileObject = 1 // SE_FILE_OBJECT — target is a file-system object

	// GENERIC_ALL grants full control (read, write, execute, and all special
	// rights) when used as an access mask in an ACE.
	genericAll = 0x10000000
)

var (
	// procSetNamedSecurityInfoW is looked up from advapi32.dll, which is
	// already loaded via modadvapi32 declared in credman.go.
	procSetNamedSecurityInfoW = modadvapi32.NewProc("SetNamedSecurityInfoW")
)

// GetCurrentUserSID returns the SID of the account under which the current
// process is running. The returned SID is heap-allocated and owned by the
// caller; it remains valid after the underlying token is closed.
func GetCurrentUserSID() (*windows.SID, error) {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return nil, fmt.Errorf("acl: OpenCurrentProcessToken: %w", err)
	}
	defer token.Close()

	tu, err := token.GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("acl: GetTokenUser: %w", err)
	}

	sid, err := tu.User.Sid.Copy()
	if err != nil {
		return nil, fmt.Errorf("acl: copy user SID: %w", err)
	}
	return sid, nil
}

// buildWellKnownSID allocates and returns a well-known SID identified by
// sidType. The caller is responsible for freeing the SID if necessary; in
// practice the GC handles this because windows.SID is a Go-allocated value.
func buildWellKnownSID(sidType windows.WELL_KNOWN_SID_TYPE) (*windows.SID, error) {
	sid, err := windows.CreateWellKnownSid(sidType)
	if err != nil {
		return nil, fmt.Errorf("acl: CreateWellKnownSid(%d): %w", sidType, err)
	}
	return sid, nil
}

// LocalServiceSID returns the built-in LocalService SID (S-1-5-19).
func LocalServiceSID() (*windows.SID, error) {
	return buildWellKnownSID(windows.WinLocalServiceSid)
}

// LockDirToService applies a protected DACL granting GENERIC_ALL to the
// current user, SYSTEM, and LocalService. Call this at --install-service time
// so the service account can read and write the data directory.
func LockDirToService(path string) error {
	userSID, err := GetCurrentUserSID()
	if err != nil {
		return err
	}
	systemSID, err := buildWellKnownSID(windows.WinLocalSystemSid)
	if err != nil {
		return err
	}
	lsSID, err := buildWellKnownSID(windows.WinLocalServiceSid)
	if err != nil {
		return err
	}

	inheritFlags := windows.CONTAINER_INHERIT_ACE | windows.OBJECT_INHERIT_ACE

	dacl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{
		{
			AccessPermissions: windows.ACCESS_MASK(genericAll),
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       uint32(inheritFlags),
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_USER,
				TrusteeValue: windows.TrusteeValueFromSID(userSID),
			},
		},
		{
			AccessPermissions: windows.ACCESS_MASK(genericAll),
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       uint32(inheritFlags),
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
				TrusteeValue: windows.TrusteeValueFromSID(systemSID),
			},
		},
		{
			AccessPermissions: windows.ACCESS_MASK(genericAll),
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       uint32(inheritFlags),
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
				TrusteeValue: windows.TrusteeValueFromSID(lsSID),
			},
		},
	}, nil)
	if err != nil {
		return fmt.Errorf("acl: ACLFromEntries: %w", err)
	}

	return applyDACL(path, dacl)
}

// GrantFileToSIDs applies a protected, non-inheriting DACL to a single file
// granting GENERIC_ALL to each SID. Used to lock down service-key.enc.
func GrantFileToSIDs(path string, sids ...*windows.SID) error {
	entries := make([]windows.EXPLICIT_ACCESS, len(sids))
	for i, sid := range sids {
		entries[i] = windows.EXPLICIT_ACCESS{
			AccessPermissions: windows.ACCESS_MASK(genericAll),
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_UNKNOWN,
				TrusteeValue: windows.TrusteeValueFromSID(sid),
			},
		}
	}

	dacl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return fmt.Errorf("acl: ACLFromEntries: %w", err)
	}
	return applyDACL(path, dacl)
}

// applyDACL sets a protected DACL on path via SetNamedSecurityInfoW.
func applyDACL(path string, dacl *windows.ACL) error {
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return fmt.Errorf("acl: encode path: %w", err)
	}
	secInfo := windows.SECURITY_INFORMATION(
		windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION,
	)
	ret, _, e := procSetNamedSecurityInfoW.Call(
		uintptr(unsafe.Pointer(pathPtr)),
		seFileObject,
		uintptr(secInfo),
		0, 0,
		uintptr(unsafe.Pointer(dacl)),
		0,
	)
	if ret != 0 {
		return fmt.Errorf("acl: SetNamedSecurityInfoW(%q): %w", path, e)
	}
	return nil
}

// LockDirToCurrentUser applies a protected DACL to path so that only the
// current user and the built-in SYSTEM account retain access. The DACL is
// marked protected, which blocks ACE inheritance from the parent directory.
//
// The resulting DACL contains exactly two Allow ACEs:
//  1. Current user — GENERIC_ALL with container and object inheritance
//  2. SYSTEM       — GENERIC_ALL with container and object inheritance
//
// All other principals have no access because there are no additional Allow or
// Deny ACEs and the DACL is non-null (a non-null empty DACL denies everyone).
func LockDirToCurrentUser(path string) error {
	userSID, err := GetCurrentUserSID()
	if err != nil {
		return err
	}

	systemSID, err := buildWellKnownSID(windows.WinLocalSystemSid)
	if err != nil {
		return err
	}

	inheritFlags := windows.CONTAINER_INHERIT_ACE | windows.OBJECT_INHERIT_ACE

	dacl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{
		{
			AccessPermissions: windows.ACCESS_MASK(genericAll),
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       uint32(inheritFlags),
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_USER,
				TrusteeValue: windows.TrusteeValueFromSID(userSID),
			},
		},
		{
			AccessPermissions: windows.ACCESS_MASK(genericAll),
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       uint32(inheritFlags),
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
				TrusteeValue: windows.TrusteeValueFromSID(systemSID),
			},
		},
	}, nil)
	if err != nil {
		return fmt.Errorf("acl: ACLFromEntries: %w", err)
	}

	return applyDACL(path, dacl)
}
