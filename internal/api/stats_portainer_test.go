package api

import "testing"

func TestNormalizeDockerStatsForPortainer(t *testing.T) {
	t.Parallel()

	s := dockerStats{
		CPUStats: cpuStats{
			OnlineCPUs: 2,
			CPUUsage: cpuUsage{
				TotalUsage: 11,
			},
		},
		PreCPUStats: cpuStats{
			CPUUsage: cpuUsage{},
		},
		MemoryStats: memStats{
			Usage: 7,
			Limit: 99,
			Stats: map[string]uint64{
				"file":          5,
				"active_file":   3,
				"inactive_file": 2,
				"anon":          4,
			},
		},
	}

	normalizeDockerStats(&s)

	if s.Networks == nil || s.StorageStats == nil {
		t.Fatalf("expected normalized empty maps, got networks=%#v storage=%#v", s.Networks, s.StorageStats)
	}
	if s.BlkioStats.IOServiceBytesRecursive == nil || s.BlkioStats.IOServicedRecursive == nil {
		t.Fatalf("expected normalized blkio arrays, got %#v", s.BlkioStats)
	}
	if len(s.CPUStats.CPUUsage.PercpuUsage) != 2 {
		t.Fatalf("expected current percpu_usage length 2, got %#v", s.CPUStats.CPUUsage.PercpuUsage)
	}
	if len(s.PreCPUStats.CPUUsage.PercpuUsage) != 1 {
		t.Fatalf("expected precpu_usage fallback length 1, got %#v", s.PreCPUStats.CPUUsage.PercpuUsage)
	}
	for _, key := range []string{
		"cache",
		"total_cache",
		"active_file",
		"total_active_file",
		"inactive_file",
		"total_inactive_file",
		"rss",
		"total_rss",
		"pgfault",
		"total_pgfault",
		"pgmajfault",
		"total_pgmajfault",
		"mapped_file",
		"total_mapped_file",
		"writeback",
		"total_writeback",
		"unevictable",
		"total_unevictable",
		"hierarchical_memory_limit",
		"hierarchical_memsw_limit",
	} {
		if _, ok := s.MemoryStats.Stats[key]; !ok {
			t.Fatalf("expected memory stats key %q in %#v", key, s.MemoryStats.Stats)
		}
	}
	if s.MemoryStats.Stats["cache"] != 5 || s.MemoryStats.Stats["rss"] != 4 {
		t.Fatalf("expected cache/rss aliases from file+anon, got %#v", s.MemoryStats.Stats)
	}
	if s.MemoryStats.Stats["hierarchical_memory_limit"] != 99 || s.MemoryStats.Stats["hierarchical_memsw_limit"] != 99 {
		t.Fatalf("expected hierarchical limits from limit, got %#v", s.MemoryStats.Stats)
	}
}

func TestNormalizeMemoryStatsPreservesExplicitValues(t *testing.T) {
	t.Parallel()

	ms := memStats{
		Limit: 123,
		Stats: map[string]uint64{
			"cache":             8,
			"rss":               9,
			"pgfault":           10,
			"pgmajfault":        11,
			"mapped_file":       12,
			"writeback":         13,
			"unevictable":       14,
			"total_cache":       15,
			"hierarchical_memory_limit": 16,
		},
	}

	normalizeMemoryStats(&ms)

	if ms.Stats["cache"] != 8 || ms.Stats["rss"] != 9 || ms.Stats["total_cache"] != 15 {
		t.Fatalf("expected explicit values preserved, got %#v", ms.Stats)
	}
	if ms.Stats["hierarchical_memory_limit"] != 16 {
		t.Fatalf("expected explicit hierarchical_memory_limit preserved, got %#v", ms.Stats)
	}
	if ms.Stats["hierarchical_memsw_limit"] != 123 {
		t.Fatalf("expected memsw limit defaulted from limit, got %#v", ms.Stats)
	}
}
