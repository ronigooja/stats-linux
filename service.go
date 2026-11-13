package main

import (
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

//go:embed stats-linux.service
var serviceUnit []byte

var version = "dev"

type system struct {
	root       string
	uid        func() int
	executable func() (string, error)
	run        func(...string) error
}

func localSystem() system {
	return system{uid: os.Geteuid, executable: os.Executable, run: func(args ...string) error {
		if err := exec.Command("systemctl", args...).Run(); err != nil {
			return fmt.Errorf("systemctl %s failed", strings.Join(args, " "))
		}
		return nil
	}}
}

func (s system) path(path string) string { return filepath.Join(s.root, path) }
func (s system) requireRoot() error {
	if s.uid() != 0 {
		return errors.New("this command must run as root")
	}
	return nil
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".stats-linux-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Chmod(mode)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}

func (s system) install(c config, force bool) error {
	if err := s.requireRoot(); err != nil {
		return err
	}
	path := s.path(configPath)
	_, err := os.Stat(path)
	writeConfig := force || os.IsNotExist(err)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var data []byte
	if writeConfig {
		if err := validateConfig(c); err != nil {
			return err
		}
		data, err = encodeEnvironment(c)
		if err != nil {
			return err
		}
	}
	for _, dir := range []string{"/usr/local/bin", "/etc/systemd/system", "/etc/stats-linux"} {
		if err := os.MkdirAll(s.path(dir), 0755); err != nil {
			return err
		}
	}
	if writeConfig {
		if force {
			err = atomicWrite(path, data, 0600)
		} else {
			var f *os.File
			f, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if err == nil {
				_, err = f.Write(data)
				closeErr := f.Close()
				if err == nil {
					err = closeErr
				}
			}
		}
		if err != nil {
			return err
		}
	}
	source, err := s.executable()
	if err != nil {
		return err
	}
	binary, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	if err := atomicWrite(s.path("/usr/local/bin/stats-linux"), binary, 0755); err != nil {
		return err
	}
	if err := atomicWrite(s.path("/etc/systemd/system/stats-linux.service"), serviceUnit, 0644); err != nil {
		return err
	}
	if err := s.run("daemon-reload"); err != nil {
		return err
	}
	return s.run("enable", "--now", "stats-linux")
}

func removeIfPresent(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s system) uninstall(purge bool) error {
	if err := s.requireRoot(); err != nil {
		return err
	}
	unit := s.path("/etc/systemd/system/stats-linux.service")
	// A missing unit is normal when uninstalling again. Still stop any loaded unit.
	stopErr := s.run("disable", "--now", "stats-linux")
	if _, err := os.Stat(unit); err == nil && stopErr != nil {
		return stopErr
	}
	if err := removeIfPresent(unit); err != nil {
		return err
	}
	if err := removeIfPresent(s.path("/usr/local/bin/stats-linux")); err != nil {
		return err
	}
	if purge {
		if err := os.RemoveAll(s.path("/etc/stats-linux")); err != nil {
			return err
		}
	}
	return s.run("daemon-reload")
}

func command(args []string) error {
	name := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	if name == "version" {
		fmt.Println(version)
		return nil
	}
	s := localSystem()
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	force, check, purge := false, false, false
	switch name {
	case "run", "install":
		if name == "install" {
			if err := s.requireRoot(); err != nil {
				return err
			}
			fs.BoolVar(&force, "force", false, "overwrite existing config")
		}
		file, err := loadEnvironment(configPath)
		if err != nil {
			return fmt.Errorf("read config: %w", err)
		}
		c := configFlags(fs, file, os.LookupEnv)
		if err := fs.Parse(args); err != nil {
			return errors.New("invalid flags (credentials are omitted from diagnostics)")
		}
		if err := finishConfig(fs, c); err != nil {
			return err
		}
		if name == "install" {
			return s.install(*c, force)
		}
		return run(*c)
	case "update":
		fs.BoolVar(&force, "force", false, "allow downgrade or unknown version")
		fs.BoolVar(&check, "check", false, "only report versions")
		if err := fs.Parse(args); err != nil {
			return errors.New("invalid update flags")
		}
		if fs.NArg() != 0 {
			return errors.New("unexpected positional arguments")
		}
		return newUpdater(s).update(check, force)
	case "uninstall":
		fs.BoolVar(&purge, "purge", false, "also remove config directory")
		if err := fs.Parse(args); err != nil {
			return errors.New("invalid uninstall flags")
		}
		if fs.NArg() != 0 {
			return errors.New("unexpected positional arguments")
		}
		return s.uninstall(purge)
	default:
		return errors.New("expected run, install, update, uninstall, or version")
	}
}
