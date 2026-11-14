package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/disk"
)

func TestDiskTargetCache(t *testing.T) {
	type step struct {
		advance     time.Duration
		paths       string
		scanError   bool
		usageError  bool
		deviceError bool
		scans       int
		mount       string
	}
	for _, tc := range []struct {
		name  string
		empty bool
		steps []step
	}{
		{name: "first scan, reuse, and exact expiry", steps: []step{
			{scans: 1, mount: "/disk1"},
			{advance: time.Minute, scans: 1, mount: "/disk1"},
			{advance: 4*time.Minute - time.Nanosecond, scans: 1, mount: "/disk1"},
			{advance: time.Nanosecond, scans: 2, mount: "/disk2"},
		}},
		{name: "usage failure invalidates", steps: []step{
			{scans: 1, mount: "/disk1"},
			{usageError: true, scans: 1},
			{scans: 2, mount: "/disk2"},
		}},
		{name: "device failure invalidates", steps: []step{
			{deviceError: true, scans: 1},
			{scans: 2, mount: "/disk2"},
		}},
		{name: "scan error is not cached", steps: []step{
			{scanError: true, scans: 1},
			{scans: 2, mount: "/disk2"},
			{advance: 5 * time.Minute, scanError: true, scans: 3},
			{scans: 4, mount: "/disk4"},
		}},
		{name: "explicit paths never scan and deduplicate devices", steps: []step{
			{paths: "/explicit,/alias,/explicit", mount: "/explicit"},
			{advance: 10 * time.Minute, paths: "/explicit,/alias", mount: "/explicit"},
		}},
		{name: "explicit paths invalidate automatic cache", steps: []step{
			{scans: 1, mount: "/disk1"},
			{paths: "/explicit", scans: 1, mount: "/explicit"},
			{scans: 2, mount: "/disk2"},
		}},
		{name: "empty successful scan is cached", empty: true, steps: []step{
			{scans: 1},
			{advance: time.Minute, scans: 1},
			{advance: 4 * time.Minute, scans: 2},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Time{}
			scans := 0
			var scanError bool
			failure := errors.New("filesystem unavailable")
			cache := diskTargetCache{
				now: func() time.Time { return now },
				partitions: func(_ context.Context, all bool) ([]disk.PartitionStat, error) {
					scans++
					if all {
						t.Error("automatic scan requested all filesystems")
					}
					if scanError {
						return nil, failure
					}
					if tc.empty {
						return nil, nil
					}
					mount := fmt.Sprintf("/disk%d", scans)
					return []disk.PartitionStat{
						{Device: "data", Mountpoint: mount, Fstype: "ext4"},
						{Device: "data", Mountpoint: "/duplicate", Fstype: "ext4"},
						{Device: "tmp", Mountpoint: "/tmp", Fstype: "tmpfs"},
					}, nil
				},
			}
			for i, s := range tc.steps {
				now = now.Add(s.advance)
				scanError = s.scanError
				var usagePaths []string
				used := uint64(i + 1)
				total, entries, err := cache.collect(context.Background(), s.paths, func(path string) (*disk.UsageStat, error) {
					usagePaths = append(usagePaths, path)
					if s.usageError {
						return nil, failure
					}
					return &disk.UsageStat{Used: used, Total: 100}, nil
				}, func(string) (uint64, error) {
					if s.deviceError {
						return 0, failure
					}
					return 1, nil
				})
				if (err != nil) != s.scanError || (err != nil && !errors.Is(err, failure)) {
					t.Fatalf("step %d: unexpected error %v", i, err)
				}
				if scans != s.scans {
					t.Fatalf("step %d: scans = %d, want %d", i, scans, s.scans)
				}
				if s.mount != "" {
					want := capacity{Used: used, Total: 100}
					if total != want || !reflect.DeepEqual(entries, []diskCapacity{{Mount: s.mount, capacity: want}}) || !reflect.DeepEqual(usagePaths, []string{s.mount}) {
						t.Fatalf("step %d: stale or incorrect metrics: total=%+v entries=%+v usage=%v", i, total, entries, usagePaths)
					}
				} else if total != (capacity{}) || len(entries) != 0 {
					t.Fatalf("step %d: unexpected metrics: %+v %+v", i, total, entries)
				}
			}
		})
	}
}
