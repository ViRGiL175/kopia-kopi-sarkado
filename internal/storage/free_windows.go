//go:build windows

package storage

import (
	"fmt"
	"syscall"
	"unsafe"
)

var (
	kernel32                = syscall.NewLazyDLL("kernel32.dll")
	getDiskFreeSpaceExWProc = kernel32.NewProc("GetDiskFreeSpaceExW")
)

func FreeBytes(path string) (int64, error) {
	pathPtr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, fmt.Errorf("encode path %q: %w", path, err)
	}

	var freeBytesAvailable int64
	result, _, callErr := getDiskFreeSpaceExWProc.Call(
		uintptr(unsafe.Pointer(pathPtr)),
		uintptr(unsafe.Pointer(&freeBytesAvailable)),
		0,
		0,
	)
	if result == 0 {
		if callErr != nil && callErr != syscall.Errno(0) {
			return 0, fmt.Errorf("GetDiskFreeSpaceExW %q: %w", path, callErr)
		}

		return 0, fmt.Errorf("GetDiskFreeSpaceExW %q failed", path)
	}

	return freeBytesAvailable, nil
}
