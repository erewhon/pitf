package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func writeExec(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLookupExternalWalksPathInOrder(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	want := writeExec(t, first, "pitf-hello", "#!/bin/sh\n")
	writeExec(t, second, "pitf-hello", "#!/bin/sh\n")
	pathEnv := first + string(os.PathListSeparator) + second

	got, ok := lookupExternal("hello", pathEnv)
	if !ok || got != want {
		t.Fatalf("lookupExternal = %q, %v; want %q, true", got, ok, want)
	}
	if _, ok := lookupExternal("missing", pathEnv); ok {
		t.Fatal("found an external that does not exist")
	}
	if _, ok := lookupExternal("../hello", pathEnv); ok {
		t.Fatal("path traversal in a subcommand name must not resolve")
	}
}

func TestLookupExternalIgnoresNonExecutables(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "pitf-plain")
	if err := os.WriteFile(p, []byte("not a program"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := lookupExternal("plain", dir); ok {
		t.Fatal("non-executable file resolved as an external command")
	}
	if err := os.Mkdir(filepath.Join(dir, "pitf-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok := lookupExternal("dir", dir); ok {
		t.Fatal("directory resolved as an external command")
	}
}

func TestListExternalsSortedAndDeduped(t *testing.T) {
	a := t.TempDir()
	b := t.TempDir()
	writeExec(t, a, "pitf-zeta", "#!/bin/sh\n")
	writeExec(t, a, "pitf-alpha", "#!/bin/sh\n")
	writeExec(t, b, "pitf-alpha", "#!/bin/sh\n")
	writeExec(t, b, "pitf-", "#!/bin/sh\n") // bare prefix is not a command
	writeExec(t, b, "unrelated", "#!/bin/sh\n")

	got := listExternals(a + string(os.PathListSeparator) + b)
	want := []string{"alpha", "zeta"}
	if len(got) != len(want) {
		t.Fatalf("listExternals = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("listExternals = %v, want %v", got, want)
		}
	}
}

func TestExternalCandidateSkipsBuiltinsAndFlags(t *testing.T) {
	root := NewRoot("test")
	cases := []struct {
		args []string
		ok   bool
		name string
	}{
		{nil, false, ""},
		{[]string{"--version"}, false, ""},
		{[]string{"help"}, false, ""},
		{[]string{"completion", "zsh"}, false, ""},
		{[]string{"hello", "--flag", "x"}, true, "hello"},
	}
	for _, c := range cases {
		name, rest, ok := externalCandidate(root, c.args)
		if ok != c.ok || name != c.name {
			t.Errorf("externalCandidate(%v) = %q, %v; want %q, %v", c.args, name, ok, c.name, c.ok)
		}
		if ok && len(rest) != len(c.args)-1 {
			t.Errorf("externalCandidate(%v) rest = %v", c.args, rest)
		}
	}
}
