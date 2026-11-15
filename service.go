package main

import (
	"bufio"
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
	root        string
	uid         func() int
	executable  func() (string, error)
	run         func(...string) error
	runStatus   func(...string) error
	clearRemote func(config) error
	input       io.Reader
	output      io.Writer
	warnings    io.Writer
	terminal    func() bool
}

func localSystem() system {
	return system{uid: os.Geteuid, executable: os.Executable, clearRemote: clearRemote,
		input: os.Stdin, output: os.Stdout, warnings: os.Stderr, terminal: func() bool {
			info, err := os.Stdin.Stat()
			return err == nil && info.Mode()&os.ModeCharDevice != 0
		}, run: func(args ...string) error {
			if err := exec.Command("systemctl", args...).Run(); err != nil {
				return fmt.Errorf("systemctl %s failed", strings.Join(args, " "))
			}
			return nil
		}, runStatus: func(args ...string) error {
			cmd := exec.Command("systemctl", args...)
			cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("systemctl %s: %w", strings.Join(args, " "), err)
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

type uninstallOptions struct {
	purge, keepConfig, keepRemote bool
}

func (s system) deleteConfig(options uninstallOptions) bool {
	if options.purge {
		return true
	}
	if options.keepConfig {
		return false
	}
	if s.terminal == nil || !s.terminal() {
		fmt.Fprintln(s.output, "Keeping configuration /etc/stats-linux (stdin is not a terminal; use --purge to delete).")
		return false
	}
	fmt.Fprint(s.output, "Delete configuration /etc/stats-linux? [Y/n] ")
	line, err := bufio.NewReader(s.input).ReadString('\n')
	if err != nil && (err != io.EOF || strings.TrimSpace(line) == "") {
		fmt.Fprintln(s.output, "Keeping configuration (no answer received).")
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "" || answer == "y" || answer == "yes"
}

func (s system) uninstall(options uninstallOptions) error {
	if options.purge && options.keepConfig {
		return errors.New("--purge and --keep-config are mutually exclusive")
	}
	if err := s.requireRoot(); err != nil {
		return err
	}
	var c *config
	var remoteErr error
	if !options.keepRemote {
		// Check existence explicitly: run permits a missing file, uninstall cannot
		// safely infer which installed machine to remove from the broker.
		_, remoteErr = os.Stat(s.path(configPath))
		if remoteErr == nil {
			var file map[string]string
			file, remoteErr = loadEnvironment(s.path(configPath))
			if remoteErr == nil {
				fs := flag.NewFlagSet("cleanup", flag.ContinueOnError)
				c = configFlags(fs, file, func(string) (string, bool) { return "", false })
				remoteErr = finishConfig(fs, c)
			}
		}
		// Never include errors or config values: broker errors may contain secrets.
		defer func() {
			if remoteErr != nil {
				fmt.Fprintln(s.warnings, "Warning: MQTT cleanup failed or configuration was unavailable/invalid; the machine may still appear in the Mac app. Clear each retained topic manually:")
				fmt.Fprintln(s.warnings, "mosquitto_pub -h <host> -p <port> -u <user> -P <password> --capath /etc/ssl/certs -t stats/v1/machines/<device-id>/<info|metrics|status> -r -n")
			}
		}()
	}
	unit := s.path("/etc/systemd/system/stats-linux.service")
	// A missing unit is normal when uninstalling again. Still stop any loaded unit.
	stopErr := s.run("disable", "--now", "stats-linux")
	if _, err := os.Stat(unit); err == nil && stopErr != nil {
		return stopErr
	}
	if !options.keepRemote && remoteErr == nil {
		if stopErr != nil {
			remoteErr = stopErr // Do not clear while a loaded service might publish.
		} else {
			remoteErr = s.clearRemote(*c)
		}
	}
	if err := removeIfPresent(unit); err != nil {
		return err
	}
	if err := removeIfPresent(s.path("/usr/local/bin/stats-linux")); err != nil {
		return err
	}
	if s.deleteConfig(options) {
		if err := os.RemoveAll(s.path("/etc/stats-linux")); err != nil {
			return err
		}
	}
	return s.run("daemon-reload")
}

func (s system) restart() error {
	if err := s.requireRoot(); err != nil {
		return err
	}
	return s.run("restart", "stats-linux")
}

func (s system) status() error {
	return s.runStatus("status", "--no-pager", "--full", "stats-linux")
}

func command(args []string) error {
	name := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	if (name == "run" && len(args) == 1 && (args[0] == "-h" || args[0] == "--help")) || name == "help" ||
		(knownCommand(name) && len(args) == 1 && (args[0] == "-h" || args[0] == "--help")) {
		printUsage(os.Stdout)
		return nil
	}
	if name == "version" {
		fmt.Println(version)
		return nil
	}
	s := localSystem()
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	force, check := false, false
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
			if err := s.install(*c, force); err != nil {
				return err
			}
			// Report the persisted ID when an existing configuration was kept.
			if !force && file["STATS_DEVICE_ID"] != "" {
				c.deviceID = file["STATS_DEVICE_ID"]
			}
			fmt.Printf("Installed stats-linux (device ID: %s)\nCheck status: stats-linux status\n", c.deviceID)
			return nil
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
	case "status", "restart":
		if err := fs.Parse(args); err != nil {
			return fmt.Errorf("invalid %s flags", name)
		}
		if fs.NArg() != 0 {
			return errors.New("unexpected positional arguments")
		}
		if name == "status" {
			return s.status()
		}
		if err := s.restart(); err != nil {
			return err
		}
		fmt.Println("restarted")
		return nil
	case "uninstall":
		options := uninstallOptions{}
		fs.BoolVar(&options.purge, "purge", false, "remove config without asking")
		fs.BoolVar(&options.keepConfig, "keep-config", false, "keep config without asking")
		fs.BoolVar(&options.keepRemote, "keep-remote", false, "skip MQTT cleanup")
		if err := fs.Parse(args); err != nil {
			return errors.New("invalid uninstall flags")
		}
		if fs.NArg() != 0 {
			return errors.New("unexpected positional arguments")
		}
		return s.uninstall(options)
	default:
		return errors.New("expected run, install, update, status, restart, uninstall, or version")
	}
}

func knownCommand(name string) bool {
	switch name {
	case "run", "install", "update", "status", "restart", "uninstall", "version":
		return true
	}
	return false
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, `Usage: stats-linux [subcommand] [flags]
Subcommands:
  run        Publish metrics (also used when no subcommand is given)
  install    Install and start the service; --force overwrites configuration
  update     Update the binary; --check reports versions, --force permits downgrade
  status     Show systemd service status
  restart    Restart the service
  uninstall  Stop and remove the service and clear retained MQTT topics
             --purge deletes configuration without asking
             --keep-config keeps configuration without asking
             --keep-remote skips MQTT cleanup
  version    Print the version
Run and install flags:
  --broker     MQTT TLS broker URL
  --device-id  Unique MQTT topic segment
  --username   MQTT username
  --password   MQTT password (prefer environment)
  --ca-file    Additional PEM CA certificates
  --disk-path  Comma-separated filesystem paths
  --interval   Metrics collection interval
Help: -h, --help, help, or <subcommand> -h`)
}
