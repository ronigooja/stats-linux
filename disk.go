package main

import (
	"context"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/shirou/gopsutil/v4/disk"
)

func realFilesystem(fs string) bool {
	switch fs {
	case "", "tmpfs", "devtmpfs", "overlay", "aufs", "squashfs", "proc", "sysfs", "cgroup", "cgroup2", "devpts", "securityfs", "pstore", "debugfs", "tracefs", "configfs", "fusectl", "mqueue", "hugetlbfs", "ramfs", "autofs", "binfmt_misc", "nsfs", "rpc_pipefs", "efivarfs":
		return false
	}
	return true
}

type diskTarget struct{ path, device string }

func selectDisks(partitions []disk.PartitionStat, paths string) []diskTarget {
	var candidates []diskTarget
	if strings.TrimSpace(paths) == "" {
		for _, p := range partitions {
			if realFilesystem(p.Fstype) {
				candidates = append(candidates, diskTarget{p.Mountpoint, p.Device})
			}
		}
	} else {
		for _, path := range strings.Split(paths, ",") {
			path = strings.TrimSpace(path)
			if path == "" {
				continue
			}
			path = filepath.Clean(path)
			device, longest := "", -1
			for _, p := range partitions {
				mount := filepath.Clean(p.Mountpoint)
				if (path == mount || strings.HasPrefix(path, strings.TrimSuffix(mount, "/")+"/")) && len(mount) > longest {
					device, longest = p.Device, len(mount)
				}
			}
			candidates = append(candidates, diskTarget{path, device})
		}
	}
	seen := map[string]bool{}
	var result []diskTarget
	for _, candidate := range candidates {
		key := candidate.device
		if key == "" {
			key = candidate.path
		}
		if !seen[key] {
			seen[key] = true
			result = append(result, candidate)
		}
	}
	return result
}

func aggregateDisks(targets []diskTarget, usage func(string) (*disk.UsageStat, error), device func(string) (uint64, error)) capacity {
	var result capacity
	seen := map[uint64]bool{}
	for _, target := range targets {
		id, err := device(target.path)
		if err != nil || seen[id] {
			continue
		}
		u, err := usage(target.path)
		if err != nil || u.Total == 0 {
			continue
		}
		seen[id] = true
		result.Total += u.Total
		result.Used += u.Used
	}
	return result
}

func collectDisks(ctx context.Context, paths string) (capacity, error) {
	partitions, err := disk.PartitionsWithContext(ctx, paths != "")
	if err != nil {
		return capacity{}, err
	}
	return aggregateDisks(selectDisks(partitions, paths), func(path string) (*disk.UsageStat, error) {
		return disk.UsageWithContext(ctx, path)
	}, func(path string) (uint64, error) {
		var stat syscall.Stat_t
		err := syscall.Stat(path, &stat)
		return uint64(stat.Dev), err
	}), nil
}
