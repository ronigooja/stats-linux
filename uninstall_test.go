package main

import (
	"bytes"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestClearRetained(t *testing.T) {
	p := &fakePublisher{open: true, token: fakeToken{complete: true}}
	if err := clearRetained(p, "test-device"); err != nil {
		t.Fatal(err)
	}
	want := []string{"stats/v1/machines/test-device/info", "stats/v1/machines/test-device/metrics", "stats/v1/machines/test-device/status"}
	if !reflect.DeepEqual(p.topics, want) {
		t.Fatalf("topics = %v", p.topics)
	}
	for _, payload := range p.payloads {
		if len(payload) != 0 {
			t.Fatal("cleanup must publish empty bytes, not JSON")
		}
	}
}

func TestClearRetainedFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		open      bool
		token     fakeToken
		publishes int
	}{
		{"offline", false, fakeToken{}, 0},
		{"timeout", true, fakeToken{}, 1},
		{"error", true, fakeToken{complete: true, err: errors.New("failed")}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &fakePublisher{open: tc.open, token: tc.token}
			if clearRetained(p, "device") == nil || len(p.topics) != tc.publishes {
				t.Fatal("cleanup did not stop on failure")
			}
		})
	}
}

func TestUninstallCleanupOrderAndWarnings(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "broker failure"}[fail], func(t *testing.T) {
			s, calls := fakeSystem(t)
			c := config{broker: "ssl://host:8883", deviceID: "installed-device", password: "sample-secret", interval: time.Minute}
			if err := s.install(c, false); err != nil {
				t.Fatal(err)
			}
			*calls = nil
			var warnings bytes.Buffer
			s.warnings = &warnings
			originalRun := s.run
			s.run = func(args ...string) error {
				if args[0] == "disable" {
					// Changing the file proves uninstall read it before stopping.
					if err := os.WriteFile(s.path(configPath), []byte("invalid config"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				return originalRun(args...)
			}
			s.clearRemote = func(got config) error {
				if got.deviceID != c.deviceID || got.password != c.password {
					t.Fatal("did not read installed config before stopping")
				}
				if !reflect.DeepEqual(*calls, []string{"disable --now stats-linux"}) {
					t.Fatal("cleanup before stop")
				}
				for _, path := range []string{configPath, "/etc/systemd/system/stats-linux.service", "/usr/local/bin/stats-linux"} {
					if _, err := os.Stat(s.path(path)); err != nil {
						t.Fatal("deleted before cleanup")
					}
				}
				if fail {
					return errors.New("sample-secret")
				}
				return nil
			}
			if err := s.uninstall(uninstallOptions{purge: true}); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{"/etc/stats-linux", "/etc/systemd/system/stats-linux.service", "/usr/local/bin/stats-linux"} {
				if _, err := os.Stat(s.path(path)); !os.IsNotExist(err) {
					t.Fatal("local file remains")
				}
			}
			if !reflect.DeepEqual(*calls, []string{"disable --now stats-linux", "daemon-reload"}) {
				t.Fatal("incorrect service order")
			}
			if strings.Contains(warnings.String(), "sample-secret") {
				t.Fatal("warning leaked secret")
			}
			if fail && (!strings.Contains(warnings.String(), "Mac app") || !strings.Contains(warnings.String(), "<device-id>/<info|metrics|status> -r -n")) {
				t.Fatal("missing cleanup warning")
			}
			if !fail && warnings.Len() != 0 {
				t.Fatal("unexpected warning")
			}
		})
	}
}

func TestUninstallUnavailableConfig(t *testing.T) {
	for _, kind := range []string{"missing", "invalid", "unreadable"} {
		t.Run(kind, func(t *testing.T) {
			s, _ := fakeSystem(t)
			if err := os.MkdirAll(s.path("/etc/stats-linux"), 0755); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "invalid":
				if err := os.WriteFile(s.path(configPath), []byte("STATS_BROKER=invalid\nSTATS_PASSWORD=sample-secret"), 0600); err != nil {
					t.Fatal(err)
				}
			case "unreadable":
				// A directory is unreadable as an environment file even as root.
				if err := os.Mkdir(s.path(configPath), 0700); err != nil {
					t.Fatal(err)
				}
			}
			var warnings bytes.Buffer
			s.warnings = &warnings
			s.clearRemote = func(config) error { t.Fatal("cleanup with unavailable config"); return nil }
			if err := s.uninstall(uninstallOptions{purge: true}); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(warnings.String(), "Mac app") || strings.Contains(warnings.String(), "sample-secret") {
				t.Fatal("missing or unsafe warning")
			}
		})
	}
}

