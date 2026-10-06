//go:build linux && cgo

package imgengine

/*
#include <malloc.h>
*/
import "C"

import (
	"os"
	"strconv"
	"strings"
)

// trimNativeHeap hands glibc's freed-but-retained pages back to the OS.
// debug.FreeOSMemory only covers the Go heap; libvips allocates its pixel
// buffers through glibc malloc, which keeps freed chunks in its arenas (one per
// thread that ever called into C, up to 8×cores) instead of unmapping them.
// That retained native memory is the post-burst RSS plateau. malloc_trim(0)
// madvises the free pages in every arena back to the kernel.
func trimNativeHeap() {
	C.malloc_trim(0)
}

// residentBytes reports the process RSS from /proc/self/statm (0 if unreadable),
// so the scavenge log shows how much each reclaim actually returned.
func residentBytes() int64 {
	b, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) < 2 {
		return 0
	}
	pages, err := strconv.ParseInt(f[1], 10, 64)
	if err != nil {
		return 0
	}
	return pages * int64(os.Getpagesize())
}
