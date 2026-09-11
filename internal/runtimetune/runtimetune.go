// Package runtimetune applies memory-related Go runtime settings at boot so the
// service's footprint tracks the container it runs in. It sets the GC target
// (GOGC) and derives a soft memory limit (GOMEMLIMIT) from the cgroup memory
// limit — keeping the heap bounded and container RSS predictable across the
// bursty libvips decode/encode workload. It complements the engine's idle
// scavenge (debug.FreeOSMemory), which returns reclaimed pages to the OS after a
// burst.
//
// Note: libvips also allocates NATIVE (off-heap) memory that GOMEMLIMIT does not
// govern (like ffmpeg's child-process RSS in audio-service), so the derived limit
// is a Go-heap backstop, not a cgroup-wide OOM guard — leave headroom via a
// ratio below 1. See MEMORY_TUNING.md.
package runtimetune

import (
	"io/fs"
	"log/slog"
	"math"
	"os"
	"path"
	"runtime/debug"
	"strconv"
	"strings"

	"github.com/ogen-app/image-service/internal/config"
)

// cgroup memory-limit files, relative to the filesystem root (no leading slash,
// as io/fs requires). v2 is tried first, then the v1 fallback.
const (
	cgroupV2Max   = "sys/fs/cgroup/memory.max"
	cgroupV1Limit = "sys/fs/cgroup/memory/memory.limit_in_bytes"
)

// unlimitedThreshold is the floor above which a cgroup value is treated as "no
// limit". cgroup v1 reports no-limit as a huge page-count sentinel
// (~0x7FFFFFFFFFFFF000); anything near the int64 ceiling means unbounded and
// would make a ratio meaningless.
const unlimitedThreshold = int64(1) << 62

// Apply sets GOGC and, unless GOMEMLIMIT is already set in the environment, a
// soft memory limit derived from the cgroup limit. It never fails the boot: a
// missing or unreadable cgroup (e.g. local dev on macOS) just leaves the runtime
// default in place. Call it once, early in main.
func Apply(logger *slog.Logger, cfg *config.Config) {
	apply(logger, cfg, os.DirFS("/"), os.LookupEnv)
}

// apply is the testable core: the filesystem root and env lookup are injected so
// the cgroup-derived path can be exercised without touching the real /sys.
func apply(logger *slog.Logger, cfg *config.Config, root fs.FS, lookupEnv func(string) (string, bool)) {
	if cfg.GCPercent > 0 {
		debug.SetGCPercent(cfg.GCPercent)
		logger.Info("gc percent set", "component", "runtimetune", "gogc", cfg.GCPercent)
	}

	if v, ok := lookupEnv("GOMEMLIMIT"); ok {
		// The runtime already applied an operator-set GOMEMLIMIT at startup; don't
		// override it.
		logger.Info("memory limit from env", "component", "runtimetune", "gomemlimit", v)
		return
	}
	if cfg.MemoryLimitRatio <= 0 {
		return
	}

	limit, ok := cgroupMemoryLimit(root)
	if !ok {
		logger.Info("no cgroup memory limit found; leaving GOMEMLIMIT unset",
			"component", "runtimetune")
		return
	}
	// Validate before the int64 conversion: NaN/±Inf and any product outside the
	// int64 range convert to an implementation-defined value. (The <=0 guard above
	// rejects non-positive ratios but not NaN, since NaN <= 0 is false.)
	product := float64(limit) * cfg.MemoryLimitRatio
	if math.IsNaN(product) || product >= float64(math.MaxInt64) || product < float64(math.MinInt64) {
		logger.Warn("invalid or out-of-range memory limit ratio; leaving GOMEMLIMIT unset",
			"component", "runtimetune", "ratio", cfg.MemoryLimitRatio)
		return
	}
	soft := int64(product)
	if soft <= 0 {
		return
	}
	debug.SetMemoryLimit(soft)
	logger.Info("soft memory limit derived from cgroup",
		"component", "runtimetune",
		"cgroup_limit_bytes", limit,
		"gomemlimit_bytes", soft,
		"ratio", cfg.MemoryLimitRatio,
	)
}

// cgroupMemoryLimit returns the container's memory limit in bytes. It resolves
// the PROCESS's own cgroup from /proc/self/cgroup first and reads the limit file
// under that subpath, then falls back to the cgroup mount root. This matters when
// the process is NOT in the root cgroup (cgroupns=host, or nested cgroups): the
// root's memory.max is then the host/parent limit, not the container's, so reading
// it blindly would derive GOMEMLIMIT from the wrong number. v2 (unified) is tried
// before the v1 fallback. ok is false when no finite limit is configured (the
// "max"/huge-sentinel values) or the files are absent — the caller then leaves
// GOMEMLIMIT unset.
func cgroupMemoryLimit(root fs.FS) (int64, bool) {
	v2rel, v1rel := cgroupPaths(root)
	for _, name := range candidatePaths("sys/fs/cgroup", v2rel, "memory.max", cgroupV2Max) {
		if v, ok := readLimit(root, name); ok {
			return v, true
		}
	}
	for _, name := range candidatePaths("sys/fs/cgroup/memory", v1rel, "memory.limit_in_bytes", cgroupV1Limit) {
		if v, ok := readLimit(root, name); ok {
			return v, true
		}
	}
	return 0, false
}

// cgroupPaths reads /proc/self/cgroup and returns the process's cgroup subpaths
// for the v2 unified hierarchy and the v1 memory controller (each relative, no
// leading slash). Empty when the file is absent (non-Linux) or a line is missing.
// Lines are "hierarchy-ID:controller-list:cgroup-path"; the unified v2 line has
// hierarchy-ID 0 and an empty controller list.
func cgroupPaths(root fs.FS) (v2rel, v1rel string) {
	b, err := fs.ReadFile(root, "proc/self/cgroup")
	if err != nil {
		return "", ""
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		f := strings.SplitN(line, ":", 3)
		if len(f) != 3 {
			continue
		}
		rel := strings.TrimPrefix(f[2], "/") // relative for path.Join
		if f[0] == "0" {                     // cgroup v2 unified hierarchy
			v2rel = rel
			continue
		}
		for _, ctrl := range strings.Split(f[1], ",") {
			if ctrl == "memory" {
				v1rel = rel
			}
		}
	}
	return v2rel, v1rel
}

// candidatePaths returns the limit files to try in order: the process's resolved
// cgroup dir first, then the mount root as a fallback. When the resolved path is
// the root (rel empty, e.g. cgroupns=private), only the root is returned so we
// don't read the same file twice.
func candidatePaths(mount, rel, file, rootFallback string) []string {
	resolved := path.Join(mount, rel, file)
	if resolved == rootFallback {
		return []string{rootFallback}
	}
	return []string{resolved, rootFallback}
}

// readLimit parses a single cgroup limit file, rejecting the empty/"max"/huge
// sentinels that mean "unlimited".
func readLimit(root fs.FS, name string) (int64, bool) {
	b, err := fs.ReadFile(root, name)
	if err != nil {
		return 0, false
	}
	s := strings.TrimSpace(string(b))
	if s == "" || s == "max" { // cgroup v2 spells "unlimited" as "max"
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 || n >= unlimitedThreshold {
		return 0, false
	}
	return n, true
}
