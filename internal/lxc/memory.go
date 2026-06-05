package lxc

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// ParseMemorySize parses a human memory size such as "16G", "32768M", "512K",
// "16GiB", or a plain byte count, returning the value in bytes. An empty
// string or "0" returns 0, meaning "no explicit limit". Suffixes are
// base-1024 and case-insensitive; a trailing "b" or "ib" is tolerated.
func ParseMemorySize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0, nil
	}
	num := strings.TrimSuffix(strings.TrimSuffix(strings.ToLower(s), "ib"), "b")
	mult := int64(1)
	if n := len(num); n > 0 {
		switch num[n-1] {
		case 'k':
			mult, num = 1<<10, num[:n-1]
		case 'm':
			mult, num = 1<<20, num[:n-1]
		case 'g':
			mult, num = 1<<30, num[:n-1]
		case 't':
			mult, num = 1<<40, num[:n-1]
		}
	}
	val, err := strconv.ParseInt(strings.TrimSpace(num), 10, 64)
	if err != nil || val < 0 {
		return 0, fmt.Errorf("invalid memory size %q", s)
	}
	return val * mult, nil
}

// hostMemoryBytes returns the host's total RAM (MemTotal) in bytes, or a
// conservative fallback if /proc/meminfo can't be read. Used as the PVE
// memory default when neither --memory, a dld.memory label, nor
// --default-memory is set, so unconstrained CTs match Docker's
// unlimited-by-default semantics instead of a low placeholder.
func hostMemoryBytes() int64 {
	if data, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if !strings.HasPrefix(line, "MemTotal:") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				if kb, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
					return kb * 1024
				}
			}
		}
	}
	return 8 << 30 // 8 GiB fallback
}
