//go:build !linux || !cgo

package imgengine

// trimNativeHeap is a no-op off glibc/Linux (e.g. local dev on macOS, whose
// allocator returns memory on its own).
func trimNativeHeap() {}

// residentBytes is unavailable off Linux.
func residentBytes() int64 { return 0 }
