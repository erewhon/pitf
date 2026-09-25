package services

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Agent is the part of a launchd plist adoption needs.
type Agent struct {
	Path  string
	Label string
	Args  []string // ProgramArguments, or [Program]
	Env   map[string]string
}

// ReadFunc decodes one plist file. On macOS it shells out to plutil, which
// reads both XML and binary plists; tests inject a fake.
type ReadFunc func(path string) (Agent, error)

// FindLegacy returns the user agents in dir that run the standalone
// llm-router (the pre-pitf setup), skipping pitf's own. Unreadable plists
// are skipped: they cannot be ours to adopt.
func FindLegacy(dir string, read ReadFunc) ([]Agent, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Agent
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".plist") || strings.HasPrefix(e.Name(), LabelPrefix) {
			continue
		}
		a, err := read(filepath.Join(dir, e.Name()))
		if err != nil || strings.HasPrefix(a.Label, LabelPrefix) {
			continue
		}
		if isLegacyRouter(a) {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// isLegacyRouter: the binary is llm-router(-go), or a shell wrapper whose
// command line mentions it. Other llm-router-go daemons (node-agent,
// tool-proxy, orpheus-say) are not the front door and are left alone.
func isLegacyRouter(a Agent) bool {
	if len(a.Args) == 0 {
		return false
	}
	bin := filepath.Base(a.Args[0])
	if bin == "llm-router" || bin == "llm-router-go" {
		return true
	}
	if isShell(bin) {
		return strings.Contains(strings.Join(a.Args[1:], " "), "llm-router ")
	}
	return false
}

func isShell(bin string) bool {
	switch bin {
	case "sh", "bash", "zsh", "dash", "fish":
		return true
	}
	return false
}

// Adopted is what a legacy router agent contributes to the pitf one.
type Adopted struct {
	ModelsYAML string            // "" when the legacy relied on the default
	Args       []string          // remaining router flags, in order
	Env        map[string]string // its EnvironmentVariables, minus PATH
	Dropped    []string          // flags pitf now owns, for the report
}

// Adopt carries a legacy router's flags and environment over. The listen
// and dashboard flags are pitf's now (loopback 4010/4011); everything else
// (api keys, log format, mode, postgres…) is kept verbatim. A shell wrapper
// cannot be parsed reliably, so it is an error: the operator moves the
// flags to --router-arg and the secrets to the pitf profile's [env].
func Adopt(a Agent) (Adopted, error) {
	if len(a.Args) == 0 {
		return Adopted{}, fmt.Errorf("%s: no ProgramArguments", a.Path)
	}
	if isShell(filepath.Base(a.Args[0])) {
		return Adopted{}, fmt.Errorf("%s runs the router through a shell (%s); pitf cannot tell its flags and secrets apart.\n"+
			"Pass its router flags with --router-arg and put its environment in the profile's [env] table,\n"+
			"then re-run with --replace-legacy", a.Path, strings.Join(a.Args, " "))
	}
	out := Adopted{Env: map[string]string{}}
	for k, v := range a.Env {
		if k == "PATH" {
			continue
		}
		out.Env[k] = v
	}
	owned := map[string]bool{"addr": true, "dashboard": true, "dashboard-addr": true}
	args := a.Args[1:]
	for i := 0; i < len(args); i++ {
		arg := args[i]
		name, val, hasVal := flagName(arg)
		switch {
		case name == "":
			out.Args = append(out.Args, arg)
		case name == "models-yaml":
			if !hasVal && i+1 < len(args) {
				i++
				val = args[i]
			}
			out.ModelsYAML = val
		case owned[name]:
			out.Dropped = append(out.Dropped, arg)
			// -dashboard is a bool; the address flags take a value.
			if name != "dashboard" && !hasVal && i+1 < len(args) {
				i++
				out.Dropped[len(out.Dropped)-1] += " " + args[i]
			}
		default:
			out.Args = append(out.Args, arg)
		}
	}
	return out, nil
}

// flagName splits "-x", "--x" and "-x=v" into name and inline value. A
// non-flag argument yields "".
func flagName(arg string) (name, val string, hasVal bool) {
	if !strings.HasPrefix(arg, "-") || arg == "-" || arg == "--" {
		return "", "", false
	}
	name = strings.TrimLeft(arg, "-")
	name, val, hasVal = strings.Cut(name, "=")
	return name, val, hasVal
}
