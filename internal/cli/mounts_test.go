package cli

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestFindMount(t *testing.T) {
	cases := []struct {
		args []string
		want string // resolved mount name, "" for not-a-mount
		rest int
	}{
		{[]string{"monitor", "--list"}, "monitor", 1},
		{[]string{"tokens"}, "tokens", 0},
		{[]string{"router", "serve", "--help"}, "serve", 1},
		{[]string{"router"}, "", 0},         // group with no sub → cobra help
		{[]string{"router", "nope"}, "", 0}, // unknown sub → cobra error
		{[]string{"bench"}, "", 0},          // external, not a mount
		{nil, "", 0},
	}
	for _, c := range cases {
		m, rest, ok := findMount(c.args)
		got := ""
		if ok {
			got = m.name
		}
		if got != c.want || (ok && len(rest) != c.rest) {
			t.Errorf("findMount(%v) = %q rest=%v ok=%v; want %q rest len %d", c.args, got, rest, ok, c.want, c.rest)
		}
	}
}

func runMounted(t *testing.T, bin string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+t.TempDir(), "PITF_PROFILE=", "ROUTER_API_KEY=")
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run %v: %v\n%s", args, err, out)
	}
	return string(out), code
}

func TestMountedHelpExitsZeroAndUsageExitsTwo(t *testing.T) {
	bin := buildPitf(t)
	cases := []struct {
		args     []string
		code     int
		contains string
	}{
		{[]string{"monitor", "--help"}, 0, "Usage of agent-monitor"},
		{[]string{"monitor", "-bogus"}, 2, "flag provided but not defined: -bogus"},
		{[]string{"tokens", "--help"}, 0, "tokenator"},
		{[]string{"tokens", "frobnicate"}, 2, "unknown command"},
		{[]string{"router", "serve", "--help"}, 0, "Usage"},
		{[]string{"router", "serve", "--bogus"}, 2, "flag provided but not defined"},
		{[]string{"router", "say", "--help"}, 0, "usage: orpheus-say"},
		{[]string{"router"}, 0, "serve"}, // group help lists subs
		{[]string{"router", "nope"}, 2, "unknown subcommand"},
		{[]string{"--profile", "nope", "tokens", "--help"}, 1, `profile "nope"`}, // config error before the tool runs
	}
	for _, c := range cases {
		out, code := runMounted(t, bin, c.args...)
		if code != c.code || !strings.Contains(out, c.contains) {
			t.Errorf("pitf %v: exit %d (want %d); output %q (want it to contain %q)", c.args, code, c.code, out, c.contains)
		}
	}
}

func TestMountsListedInHelpAndWinOverExternals(t *testing.T) {
	bin := buildPitf(t)
	dir := t.TempDir()
	writeExec(t, dir, "pitf-tokens", "#!/bin/sh\necho SHADOWED\n")
	cmd := exec.Command(bin, "help")
	cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "XDG_CONFIG_HOME="+t.TempDir())
	out, _ := cmd.CombinedOutput()
	for _, name := range []string{"monitor", "tokens", "router"} {
		if !strings.Contains(string(out), "  "+name) {
			t.Errorf("help does not list mount %q:\n%s", name, out)
		}
	}
	cmd = exec.Command(bin, "tokens", "--help")
	cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "XDG_CONFIG_HOME="+t.TempDir())
	out, _ = cmd.CombinedOutput()
	if strings.Contains(string(out), "SHADOWED") {
		t.Fatal("an external pitf-tokens shadowed the compiled-in mount")
	}
}
