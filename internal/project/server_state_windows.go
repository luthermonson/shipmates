//go:build windows

package project

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsStateDirs struct {
	handles []windows.Handle
	path    string
}

func openWindowsStateDirHandle(path string, access uint32) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return windows.InvalidHandle, err
	}
	h, err := windows.CreateFile(p, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return windows.InvalidHandle, err
	}
	var info windows.ByHandleFileInformation
	if err = windows.GetFileInformationByHandle(h, &info); err != nil || info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		windows.CloseHandle(h)
		return windows.InvalidHandle, errors.New("unsafe server state directory")
	}
	return h, nil
}

func privateWindowsDACL(handle windows.Handle, inherit bool) error {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		return err
	}
	defer token.Close()
	u, err := token.GetTokenUser()
	if err != nil {
		return err
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return err
	}
	inheritance := uint32(0)
	if inherit {
		inheritance = windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT
	}
	entries := []windows.EXPLICIT_ACCESS{
		{AccessPermissions: windows.GENERIC_ALL, AccessMode: windows.SET_ACCESS, Inheritance: inheritance, Trustee: windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_USER, TrusteeValue: windows.TrusteeValueFromSID(u.User.Sid)}},
		{AccessPermissions: windows.GENERIC_ALL, AccessMode: windows.SET_ACCESS, Inheritance: inheritance, Trustee: windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_WELL_KNOWN_GROUP, TrusteeValue: windows.TrusteeValueFromSID(system)}},
	}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return err
	}
	if err = windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		return err
	}
	sd, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil || dacl.AceCount != 2 {
		return errors.New("server state DACL verification failed")
	}
	seenUser, seenSystem := false, false
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if windows.GetAce(dacl, i, &ace) != nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Mask&windows.GENERIC_ALL == 0 {
			return errors.New("server state DACL verification failed")
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		seenUser = seenUser || sid.Equals(u.User.Sid)
		seenSystem = seenSystem || sid.Equals(system)
	}
	if !seenUser || !seenSystem {
		return errors.New("server state DACL verification failed")
	}
	return nil
}

func (d *windowsStateDirs) close() {
	for i := len(d.handles) - 1; i >= 0; i-- {
		windows.CloseHandle(d.handles[i])
	}
}

func openWindowsStateDirs(root string, create bool) (*windowsStateDirs, error) {
	d := &windowsStateDirs{}
	p := root
	for _, part := range []string{"", Dir, SessionsDirName} {
		if part != "" {
			p = filepath.Join(p, part)
			if create {
				if e := os.Mkdir(p, 0o700); e != nil && !errors.Is(e, os.ErrExist) {
					d.close()
					return nil, e
				}
			}
		}
		access := uint32(windows.GENERIC_READ)
		if part == SessionsDirName {
			access |= windows.READ_CONTROL | windows.WRITE_DAC
		}
		h, e := openWindowsStateDirHandle(p, access)
		if e != nil {
			d.close()
			return nil, errors.New("unsafe server state directory")
		}
		d.handles = append(d.handles, h)
	}
	d.path = p
	if err := privateWindowsDACL(d.handles[len(d.handles)-1], true); err != nil {
		d.close()
		return nil, err
	}
	return d, nil
}
func EnsureServerStateDirectory(root string) error {
	d, e := openWindowsStateDirs(root, true)
	if d != nil {
		d.close()
	}
	return e
}

