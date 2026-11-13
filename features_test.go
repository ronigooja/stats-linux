package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/disk"
)

func TestSelectDisks(t *testing.T) {
	parts := []disk.PartitionStat{
		{Device: "/dev/a", Mountpoint: "/", Fstype: "ext4"},
		{Device: "/dev/b", Mountpoint: "/data", Fstype: "btrfs"},
		{Device: "/dev/b", Mountpoint: "/subvol", Fstype: "btrfs"},
		{Device: "/dev/a", Mountpoint: "/bind", Fstype: "ext4"},
		{Device: "tmpfs", Mountpoint: "/tmp", Fstype: "tmpfs"},
		{Device: "overlay", Mountpoint: "/container", Fstype: "overlay"},
	}
	for _, tc := range []struct {
		name, paths string
		want        []diskTarget
	}{
		{"auto", "", []diskTarget{{"/", "/dev/a"}, {"/data", "/dev/b"}}},
		{"explicit", "/data,/subvol,/data/folder, /tmp", []diskTarget{{"/data", "/dev/b"}, {"/tmp", "tmpfs"}}},
		{"root descendants", "/var,/bind", []diskTarget{{"/var", "/dev/a"}}},
		{"prefix boundary", "/database", []diskTarget{{"/database", "/dev/a"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := selectDisks(parts, tc.paths); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
	for _, fs := range []string{"tmpfs", "devtmpfs", "overlay", "squashfs", "proc", "sysfs", "cgroup", "cgroup2", "nsfs"} {
		if realFilesystem(fs) {
			t.Errorf("accepted pseudo filesystem %s", fs)
		}
	}
}

func TestAggregateDisks(t *testing.T) {
	targets := []diskTarget{{path: "bad"}, {path: "zero"}, {path: "one"}, {path: "alias"}, {path: "two"}, {path: "missing"}}
	got := aggregateDisks(targets, func(path string) (*disk.UsageStat, error) {
		if path == "bad" {
			return nil, errors.New("unmounted")
		}
		if path == "zero" {
			return &disk.UsageStat{}, nil
		}
		return &disk.UsageStat{Total: 100, Used: 40}, nil
	}, func(path string) (uint64, error) {
		if path == "missing" {
			return 0, errors.New("missing")
		}
		if path == "two" {
			return 2, nil
		}
		return 1, nil
	})
	if got != (capacity{Total: 200, Used: 80}) {
		t.Fatalf("got %+v", got)
	}
}

func TestEnvironment(t *testing.T) {
	c := config{broker: "ssl://example:8883", deviceID: "host", username: "a b", password: "p'\"\\$`#", interval: time.Minute}
	encoded, err := encodeEnvironment(c)
	if err != nil {
		t.Fatal(err)
	}
	values, err := parseEnvironment(strings.NewReader(string(encoded)))
	if err != nil || values["STATS_PASSWORD"] != c.password {
		t.Fatal("config round trip failed")
	}
	for _, input := range []string{"bad line", "1KEY=x", "KEY='unterminated", "KEY=\"unterminated", "KEY=\"a\"b\"", "KEY=x\\y", "KEY=a\x00"} {
		if _, err := parseEnvironment(strings.NewReader(input)); err == nil {
			t.Errorf("accepted malformed config")
		}
	}
	values, err = parseEnvironment(strings.NewReader("# comment\n; comment\n A = 'space value'\nA=last\nEMPTY=\n"))
	if err != nil || values["A"] != "last" || values["EMPTY"] != "" {
		t.Fatalf("unexpected values %v, %v", values, err)
	}
	c.password = "new\nline"
	if _, err := encodeEnvironment(c); err == nil {
		t.Fatal("accepted newline")
	}
}

func TestConfigPrecedenceAndValidation(t *testing.T) {
	for _, tc := range []struct {
		name, interval, broker, id string
		valid                      bool
	}{
		{"valid", "5s", "ssl://host:8883", "host", true},
		{"duration", "1m", "ssl://host:8883", "host", true},
		{"too short", "4s", "ssl://host:8883", "host", false},
		{"invalid duration", "30", "ssl://host:8883", "host", false},
		{"plaintext", "30s", "tcp://host:1883", "host", false},
		{"credentials", "30s", "ssl://user:secret@host:8883", "host", false},
		{"no port", "30s", "ssl://host", "host", false},
		{"topic", "30s", "ssl://host:8883", "host/+", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			c := configFlags(fs, map[string]string{"STATS_BROKER": tc.broker, "STATS_DEVICE_ID": tc.id, "STATS_INTERVAL": tc.interval}, func(string) (string, bool) { return "", false })
			if err := fs.Parse(nil); err != nil {
				t.Fatal(err)
			}
			if err := finishConfig(fs, c); (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	c := configFlags(fs, map[string]string{"STATS_BROKER": "ssl://file:8883", "STATS_DEVICE_ID": "file", "STATS_INTERVAL": "bad"}, func(key string) (string, bool) {
		if key == "STATS_DEVICE_ID" {
			return "env", true
		}
		return "", false
	})
	if err := fs.Parse([]string{"--broker=ssl://flag:8883", "--interval=1m"}); err != nil {
		t.Fatal(err)
	}
	if err := finishConfig(fs, c); err != nil || c.deviceID != "env" || c.broker != "ssl://flag:8883" || c.interval != time.Minute {
		t.Fatal("precedence failed")
	}
}

func TestChecksum(t *testing.T) {
	data := []byte("binary")
	sum := fmt.Sprintf("%x  stats-linux_linux_amd64\n", sha256.Sum256(data))
	for _, tc := range []struct {
		name  string
		data  []byte
		sums  string
		valid bool
	}{
		{"good", data, sum, true}, {"bad", []byte("corrupt"), sum, false}, {"missing", data, "", false}, {"duplicate", data, sum + sum, false}, {"malformed", data, "bad  stats-linux_linux_amd64", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := verifyChecksum(tc.data, []byte(tc.sums), "stats-linux_linux_amd64"); (err == nil) != tc.valid {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestAssetName(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		if got, err := assetName(arch); err != nil || got != "stats-linux_linux_"+arch {
			t.Fatal(got, err)
		}
	}
	if _, err := assetName("386"); err == nil {
		t.Fatal("accepted unsupported arch")
	}
}

func TestVersions(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"v1.2.3", "1.2.3", 0}, {"v1.10.0", "v1.9.0", 1}, {"v1.0.0-rc.1", "v1.0.0", -1},
		{"v1.0.0-alpha.9", "v1.0.0-alpha.10", -1}, {"v1.0.0+build", "v1.0.0", 0}, {"v2.0.0", "v10.0.0", -1},
	} {
		if got, err := compareVersions(tc.a, tc.b); err != nil || got != tc.want {
			t.Errorf("%s vs %s: %d %v", tc.a, tc.b, got, err)
		}
	}
	for _, bad := range []string{"dev", "v1", "v01.0.0", "v1.0.0-01", "v1.0.0+", "v1.0.0-"} {
		if _, err := parseVersion(bad); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if err := allowUpdate("v2.0.0", "v1.0.0", false); err == nil {
		t.Fatal("downgrade allowed")
	}
	if err := allowUpdate("v2.0.0", "v1.0.0", true); err != nil {
		t.Fatal(err)
	}
	if err := allowUpdate("dev", "v1.0.0", false); err == nil {
		t.Fatal("unknown version allowed")
	}
}

func fakeSystem(t *testing.T) (system, *[]string) {
	t.Helper()
	root := t.TempDir()
	binary := filepath.Join(root, "running")
	if err := os.WriteFile(binary, []byte("old binary"), 0755); err != nil {
		t.Fatal(err)
	}
	var calls []string
	return system{root: root, uid: func() int { return 0 }, executable: func() (string, error) { return binary, nil }, run: func(args ...string) error { calls = append(calls, strings.Join(args, " ")); return nil }}, &calls
}

func TestInstallUninstall(t *testing.T) {
	s, calls := fakeSystem(t)
	c := config{broker: "ssl://host:8883", deviceID: "host", password: "secret", interval: 30 * time.Second}
	if err := s.install(c, false); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(s.path(configPath))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("config mode")
	}
	original, err := os.ReadFile(s.path(configPath))
	if err != nil {
		t.Fatal(err)
	}
	c.password = "replacement"
	if err := s.install(c, false); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(s.path(configPath))
	if string(data) != string(original) {
		t.Fatal("overwrote existing config")
	}
	if err := s.install(c, true); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(s.path(configPath))
	if string(data) == string(original) {
		t.Fatal("force did not replace config")
	}
	for _, call := range *calls {
		if strings.Contains(call, "secret") || strings.Contains(call, "replacement") {
			t.Fatal("credential in command")
		}
	}
	if err := s.uninstall(false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.path(configPath)); err != nil {
		t.Fatal("removed config")
	}
	if err := s.uninstall(true); err != nil {
		t.Fatal(err)
	}
	if err := s.uninstall(true); err != nil {
		t.Fatal(err)
	}
	s.uid = func() int { return 1000 }
	if err := s.install(c, false); err == nil {
		t.Fatal("non-root install allowed")
	}
	if err := s.uninstall(false); err == nil {
		t.Fatal("non-root uninstall allowed")
	}
}

func TestUpdateHTTP(t *testing.T) {
	for _, tc := range []struct {
		name, current                string
		check, force, corrupt, valid bool
	}{
		{"upgrade", "v1.0.0", false, false, false, true}, {"check", "v1.0.0", true, false, false, true},
		{"downgrade", "v3.0.0", false, false, false, false}, {"forced downgrade", "v3.0.0", false, true, false, true},
		{"corrupt", "v1.0.0", false, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, calls := fakeSystem(t)
			data := []byte("new binary")
			var base string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/repos/ronigooja/stats-linux/releases/latest":
					json.NewEncoder(w).Encode(map[string]any{"tag_name": "v2.0.0", "assets": []map[string]string{{"name": "stats-linux_linux_amd64", "browser_download_url": base + "/binary"}, {"name": "sha256sums.txt", "browser_download_url": base + "/sums"}}})
				case "/binary":
					if tc.corrupt {
						w.Write([]byte("corrupt"))
					} else {
						w.Write(data)
					}
				case "/sums":
					fmt.Fprintf(w, "%x  stats-linux_linux_amd64\n", sha256.Sum256(data))
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			base = server.URL
			u := updater{baseURL: base, client: server.Client(), arch: "amd64", current: tc.current, system: s, output: io.Discard}
			if err := u.update(tc.check, tc.force); (err == nil) != tc.valid {
				t.Fatalf("err=%v", err)
			}
			path, _ := s.executable()
			got, _ := os.ReadFile(path)
			if tc.valid && !tc.check {
				if string(got) != string(data) || len(*calls) != 2 {
					t.Fatal("update not applied")
				}
			} else if string(got) != "old binary" || len(*calls) != 0 {
				t.Fatal("failed/check update changed system")
			}
		})
	}
}

func TestDownloadLimitAndStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, "oversized")
	}))
	defer server.Close()
	u := updater{client: server.Client()}
	if _, err := u.fetch(server.URL, 3); err == nil {
		t.Fatal("size limit ignored")
	}
	if _, err := u.fetch(server.URL+"/missing", 100); err == nil {
		t.Fatal("status ignored")
	}
}
