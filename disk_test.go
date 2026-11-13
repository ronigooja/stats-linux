package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/shirou/gopsutil/v4/disk"
)

func TestDiskBreakdown(t *testing.T) {
	parts := []disk.PartitionStat{
		{Device: "z", Mountpoint: "/z", Fstype: "ext4"},
		{Device: "z", Mountpoint: "/alias", Fstype: "ext4"},
		{Device: "b", Mountpoint: "/b", Fstype: "btrfs"},
		{Device: "a", Mountpoint: "/a", Fstype: "ext4"},
		{Device: "tmp", Mountpoint: "/tmp", Fstype: "tmpfs"},
	}
	var many []diskTarget
	var first32 []string
	for i := 34; i >= 0; i-- {
		many = append(many, diskTarget{path: fmt.Sprintf("/disk/%02d", i)})
	}
	for i := 0; i < 32; i++ {
		first32 = append(first32, fmt.Sprintf("/disk/%02d", i))
	}
	for _, tc := range []struct {
		name       string
		targets    []diskTarget
		deviceIDs  map[string]uint64
		wantMounts []string
		counted    int
	}{
		{name: "empty"},
		{name: "automatic selection dedup and sort", targets: selectDisks(parts, ""), wantMounts: []string{"/a", "/b", "/z"}, counted: 3},
		{name: "explicit paths", targets: selectDisks(parts, "/z/folder,/alias,/tmp,/b/folder"), wantMounts: []string{"/b/folder", "/tmp", "/z/folder"}, counted: 3},
		{name: "runtime device dedup keeps first counted path", targets: []diskTarget{{path: "/z"}, {path: "/a"}, {path: "/b"}}, deviceIDs: map[string]uint64{"/z": 1, "/a": 1, "/b": 2}, wantMounts: []string{"/b", "/z"}, counted: 2},
		{name: "exactly 32", targets: many[3:], wantMounts: first32, counted: 32},
		{name: "cap retains aggregate over all filesystems", targets: many, wantMounts: first32, counted: 35},
	} {
		t.Run(tc.name, func(t *testing.T) {
			usageCalls, deviceCalls := map[string]int{}, map[string]int{}
			total, entries := aggregateDisks(tc.targets, func(path string) (*disk.UsageStat, error) {
				usageCalls[path]++
				return &disk.UsageStat{Used: 7, Total: 20}, nil
			}, func(path string) (uint64, error) {
				deviceCalls[path]++
				if tc.deviceIDs != nil {
					return tc.deviceIDs[path], nil
				}
				for i, target := range tc.targets {
					if target.path == path {
						return uint64(i + 1), nil
					}
				}
				panic("unexpected path")
			})
			wantTotal := capacity{Used: uint64(tc.counted) * 7, Total: uint64(tc.counted) * 20}
			if total != wantTotal {
				t.Fatalf("aggregate = %+v, want %+v", total, wantTotal)
			}
			var mounts []string
			var sum capacity
			for _, entry := range entries {
				mounts = append(mounts, entry.Mount)
				if entry.capacity != (capacity{Used: 7, Total: 20}) {
					t.Errorf("unexpected entry: %+v", entry)
				}
				sum.Used += entry.Used
				sum.Total += entry.Total
			}
			if !reflect.DeepEqual(mounts, tc.wantMounts) {
				t.Fatalf("mounts = %v, want %v", mounts, tc.wantMounts)
			}
			if tc.counted <= 32 && sum != total {
				t.Errorf("breakdown sum = %+v, aggregate = %+v", sum, total)
			}
			if tc.counted > 32 && (len(entries) != 32 || sum.Total >= total.Total || sum.Used >= total.Used) {
				t.Errorf("cap lost aggregate or did not truncate: entries=%d, sum=%+v, aggregate=%+v", len(entries), sum, total)
			}
			if len(usageCalls) != tc.counted || len(deviceCalls) != len(tc.targets) {
				t.Errorf("unexpected scan counts: usage=%d, devices=%d", len(usageCalls), len(deviceCalls))
			}
			for path, count := range usageCalls {
				if count != 1 || deviceCalls[path] != 1 {
					t.Errorf("multiple scans for %s: usage=%d, device=%d", path, count, deviceCalls[path])
				}
			}
		})
	}
}

func TestDiskBreakdownJSON(t *testing.T) {
	for _, tc := range []struct {
		name  string
		disks []diskCapacity
		want  string
	}{
		{name: "nil omitted"},
		{name: "empty omitted", disks: []diskCapacity{}},
		{name: "exact entry fields", disks: []diskCapacity{{Mount: "/data", capacity: capacity{Used: 7, Total: 20}}}, want: `[{"mount":"/data","used_bytes":7,"total_bytes":20}]`},
		{name: "zero bytes retained", disks: []diskCapacity{{Mount: "/empty"}}, want: `[{"mount":"/empty","used_bytes":0,"total_bytes":0}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(metrics{Disk: capacity{Used: 7, Total: 20}, Disks: tc.disks})
			if err != nil {
				t.Fatal(err)
			}
			var obj map[string]json.RawMessage
			if err := json.Unmarshal(data, &obj); err != nil {
				t.Fatal(err)
			}
			if got := string(obj["disk"]); got != `{"used_bytes":7,"total_bytes":20}` {
				t.Errorf("aggregate JSON changed: %s", got)
			}
			if got := string(obj["disks"]); got != tc.want {
				t.Errorf("disks = %s, want %s", got, tc.want)
			}
			delete(obj, "disks")
			wantKeys := map[string]bool{"version": true, "timestamp": true, "cpu_percent": true, "memory": true, "disk": true, "network": true, "load_1m": true, "uptime_seconds": true}
			if len(obj) != len(wantKeys) {
				t.Errorf("unexpected metrics field count: %d", len(obj))
			}
			for key := range obj {
				if !wantKeys[key] {
					t.Errorf("unexpected field %s", key)
				}
			}
		})
	}
}
