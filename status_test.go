package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestStatusDoesNotRequireRoot(t *testing.T) {
	wantErr := errors.New("systemctl unavailable")
	s := system{
		uid: func() int { t.Fatal("status checked root"); return 1000 },
		runStatus: func(args ...string) error {
			want := []string{"status", "--no-pager", "--full", "stats-linux"}
			if !reflect.DeepEqual(args, want) {
				t.Fatalf("args = %v, want %v", args, want)
			}
			return wantErr
		},
	}
	if err := s.status(); !errors.Is(err, wantErr) {
		t.Fatalf("status error = %v, want %v", err, wantErr)
	}
}

func TestStatusCLIHelper(t *testing.T) {
	if os.Getenv("STATS_TEST_STATUS_HELPER") != "1" {
		return
	}
	os.Args = []string{"stats-linux", "status"}
	main()
	os.Exit(0)
}

func TestStatusCLI(t *testing.T) {
	for _, code := range []int{0, 1, 3, 4} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			dir := t.TempDir()
			script := fmt.Sprintf("#!/bin/sh\n[ \"$*\" = 'status --no-pager --full stats-linux' ] || exit 99\nprintf 'service status details\\n'\nprintf 'systemctl diagnostic\\n' >&2\nexit %d\n", code)
			if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestStatusCLIHelper$")
			cmd.Env = append(os.Environ(), "STATS_TEST_STATUS_HELPER=1", "PATH="+dir,
				"STATS_BROKER=invalid", "STATS_INTERVAL=invalid")
			var stdout, stderr strings.Builder
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if got := exitCode(cmd.Run()); got != code {
				t.Fatalf("exit = %d, want %d; stderr: %s", got, code, stderr.String())
			}
			if stdout.String() != "service status details\n" || !strings.Contains(stderr.String(), "systemctl diagnostic\n") {
				t.Fatalf("missing output: stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
		})
	}
}

func TestStatusRejectsArguments(t *testing.T) {
	for _, args := range [][]string{{"status", "--unknown"}, {"status", "other-unit"}} {
		if err := command(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestStatusMissingSystemctl(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	err := command([]string{"status"})
	if err == nil || !strings.Contains(err.Error(), "systemctl") || exitCode(err) != 1 {
		t.Fatalf("unexpected missing-command error: %v", err)
	}
}

func TestExitCode(t *testing.T) {
	if exitCode(nil) != 0 || exitCode(errors.New("failure")) != 1 {
		t.Fatal("incorrect success or generic failure exit code")
	}
}
