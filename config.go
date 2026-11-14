package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const configPath = "/etc/stats-linux/config"

func parseEnvironment(r io.Reader) (map[string]string, error) {
	values := map[string]string{}
	s := bufio.NewScanner(r)
	for line := 1; s.Scan(); line++ {
		text := strings.TrimSpace(s.Text())
		if text == "" || strings.HasPrefix(text, "#") || strings.HasPrefix(text, ";") {
			continue
		}
		key, value, ok := strings.Cut(text, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !ok || !validKey(key) {
			return nil, fmt.Errorf("config line %d: expected KEY=VALUE", line)
		}
		if strings.HasPrefix(value, "'") {
			if len(value) < 2 || !strings.HasSuffix(value, "'") || strings.Contains(value[1:len(value)-1], "'") {
				return nil, fmt.Errorf("config line %d: invalid single-quoted value", line)
			}
			value = value[1 : len(value)-1]
		} else if strings.HasPrefix(value, "\"") {
			var err error
			value, err = unquoteEnvironment(value)
			if err != nil {
				return nil, fmt.Errorf("config line %d: invalid double-quoted value", line)
			}
		} else if strings.ContainsAny(value, "'\"\\") {
			return nil, fmt.Errorf("config line %d: quote values containing quotes or backslashes", line)
		}
		if strings.ContainsAny(value, "\x00\r\n") {
			return nil, fmt.Errorf("config line %d: control characters are not supported", line)
		}
		values[key] = value
	}
	return values, s.Err()
}

func validKey(key string) bool {
	if key == "" {
		return false
	}
	for i, c := range key {
		if c != '_' && (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') && (i == 0 || c < '0' || c > '9') {
			return false
		}
	}
	return true
}

func unquoteEnvironment(value string) (string, error) {
	if len(value) < 2 || value[len(value)-1] != '"' {
		return "", errors.New("missing quote")
	}
	var out strings.Builder
	for i := 1; i < len(value)-1; i++ {
		c := value[i]
		if c == '"' {
			return "", errors.New("unexpected quote")
		}
		if c == '\\' {
			i++
			if i >= len(value)-1 {
				return "", errors.New("trailing escape")
			}
			c = value[i]
			if !strings.ContainsRune("\"\\$`", rune(c)) {
				out.WriteByte('\\')
			}
		}
		out.WriteByte(c)
	}
	return out.String(), nil
}

func loadEnvironment(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return map[string]string{}, nil
	}
	// systemd reads the root-only EnvironmentFile before dropping privileges.
	if os.IsPermission(err) && os.Getenv("STATS_BROKER") != "" && os.Getenv("STATS_DEVICE_ID") != "" {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseEnvironment(f)
}

func configFlags(fs *flag.FlagSet, file map[string]string, lookup func(string) (string, bool)) *config {
	c := &config{}
	get := func(key, fallback string) string {
		if value, ok := lookup(key); ok {
			return value
		}
		if value, ok := file[key]; ok {
			return value
		}
		return fallback
	}
	fs.StringVar(&c.broker, "broker", get("STATS_BROKER", ""), "MQTT TLS broker URL")
	fs.StringVar(&c.deviceID, "device-id", get("STATS_DEVICE_ID", ""), "unique MQTT topic segment")
	fs.StringVar(&c.username, "username", get("STATS_USERNAME", ""), "MQTT username")
	fs.StringVar(&c.password, "password", get("STATS_PASSWORD", ""), "MQTT password (prefer environment)")
	fs.StringVar(&c.caFile, "ca-file", get("STATS_CA_FILE", ""), "additional PEM CA certificates")
	fs.StringVar(&c.diskPath, "disk-path", get("STATS_DISK_PATH", ""), "comma-separated filesystem paths (default: auto-detect)")
	// Parse durations after flags so an explicit flag can override an invalid environment value.
	interval := get("STATS_INTERVAL", "1m")
	fs.Var(&durationValue{text: &interval}, "interval", "metrics interval (default 1m, minimum 5s)")
	fs.SetOutput(io.Discard) // Flag defaults can contain credentials; never print them.
	return c
}

type durationValue struct {
	text *string
}

func (v *durationValue) String() string {
	if v.text == nil {
		return ""
	}
	return *v.text
}
func (v *durationValue) Set(s string) error { *v.text = s; return nil }

func finishConfig(fs *flag.FlagSet, c *config) error {
	return finishConfigWithHostname(fs, c, os.Hostname)
}

// sanitizeHostname applies only to an implicit device ID, not explicit IDs.
func sanitizeHostname(hostname string) string {
	var out strings.Builder
	for _, c := range hostname {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' {
			out.WriteRune(c)
		} else {
			out.WriteByte('-')
		}
	}
	id := strings.Trim(out.String(), "-")
	if len(id) > 64 {
		id = strings.TrimRight(id[:64], "-")
	}
	return id
}

func finishConfigWithHostname(fs *flag.FlagSet, c *config, hostname func() (string, error)) error {
	if fs.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	v := fs.Lookup("interval").Value.(*durationValue)
	d, err := time.ParseDuration(*v.text)
	if err != nil {
		return errors.New("interval must be a Go duration such as 30s or 1m")
	}
	c.interval = d
	if c.deviceID == "" {
		host, err := hostname()
		if err != nil {
			return errors.New("cannot determine hostname; provide --device-id")
		}
		c.deviceID = sanitizeHostname(host)
		if c.deviceID == "" {
			return errors.New("hostname has no usable MQTT topic characters; provide --device-id")
		}
	}
	return validateConfig(*c)
}

func validateConfig(c config) error {
	u, err := url.Parse(c.broker)
	if err != nil || u.Scheme != "ssl" || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("broker must be an ssl://host:port URL without credentials or path")
	}
	if u.Port() == "" {
		return errors.New("broker URL must include a port")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return errors.New("broker port must be between 1 and 65535")
	}
	if c.deviceID == "" || strings.ContainsAny(c.deviceID, "/+#\x00") {
		return errors.New("device-id must be a nonempty MQTT topic segment without /, +, or #")
	}
	if c.interval < 5*time.Second {
		return errors.New("interval must be at least 5s")
	}
	return nil
}

func encodeEnvironment(c config) ([]byte, error) {
	var out strings.Builder
	for _, entry := range [][2]string{
		{"STATS_BROKER", c.broker}, {"STATS_DEVICE_ID", c.deviceID}, {"STATS_USERNAME", c.username}, {"STATS_PASSWORD", c.password},
		{"STATS_CA_FILE", c.caFile}, {"STATS_DISK_PATH", c.diskPath}, {"STATS_INTERVAL", c.interval.String()},
	} {
		if strings.ContainsAny(entry[1], "\x00\r\n") {
			return nil, errors.New("config values cannot contain NUL or newlines")
		}
		value := strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "$", "\\$", "`", "\\`").Replace(entry[1])
		fmt.Fprintf(&out, "%s=\"%s\"\n", entry[0], value)
	}
	return []byte(out.String()), nil
}