func TestUninstallConfigPrompt(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		terminal    bool
		options     uninstallOptions
		deleted     bool
	}{
		{"yes", "Y\n", true, uninstallOptions{}, true},
		{"yes word", "yes\n", true, uninstallOptions{}, true},
		{"empty", "\n", true, uninstallOptions{}, true},
		{"no", "n\n", true, uninstallOptions{}, false},
		{"no word", "NO\n", true, uninstallOptions{}, false},
		{"EOF", "", true, uninstallOptions{}, false},
		{"answer at EOF", "y", true, uninstallOptions{}, true},
		{"unknown", "maybe\n", true, uninstallOptions{}, false},
		{"non-TTY", "Y\n", false, uninstallOptions{}, false},
		{"purge", "", false, uninstallOptions{purge: true}, true},
		{"keep", "Y\n", true, uninstallOptions{keepConfig: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := fakeSystem(t)
			var output bytes.Buffer
			s.input, s.output = strings.NewReader(tc.input), &output
			s.terminal = func() bool { return tc.terminal }
			if got := s.deleteConfig(tc.options); got != tc.deleted {
				t.Fatalf("delete = %v", got)
			}
			if !tc.terminal && !tc.options.purge && !strings.Contains(output.String(), "not a terminal") {
				t.Fatal("missing non-TTY notice")
			}
			if tc.terminal && !tc.options.keepConfig && !strings.Contains(output.String(), "Delete configuration /etc/stats-linux? [Y/n]") {
				t.Fatal("missing prompt")
			}
		})
	}
}

func TestUninstallFlagsAndSkipRemote(t *testing.T) {
	if err := command([]string{"uninstall", "--purge", "--keep-config"}); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatal("accepted conflicting flags")
	}
	s, _ := fakeSystem(t)
	s.clearRemote = func(config) error { t.Fatal("cleanup despite --keep-remote"); return nil }
	var warnings bytes.Buffer
	s.warnings = &warnings
	if err := s.uninstall(uninstallOptions{keepConfig: true, keepRemote: true}); err != nil {
		t.Fatal(err)
	}
	if warnings.Len() != 0 {
		t.Fatal("warning despite --keep-remote")
	}
}

func TestUninstallLocalFailure(t *testing.T) {
	for _, failure := range []string{"stop", "remove", "reload"} {
		t.Run(failure, func(t *testing.T) {
			s, _ := fakeSystem(t)
			c := config{broker: "ssl://host:8883", deviceID: "device", interval: time.Minute}
			if err := s.install(c, false); err != nil {
				t.Fatal(err)
			}
			cleared := false
			s.clearRemote = func(config) error { cleared = true; return nil }
			s.run = func(args ...string) error {
				if failure == "stop" && args[0] == "disable" || failure == "reload" && args[0] == "daemon-reload" {
					return errors.New("local failure")
				}
				return nil
			}
			if failure == "remove" {
				path := s.path("/usr/local/bin/stats-linux")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path+"/child", nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.uninstall(uninstallOptions{purge: true}); err == nil {
				t.Fatal("ignored local failure")
			}
			if failure == "stop" && cleared {
				t.Fatal("cleared topics while service could still publish")
			}
		})
	}
}

func TestHelpSafeAndSuccessful(t *testing.T) {
	t.Setenv("STATS_PASSWORD", "sample-secret")
	for _, args := range [][]string{{"-h"}, {"--help"}, {"help"}, {"run", "-h"}, {"install", "-h"}, {"update", "-h"}, {"status", "-h"}, {"restart", "-h"}, {"uninstall", "-h"}, {"version", "-h"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			original := os.Stdout
			os.Stdout = w
			err = command(args)
			os.Stdout = original
			w.Close()
			var output bytes.Buffer
			_, readErr := output.ReadFrom(r)
			r.Close()
			if err != nil || readErr != nil {
				t.Fatal("help failed")
			}
			if strings.Contains(output.String(), "sample-secret") {
				t.Fatal("help leaked secret")
			}
			for _, text := range []string{"run", "install", "update", "status", "restart", "uninstall", "version", "--keep-config", "--keep-remote", "--password"} {
				if !strings.Contains(output.String(), text) {
					t.Fatalf("missing %s", text)
				}
			}
		})
	}
}
