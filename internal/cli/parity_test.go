package cli

import (
	"io"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestAutostartEnabled(t *testing.T) {
	cases := []struct {
		autostart, noAutostart string
		want                   bool
	}{
		{"", "", true},
		{"1", "", true},
		{"0", "", false},
		{"false", "", false},
		{"off", "", false},
		{"", "1", false},
		{"", "yes", false},
		{"", "0", true},
	}
	for _, c := range cases {
		t.Setenv(AutostartEnv, c.autostart)
		t.Setenv(NoAutostartEnv, c.noAutostart)
		if got := autostartEnabled(); got != c.want {
			t.Errorf("%s=%q %s=%q: autostartEnabled = %v, want %v",
				AutostartEnv, c.autostart, NoAutostartEnv, c.noAutostart, got, c.want)
		}
	}
}

func TestSplitAutostartFlag(t *testing.T) {
	rest, off := splitAutostartFlag([]string{NoAutostartFlag, "ps", "-a"})
	if !off || !reflect.DeepEqual(rest, []string{"ps", "-a"}) {
		t.Errorf("leading flag: %v, %v", rest, off)
	}
	// Only the leading position counts: a container command may well have
	// an argument spelt the same way, and it is not ours to eat.
	args := []string{"run", "alpine", "echo", NoAutostartFlag}
	rest, off = splitAutostartFlag(args)
	if off || !reflect.DeepEqual(rest, args) {
		t.Errorf("later occurrence: %v, %v", rest, off)
	}
}

func TestClientOnly(t *testing.T) {
	for _, args := range [][]string{nil, {"--help"}, {"-h"}, {"help", "run"}, {"--version"}, {"completion", "zsh"}} {
		if !clientOnly(args) {
			t.Errorf("clientOnly(%v) = false, want true", args)
		}
	}
	for _, args := range [][]string{{"ps"}, {"version"}, {"run", "alpine"}, {"compose", "up"}} {
		if clientOnly(args) {
			t.Errorf("clientOnly(%v) = true, want false", args)
		}
	}
}

func TestWrapperArgs(t *testing.T) {
	root := NewRootCmd()
	cases := []struct {
		args []string
		want []string
	}{
		{[]string{"jpodman", "ps"}, []string{"jpodman", "podman", "ps"}},
		{[]string{"/opt/homebrew/bin/jdocker", "compose", "up"}, []string{"/opt/homebrew/bin/jdocker", "docker", "compose", "up"}},
		{[]string{"jm", "list"}, []string{"jm", "list"}},
		// A detached helper launched from a wrapper symlink keeps its own
		// arguments: rewriting them would hand the machine's forwarder or
		// resolver to podman.
		{
			[]string{"jpodman", "--state-root", "/s", "_forwarder", "dev"},
			[]string{"jpodman", "--state-root", "/s", "_forwarder", "dev"},
		},
	}
	for _, c := range cases {
		if got := wrapperArgs(root, c.args); !reflect.DeepEqual(got, c.want) {
			t.Errorf("wrapperArgs(%v) = %v, want %v", c.args, got, c.want)
		}
	}
}

func TestDockerEnv(t *testing.T) {
	env := []string{"PATH=/bin", DockerHostEnv + "=tcp://elsewhere:2375", DockerContextEnv + "=desktop", "HOME=/u"}
	got := dockerEnv(env, "unix:///tmp/jm.sock")
	// The guest engine is FreeBSD and the docker CLI has no --os flag, so
	// the wrapper defaults the platform to linux/<arch>; without it a fresh
	// pull asks the registry for OS "freebsd" and fails.
	want := []string{"PATH=/bin", "HOME=/u", DockerHostEnv + "=unix:///tmp/jm.sock",
		DockerPlatformEnv + "=linux/" + runtime.GOARCH}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("dockerEnv = %v, want %v", got, want)
	}
}

// Anyone building native FreeBSD images sets the platform themselves; the
// wrapper must not overrule them.
func TestDockerEnvKeepsAChosenPlatform(t *testing.T) {
	env := []string{"PATH=/bin", DockerPlatformEnv + "=freebsd/arm64"}
	got := dockerEnv(env, "unix:///tmp/jm.sock")
	want := []string{"PATH=/bin", DockerPlatformEnv + "=freebsd/arm64", DockerHostEnv + "=unix:///tmp/jm.sock"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("dockerEnv = %v, want %v", got, want)
	}
}

func TestParseGuestClock(t *testing.T) {
	host := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	behind := host.Add(-90 * time.Second).Unix()
	gc, err := parseGuestClock(strconv.FormatInt(behind, 10)+"\nrtcsync=running\n", host)
	if err != nil {
		t.Fatal(err)
	}
	if gc.Skew != -90*time.Second || !gc.Service {
		t.Errorf("parseGuestClock = %+v", gc)
	}
	if got := skewWord(gc.Skew); !strings.Contains(got, "behind") {
		t.Errorf("skewWord = %q", got)
	}
	gc, err = parseGuestClock(strconv.FormatInt(host.Unix(), 10)+"\nrtcsync=absent\n", host)
	if err != nil || gc.Service || gc.Skew != 0 {
		t.Errorf("parseGuestClock in step = %+v, %v", gc, err)
	}
	if got := skewWord(gc.Skew); got != "in step with" {
		t.Errorf("skewWord(0) = %q", got)
	}
	if _, err := parseGuestClock("date: not found\n", host); err == nil {
		t.Error("garbage accepted")
	}
	if _, err := parseGuestClock("", host); err == nil {
		t.Error("empty output accepted")
	}
}

// The client wrappers disable flag parsing so that podman's and docker's own
// flags reach them untouched; jm's own global flags, typed before the
// subcommand, must still be jm's. Without that, "jm --state-root DIR podman
// ps" resolves (and autostarts) a machine in the default state root and
// hands "--state-root DIR" to the client.
func TestGlobalFlagsReachTheWrappers(t *testing.T) {
	saved := stateRoot
	t.Cleanup(func() { stateRoot = saved })
	cases := []struct {
		name string
		argv []string
		want []string
	}{
		{"podman", []string{"ps", "--all"}, []string{"ps", "--all"}},
		{"docker", []string{"compose", "up", "-d"}, []string{"compose", "up", "-d"}},
		// The autostart opt-out is recognised by position, so it has to
		// survive traversal as the first argument.
		{"podman", []string{NoAutostartFlag, "ps"}, []string{NoAutostartFlag, "ps"}},
	}
	for _, c := range cases {
		dir := t.TempDir()
		root := NewRootCmd()
		var got []string
		var ran bool
		for _, sub := range root.Commands() {
			if sub.Name() == c.name {
				sub.RunE = func(_ *cobra.Command, args []string) error {
					got, ran = args, true
					return nil
				}
			}
		}
		root.SetOut(io.Discard)
		root.SetErr(io.Discard)
		root.SetArgs(append([]string{"--state-root", dir, c.name}, c.argv...))
		if err := root.Execute(); err != nil {
			t.Fatalf("jm --state-root %s %s %v: %v", dir, c.name, c.argv, err)
		}
		if !ran {
			t.Fatalf("%s: the wrapper never ran", c.name)
		}
		if stateRoot != dir {
			t.Errorf("%s: state root = %q, want %q", c.name, stateRoot, dir)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: client args = %v, want %v", c.name, got, c.want)
		}
	}
}