func openWindowsStateLeaf(d *windowsStateDirs, name string, access, create uint32) (windows.Handle, windowsFileIdentity, error) {
	p, e := windows.UTF16PtrFromString(filepath.Join(d.path, name))
	if e != nil {
		return windows.InvalidHandle, windowsFileIdentity{}, e
	}
	h, e := windows.CreateFile(p, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, create, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if e != nil {
		return windows.InvalidHandle, windowsFileIdentity{}, e
	}
	var i windows.ByHandleFileInformation
	if e = windows.GetFileInformationByHandle(h, &i); e != nil || i.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 || i.NumberOfLinks != 1 {
		windows.CloseHandle(h)
		return windows.InvalidHandle, windowsFileIdentity{}, errors.New("unsafe server state file")
	}
	id := windowsFileIdentity{volume: uint64(i.VolumeSerialNumber) | 1<<63, index: uint64(i.FileIndexHigh)<<32 | uint64(i.FileIndexLow) | 1<<63, size: uint64(i.FileSizeHigh)<<32 | uint64(i.FileSizeLow), write: i.LastWriteTime, links: i.NumberOfLinks}
	return h, id, nil
}
func ReadServerStateFile(root, name string, limit int64) ([]byte, error) {
	if name != "server.json" || limit <= 0 {
		return nil, errors.New("invalid server state file")
	}
	d, e := openWindowsStateDirs(root, false)
	if e != nil {
		return nil, e
	}
	defer d.close()
	h, before, e := openWindowsStateLeaf(d, name, windows.GENERIC_READ, windows.OPEN_EXISTING)
	if e != nil || before.size == 0 || before.size > uint64(limit) {
		if h != windows.InvalidHandle {
			windows.CloseHandle(h)
		}
		return nil, errors.New("unsafe server state file")
	}
	f := os.NewFile(uintptr(h), name)
	b, re := io.ReadAll(io.LimitReader(f, limit+1))
	var i windows.ByHandleFileInformation
	se := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &i)
	ce := f.Close()
	after := windowsFileIdentity{volume: uint64(i.VolumeSerialNumber) | 1<<63, index: uint64(i.FileIndexHigh)<<32 | uint64(i.FileIndexLow) | 1<<63, size: uint64(i.FileSizeHigh)<<32 | uint64(i.FileSizeLow), write: i.LastWriteTime, links: i.NumberOfLinks}
	if re != nil || se != nil || ce != nil || int64(len(b)) > limit || !sameWindowsIdentity(before, after) {
		return nil, errors.New("server state changed during read")
	}
	return b, nil
}
func WriteServerStateFile(root, name string, raw []byte) error {
	if name != "server.json" || len(raw) == 0 || len(raw) > 4096 {
		return errors.New("invalid server state file")
	}
	d, e := openWindowsStateDirs(root, true)
	if e != nil {
		return e
	}
	defer d.close()
	if h, _, x := openWindowsStateLeaf(d, name, windows.GENERIC_READ, windows.OPEN_EXISTING); x == nil {
		windows.CloseHandle(h)
	} else if !errors.Is(x, windows.ERROR_FILE_NOT_FOUND) {
		return x
	}
	tmp, e := os.CreateTemp(d.path, ".server-ready-*")
	if e != nil {
		return e
	}
	tn := tmp.Name()
	defer os.Remove(tn)
	if e = privateWindowsDACL(windows.Handle(tmp.Fd()), false); e == nil {
		e = tmp.Chmod(0o600)
	}
	if e == nil {
		_, e = tmp.Write(raw)
	}
	if e == nil {
		e = tmp.Sync()
	}
	e = errors.Join(e, tmp.Close())
	if e != nil {
		return e
	}
	from, _ := windows.UTF16PtrFromString(tn)
	to, _ := windows.UTF16PtrFromString(filepath.Join(d.path, name))
	if e = windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); e != nil {
		return e
	}
	got, e := ReadServerStateFile(root, name, 4096)
	if e != nil || string(got) != string(raw) {
		return errors.New("server state replacement verification failed")
	}
	return nil
}
func RemoveOwnedServerStateFile(root, name string, owned []byte) error {
	if name != "server.json" || len(owned) == 0 {
		return errors.New("invalid server state file")
	}
	d, e := openWindowsStateDirs(root, false)
	if e != nil {
		return e
	}
	defer d.close()
	h, _, e := openWindowsStateLeaf(d, name, windows.GENERIC_READ|windows.DELETE, windows.OPEN_EXISTING)
	if e != nil {
		return e
	}
	f := os.NewFile(uintptr(h), name)
	b, re := io.ReadAll(io.LimitReader(f, 4097))
	if re != nil || string(b) != string(owned) {
		f.Close()
		return errors.New("server state ownership changed")
	}
	dispose := byte(1)
	e = windows.SetFileInformationByHandle(windows.Handle(f.Fd()), windows.FileDispositionInfo, &dispose, uint32(unsafe.Sizeof(dispose)))
	return errors.Join(e, f.Close())
}
func OpenServerLog(root string) (*os.File, error) {
	d, e := openWindowsStateDirs(root, true)
	if e != nil {
		return nil, e
	}
	defer d.close()
	h, _, e := openWindowsStateLeaf(d, "server.log", windows.FILE_APPEND_DATA|windows.FILE_READ_ATTRIBUTES|windows.FILE_WRITE_ATTRIBUTES, windows.OPEN_ALWAYS)
	if e != nil {
		return nil, e
	}
	f := os.NewFile(uintptr(h), "server.log")
	if e = privateWindowsDACL(windows.Handle(f.Fd()), false); e == nil {
		e = f.Chmod(0o600)
	}
	if e != nil {
		f.Close()
		return nil, e
	}
	return f, nil
}
