//go:build !windows && !linux

package ffx

// Systems outside the two mainstream provider targets report 0, which the
// scheduler reads as "unknown" and leaves unconstrained rather than excluding.
func MemTotalMB() uint64            { return 0 }
func DiskFreeMB(path string) uint64 { return 0 }
