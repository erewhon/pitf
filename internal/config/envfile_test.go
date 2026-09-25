package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeEnv(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "router.env")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadEnvFile(t *testing.T) {
	p := writeEnv(t, `# upstream keys
AWS_BEARER_TOKEN_BEDROCK=abc123
export LOCAL_KEY="with spaces # not a comment"
SINGLE='x=y'
TRAILING=plain # comment
EMPTY=

  INDENTED = v
`)
	got, err := ReadEnvFile(p)
	if err != nil {
		t.Fatal(err)
	}
	want := []EnvVar{{"AWS_BEARER_TOKEN_BEDROCK", "abc123"}, {"LOCAL_KEY", "with spaces # not a comment"},
		{"SINGLE", "x=y"}, {"TRAILING", "plain"}, {"EMPTY", ""}, {"INDENTED", "v"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	for _, bad := range []string{"NOEQUALS\n", "1BAD=x\n", "A-B=x\n", `Q="open` + "\n"} {
		if _, err := ReadEnvFile(writeEnv(t, "OK=1\n"+bad)); err == nil || !strings.Contains(err.Error(), ":2:") {
			t.Errorf("%q: want a line-2 error, got %v", bad, err)
		}
	}
}

func TestLoadEnvFilesOverridesInOrder(t *testing.T) {
	t.Setenv("PITF_T_A", "ambient")
	t.Setenv("PITF_T_B", "")
	a := writeEnv(t, "PITF_T_A=file1\nPITF_T_B=file1\n")
	b := writeEnv(t, "PITF_T_B=file2\n")
	if err := LoadEnvFiles([]string{a, b}); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("PITF_T_A") != "file1" || os.Getenv("PITF_T_B") != "file2" {
		t.Fatalf("A=%q B=%q", os.Getenv("PITF_T_A"), os.Getenv("PITF_T_B"))
	}
	if err := LoadEnvFiles([]string{filepath.Join(t.TempDir(), "missing.env")}); err == nil {
		t.Fatal("missing file accepted")
	}
}
