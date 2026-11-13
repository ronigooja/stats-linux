package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const maxDownload = 100 << 20

type release struct {
	Tag    string `json:"tag_name"`
	Assets []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

type updater struct {
	baseURL       string
	client        *http.Client
	arch, current string
	system        system
	output        io.Writer
}

func newUpdater(s system) updater {
	return updater{baseURL: "https://api.github.com", client: &http.Client{Timeout: 2 * time.Minute}, arch: runtime.GOARCH, current: version, system: s, output: os.Stdout}
}

func assetName(arch string) (string, error) {
	if arch != "amd64" && arch != "arm64" {
		return "", errors.New("only linux amd64 and arm64 release assets are supported")
	}
	return "stats-linux_linux_" + arch, nil
}

func (u updater) fetch(url string, limit int64) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, errors.New("invalid release URL")
	}
	req.Header.Set("User-Agent", "stats-linux")
	resp, err := u.client.Do(req)
	if err != nil {
		return nil, errors.New("release download failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("release request returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, errors.New("read release download failed")
	}
	if int64(len(data)) > limit {
		return nil, errors.New("release download exceeds size limit")
	}
	return data, nil
}

func verifyChecksum(data, sums []byte, name string) error {
	var expected string
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != name {
			continue
		}
		if expected != "" {
			return errors.New("duplicate checksum entry")
		}
		expected = fields[0]
	}
	want, err := hex.DecodeString(expected)
	if err != nil || len(want) != sha256.Size {
		return errors.New("missing or invalid SHA256 checksum")
	}
	actual := sha256.Sum256(data)
	if !strings.EqualFold(hex.EncodeToString(actual[:]), expected) {
		return errors.New("SHA256 checksum mismatch")
	}
	return nil
}

type semanticVersion struct {
	core [3]uint64
	pre  []string
}

func parseVersion(value string) (semanticVersion, error) {
	var v semanticVersion
	value = strings.TrimPrefix(value, "v")
	base, build, hasBuild := strings.Cut(value, "+")
	validIdentifiers := func(text string, numericRule bool) bool {
		for _, part := range strings.Split(text, ".") {
			if part == "" {
				return false
			}
			numeric := true
			for _, c := range part {
				if c < '0' || c > '9' {
					numeric = false
				}
				if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '-') {
					return false
				}
			}
			if numericRule && numeric && len(part) > 1 && part[0] == '0' {
				return false
			}
		}
		return true
	}
	if hasBuild && !validIdentifiers(build, false) {
		return v, errors.New("invalid semantic version")
	}
	core, pre, hasPre := strings.Cut(base, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return v, errors.New("version must be semantic vMAJOR.MINOR.PATCH")
	}
	for i, part := range parts {
		if part == "" || len(part) > 1 && part[0] == '0' {
			return v, errors.New("invalid semantic version")
		}
		for _, c := range part {
			if c < '0' || c > '9' {
				return v, errors.New("invalid semantic version")
			}
		}
		n, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return v, errors.New("invalid semantic version")
		}
		v.core[i] = n
	}
	if hasPre {
		if !validIdentifiers(pre, true) {
			return v, errors.New("invalid prerelease version")
		}
		v.pre = strings.Split(pre, ".")
	}
	return v, nil
}

func compareVersions(a, b string) (int, error) {
	x, err := parseVersion(a)
	if err != nil {
		return 0, err
	}
	y, err := parseVersion(b)
	if err != nil {
		return 0, err
	}
	for i := range x.core {
		if x.core[i] < y.core[i] {
			return -1, nil
		}
		if x.core[i] > y.core[i] {
			return 1, nil
		}
	}
	if len(x.pre) == 0 && len(y.pre) > 0 {
		return 1, nil
	}
	if len(y.pre) == 0 && len(x.pre) > 0 {
		return -1, nil
	}
	for i := 0; i < len(x.pre) && i < len(y.pre); i++ {
		a, b := x.pre[i], y.pre[i]
		if a == b {
			continue
		}
		numeric := func(s string) bool {
			for _, c := range s {
				if c < '0' || c > '9' {
					return false
				}
			}
			return true
		}
		an, bn := numeric(a), numeric(b)
		if an && !bn {
			return -1, nil
		}
		if bn && !an {
			return 1, nil
		}
		if an && bn && len(a) != len(b) {
			if len(a) < len(b) {
				return -1, nil
			}
			return 1, nil
		}
		if a < b {
			return -1, nil
		}
		return 1, nil
	}
	if len(x.pre) < len(y.pre) {
		return -1, nil
	}
	if len(x.pre) > len(y.pre) {
		return 1, nil
	}
	return 0, nil
}

func allowUpdate(current, latest string, force bool) error {
	if _, err := parseVersion(latest); err != nil {
		return errors.New("latest release has an invalid version")
	}
	cmp, err := compareVersions(current, latest)
	if !force && err != nil {
		return errors.New("current version is unknown; use --force to update")
	}
	if !force && cmp > 0 {
		return errors.New("refusing to downgrade; use --force to override")
	}
	return nil
}

func (u updater) update(check, force bool) error {
	name, err := assetName(u.arch)
	if err != nil {
		return err
	}
	body, err := u.fetch(strings.TrimRight(u.baseURL, "/")+"/repos/ronigooja/stats-linux/releases/latest", 2<<20)
	if err != nil {
		return err
	}
	var latest release
	if err := json.Unmarshal(body, &latest); err != nil {
		return errors.New("invalid GitHub release response")
	}
	if _, err := parseVersion(latest.Tag); err != nil {
		return errors.New("latest release has an invalid version")
	}
	fmt.Fprintf(u.output, "current: %s; latest: %s\n", u.current, latest.Tag)
	if check {
		return nil
	}
	if err := allowUpdate(u.current, latest.Tag, force); err != nil {
		return err
	}
	if cmp, err := compareVersions(u.current, latest.Tag); err == nil && cmp == 0 {
		return nil
	}
	urls := map[string]string{}
	for _, asset := range latest.Assets {
		urls[asset.Name] = asset.URL
	}
	if urls[name] == "" || urls["sha256sums.txt"] == "" {
		return errors.New("release is missing binary or sha256sums.txt")
	}
	sums, err := u.fetch(urls["sha256sums.txt"], 1<<20)
	if err != nil {
		return err
	}
	binary, err := u.fetch(urls[name], maxDownload)
	if err != nil {
		return err
	}
	if err := verifyChecksum(binary, sums, name); err != nil {
		return err
	}
	path, err := u.system.executable()
	if err != nil {
		return err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	if err := atomicWrite(path, binary, 0755); err != nil {
		return err
	}
	if u.system.run("is-active", "--quiet", "stats-linux") == nil {
		return u.system.run("restart", "stats-linux")
	}
	return nil
}
