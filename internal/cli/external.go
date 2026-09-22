package cli

import (
	"os"
	"path/filepath"
	"strings"
)

const externalPrefix = "pitf-"

// lookupExternal finds an executable named pitf-<name> on the given PATH
// string. It mirrors exec.LookPath but takes PATH explicitly so tests can
// control it.
func lookupExternal(name, pathEnv string) (string, bool) {
	if name == "" || strings.ContainsAny(name, `/\`) {
		return "", false
	}
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			dir = "."
		}
		p := filepath.Join(dir, externalPrefix+name)
		if isExecutable(p) {
			return p, true
		}
	}
	return "", false
}

// listExternals returns the sorted, de-duplicated set of <name>s for every
// executable pitf-<name> on PATH. First hit per name wins, like lookup.
func listExternals(pathEnv string) []string {
	seen := map[string]struct{}{}
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			dir = "."
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			n := e.Name()
			if !strings.HasPrefix(n, externalPrefix) || n == externalPrefix {
				continue
			}
			if isExecutable(filepath.Join(dir, n)) {
				seen[strings.TrimPrefix(n, externalPrefix)] = struct{}{}
			}
		}
	}
	return sortedKeys(seen)
}

func isExecutable(p string) bool {
	fi, err := os.Stat(p)
	if err != nil || fi.IsDir() {
		return false
	}
	return fi.Mode()&0o111 != 0
}
