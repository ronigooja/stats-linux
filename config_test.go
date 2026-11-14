package main

import (
	"errors"
	"flag"
	"strings"
	"testing"
)

func TestSanitizeHostname(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"allowed", "Host-1_2.example", "Host-1_2.example"},
		{"spaces", " a b ", "a-b"},
		{"slash", "a/b", "a-b"},
		{"plus", "a+b", "a-b"},
		{"hash", "a#b", "a-b"},
		{"unicode", "a机器b", "a--b"},
		{"nul", "a\x00b", "a-b"},
		{"long", strings.Repeat("a", 80), strings.Repeat("a", 64)},
		{"truncated dash", strings.Repeat("a", 63) + "-b", strings.Repeat("a", 63)},
		{"empty", "", ""},
		{"only invalid", " /+#机器\x00", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeHostname(tc.input); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHostnameDefault(t *testing.T) {
	for _, tc := range []struct {
		name, id, host, broker, want string
		hostErr                      error
		valid                        bool
	}{
		{name: "hostname", host: "my host/机器", broker: "ssl://host:8883", want: "my-host", valid: true},
		{name: "explicit", id: "explicit", broker: "ssl://host:8883", want: "explicit", hostErr: errors.New("unused"), valid: true},
		{name: "empty hostname", broker: "ssl://host:8883"},
		{name: "invalid hostname", host: "/+#", broker: "ssl://host:8883"},
		{name: "hostname error", broker: "ssl://host:8883", hostErr: errors.New("unavailable")},
		{name: "broker required", host: "valid"},
		{name: "explicit slash", id: "a/b", broker: "ssl://host:8883"},
		{name: "explicit plus", id: "+", broker: "ssl://host:8883"},
		{name: "explicit hash", id: "#", broker: "ssl://host:8883"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			c := configFlags(fs, map[string]string{"STATS_BROKER": tc.broker, "STATS_DEVICE_ID": tc.id}, func(string) (string, bool) { return "", false })
			if err := fs.Parse(nil); err != nil {
				t.Fatal(err)
			}
			err := finishConfigWithHostname(fs, c, func() (string, error) { return tc.host, tc.hostErr })
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
			if tc.valid && c.deviceID != tc.want {
				t.Fatalf("got ID %q, want %q", c.deviceID, tc.want)
			}
			if tc.valid {
				s, _ := fakeSystem(t)
				if err := s.install(*c, false); err != nil {
					t.Fatal(err)
				}
				file, err := loadEnvironment(s.path(configPath))
				if err != nil || file["STATS_DEVICE_ID"] != tc.want {
					t.Fatal("resolved device ID was not persisted")
				}
			}
		})
	}
}
