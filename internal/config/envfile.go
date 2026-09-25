package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EnvVar is one KEY=VALUE line of an env file.
type EnvVar struct{ Key, Value string }

// ReadEnvFile parses a dotenv / systemd EnvironmentFile style file: one
// KEY=VALUE per line, blank lines and #-comments ignored, an optional
// leading `export `, and single- or double-quoted values (quotes stripped,
// no interpolation). An unquoted value ends at " #". Anything else is an
// error naming the line, so a typo never silently drops a secret.
func ReadEnvFile(path string) ([]EnvVar, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []EnvVar
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		key, val, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || !validEnvKey(key) {
			return nil, fmt.Errorf("%s:%d: want KEY=VALUE", path, n)
		}
		val = strings.TrimSpace(val)
		switch {
		case len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0]:
			val = val[1 : len(val)-1]
		case val != "" && (val[0] == '"' || val[0] == '\''):
			return nil, fmt.Errorf("%s:%d: unterminated quote", path, n)
		default:
			if i := strings.Index(val, " #"); i >= 0 {
				val = strings.TrimSpace(val[:i])
			}
		}
		out = append(out, EnvVar{Key: key, Value: val})
	}
	return out, sc.Err()
}

func validEnvKey(k string) bool {
	if k == "" || (k[0] >= '0' && k[0] <= '9') {
		return false
	}
	for _, c := range k {
		if !(c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// LoadEnvFiles reads each file in order and sets its variables in the
// process, later files winning. An env file is an explicit request, so it
// overrides whatever the environment already had (as systemd's
// EnvironmentFile does over the unit's defaults). It runs before config
// resolution, so to Apply its variables look exported by hand: the
// profile's [env] table overrides them only for an explicit profile.
func LoadEnvFiles(paths []string) error {
	for _, p := range paths {
		vars, err := ReadEnvFile(ExpandHome(p))
		if err != nil {
			return fmt.Errorf("--env-file: %w", err)
		}
		for _, v := range vars {
			if err := os.Setenv(v.Key, v.Value); err != nil {
				return err
			}
		}
	}
	return nil
}

// ExpandHome turns a leading ~/ into the home directory (launchd and
// quoted shell arguments never expand it).
func ExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}
