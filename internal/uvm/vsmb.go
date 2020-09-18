package uvm

import (
	"bytes"
	"context"
	"encoding/gob"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/Microsoft/hcsshim/internal/requesttype"
	hcsschema "github.com/Microsoft/hcsshim/internal/schema2"
)

const vsmbSharePrefix = `\\?\VMSMB\VSMB-{dcc079ae-60ba-4d07-847c-3493609c0870}\`

// VSMBShare contains the host path for a Vsmb Mount
type VSMBShare struct {
	// UVM the resource belongs to
	vm           *UtilityVM
	HostPath     string
	refCount     uint32
	Name         string
	AllowedFiles []string
	GuestPath    string
	Options      hcsschema.VirtualSmbShareOptions
}

// Release frees the resources of the corresponding vsmb Mount
func (vsmb *VSMBShare) Release(ctx context.Context) error {
	if err := vsmb.vm.RemoveVSMB(ctx, vsmb.HostPath, vsmb.Options.ReadOnly); err != nil {
		return fmt.Errorf("failed to remove VSMB share: %s", err)
	}
	return nil
}

// DefaultVSMBOptions returns the default VSMB options. If readOnly is specified,
// returns the default VSMB options for a readonly share.
func (uvm *UtilityVM) DefaultVSMBOptions(readOnly bool) *hcsschema.VirtualSmbShareOptions {
	opts := &hcsschema.VirtualSmbShareOptions{
		NoDirectmap: uvm.DevicesPhysicallyBacked(),
	}
	if readOnly {
		opts.ShareRead = true
		opts.CacheIo = true
		opts.ReadOnly = true
		opts.PseudoOplocks = true
	}
	return opts
}

// findVSMBShare finds a share by `hostPath`. If not found returns `ErrNotAttached`.
func (uvm *UtilityVM) findVSMBShare(ctx context.Context, m map[string]*VSMBShare, shareKey string) (*VSMBShare, error) {
	share, ok := m[shareKey]
	if !ok {
		return nil, ErrNotAttached
	}
	return share, nil
}

// AddVSMB adds a VSMB share to a Windows utility VM. Each VSMB share is ref-counted and
// only added if it isn't already. This is used for read-only layers, mapped directories
// to a container, and for mapped pipes.
func (uvm *UtilityVM) AddVSMB(ctx context.Context, hostPath string, options *hcsschema.VirtualSmbShareOptions) (*VSMBShare, error) {
	if uvm.operatingSystem != "windows" {
		return nil, errNotSupported
	}

	uvm.m.Lock()
	defer uvm.m.Unlock()

	// Temporary support to allow single-file mapping. If `hostPath` is a
	// directory, map it without restriction. However, if it is a file, map the
	// directory containing the file, and use `AllowedFileList` to only allow
	// access to that file. If the directory has been mapped before for
	// single-file use, add the new file to the `AllowedFileList` and issue an
	// Update operation.
	st, err := os.Stat(hostPath)
	if err != nil {
		return nil, err
	}
	var file string
	m := uvm.vsmbDirShares
	if !st.IsDir() {
		m = uvm.vsmbFileShares
		file = hostPath
		hostPath = filepath.Dir(hostPath)
		options.RestrictFileAccess = true
		options.SingleFileMapping = true
	}
	hostPath = filepath.Clean(hostPath)
	var requestType = requesttype.Update
	shareKey := getVSMBShareKey(hostPath, options.ReadOnly)
	share, err := uvm.findVSMBShare(ctx, m, shareKey)
	if err == ErrNotAttached {
		requestType = requesttype.Add
		uvm.vsmbCounter++
		shareName := "s" + strconv.FormatUint(uvm.vsmbCounter, 16)

		share = &VSMBShare{
			vm:        uvm,
			Name:      shareName,
			GuestPath: vsmbSharePrefix + shareName,
			HostPath:  hostPath,
		}
	}
	newAllowedFiles := share.AllowedFiles
	if options.RestrictFileAccess {
		newAllowedFiles = append(newAllowedFiles, file)
	}

	// Update on a VSMB share currently only supports updating the
	// AllowedFileList, and in fact will return an error if RestrictFileAccess
	// isn't set (e.g. if used on an unrestricted share). So we only call Modify
	// if we are either doing an Add, or if RestrictFileAccess is set.
	if requestType == requesttype.Add || options.RestrictFileAccess {
		modification := &hcsschema.ModifySettingRequest{
			RequestType: requestType,
			Settings: hcsschema.VirtualSmbShare{
				Name:         share.Name,
				Options:      options,
				Path:         hostPath,
				AllowedFiles: newAllowedFiles,
			},
			ResourcePath: vSmbShareResourcePath,
		}
		if err := uvm.modify(ctx, modification); err != nil {
			return nil, err
		}
	}

	share.AllowedFiles = newAllowedFiles
	share.refCount++
	share.Options = *options
	m[shareKey] = share
	return share, nil
}

// RemoveVSMB removes a VSMB share from a utility VM. Each VSMB share is ref-counted
// and only actually removed when the ref-count drops to zero.
func (uvm *UtilityVM) RemoveVSMB(ctx context.Context, hostPath string, readOnly bool) error {
	if uvm.operatingSystem != "windows" {
		return errNotSupported
	}

	uvm.m.Lock()
	defer uvm.m.Unlock()

	st, err := os.Stat(hostPath)
	if err != nil {
		return err
	}
	m := uvm.vsmbDirShares
	if !st.IsDir() {
		m = uvm.vsmbFileShares
		hostPath = filepath.Dir(hostPath)
	}
	hostPath = filepath.Clean(hostPath)
	shareKey := getVSMBShareKey(hostPath, readOnly)
	share, err := uvm.findVSMBShare(ctx, m, shareKey)
	if err != nil {
		return fmt.Errorf("%s is not present as a VSMB share in %s, cannot remove", hostPath, uvm.id)
	}

	share.refCount--
	if share.refCount > 0 {
		return nil
	}

	modification := &hcsschema.ModifySettingRequest{
		RequestType:  requesttype.Remove,
		Settings:     hcsschema.VirtualSmbShare{Name: share.Name},
		ResourcePath: vSmbShareResourcePath,
	}
	if err := uvm.modify(ctx, modification); err != nil {
		return fmt.Errorf("failed to remove vsmb share %s from %s: %+v: %s", hostPath, uvm.id, modification, err)
	}

	delete(m, shareKey)
	return nil
}

// GetVSMBUvmPath returns the guest path of a VSMB mount.
func (uvm *UtilityVM) GetVSMBUvmPath(ctx context.Context, hostPath string, readOnly bool) (string, error) {
	if hostPath == "" {
		return "", fmt.Errorf("no hostPath passed to GetVSMBUvmPath")
	}

	uvm.m.Lock()
	defer uvm.m.Unlock()

	st, err := os.Stat(hostPath)
	if err != nil {
		return "", err
	}
	m := uvm.vsmbDirShares
	f := ""
	if !st.IsDir() {
		m = uvm.vsmbFileShares
		hostPath, f = filepath.Split(hostPath)
	}
	hostPath = filepath.Clean(hostPath)
	shareKey := getVSMBShareKey(hostPath, readOnly)
	share, err := uvm.findVSMBShare(ctx, m, shareKey)
	if err != nil {
		return "", err
	}
	return filepath.Join(share.GuestPath, f), nil
}

var _ = (Cloneable)(&VSMBShare{})

// serializes the VSMBShare struct
func (vsmb *VSMBShare) GobEncode() ([]byte, error) {
	var buf bytes.Buffer
	encoder := gob.NewEncoder(&buf)
	errMsgFmt := "failed to encode VSMBShare: %s"
	// encode only the fields that can be safely deserialized.
	if err := encoder.Encode(vsmb.HostPath); err != nil {
		return []byte{}, fmt.Errorf(errMsgFmt, err)
	}
	if err := encoder.Encode(vsmb.Name); err != nil {
		return []byte{}, fmt.Errorf(errMsgFmt, err)
	}
	if err := encoder.Encode(vsmb.AllowedFiles); err != nil {
		return []byte{}, fmt.Errorf(errMsgFmt, err)
	}
	if err := encoder.Encode(vsmb.GuestPath); err != nil {
		return []byte{}, fmt.Errorf(errMsgFmt, err)
	}
	if err := encoder.Encode(vsmb.Options); err != nil {
		return []byte{}, fmt.Errorf(errMsgFmt, err)
	}
	return buf.Bytes(), nil
}

// deserializes the VSMBShare struct into the struct on which this is called (i.e the vsmb pointer)
func (vsmb *VSMBShare) GobDecode(data []byte) error {
	buf := bytes.NewBuffer(data)
	decoder := gob.NewDecoder(buf)
	errMsgFmt := "failed to decode VSMBShare: %s"
	// fields should be decoded in the same order in which they were encoded.
	if err := decoder.Decode(&vsmb.HostPath); err != nil {
		return fmt.Errorf(errMsgFmt, err)
	}
	if err := decoder.Decode(&vsmb.Name); err != nil {
		return fmt.Errorf(errMsgFmt, err)
	}
	if err := decoder.Decode(&vsmb.AllowedFiles); err != nil {
		return fmt.Errorf(errMsgFmt, err)
	}
	if err := decoder.Decode(&vsmb.GuestPath); err != nil {
		return fmt.Errorf(errMsgFmt, err)
	}
	if err := decoder.Decode(&vsmb.Options); err != nil {
		return fmt.Errorf(errMsgFmt, err)
	}
	return nil
}

// To clone VSMB share we just need to add it into the config doc of that VM and increase the
// vsmb counter.
func (vsmb *VSMBShare) Clone(ctx context.Context, vm *UtilityVM, cd *cloneData) (interface{}, error) {
	cd.doc.VirtualMachine.Devices.VirtualSmb.Shares = append(cd.doc.VirtualMachine.Devices.VirtualSmb.Shares, hcsschema.VirtualSmbShare{
		Name:         vsmb.Name,
		Path:         vsmb.HostPath,
		Options:      &vsmb.Options,
		AllowedFiles: vsmb.AllowedFiles,
	})
	vm.vsmbCounter++

	clonedVSMB := &VSMBShare{
		vm:           vm,
		HostPath:     vsmb.HostPath,
		refCount:     1,
		Name:         vsmb.Name,
		Options:      vsmb.Options,
		AllowedFiles: vsmb.AllowedFiles,
		GuestPath:    vsmb.GuestPath,
	}

	vm.vsmbDirShares[vsmb.HostPath] = clonedVSMB

	return clonedVSMB, nil
}

// getVSMBShareKey returns a string key which encapsulates the information that
// is used to look up an existing VSMB share. If a share is being added, but
// there is an existing share with the same key, the existing share will be used
// instead (and its ref count incremented).
func getVSMBShareKey(hostPath string, readOnly bool) string {
	return fmt.Sprintf("%v-%v", hostPath, readOnly)
}
