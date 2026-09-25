package services

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// State is what launchctl says about one agent.
type State struct {
	Loaded   bool
	Running  bool
	PID      int
	LastExit string // "" when launchd has not recorded one
}

// Launchd is the slice of launchctl pitf uses, behind an interface so the
// commands can be tested off macOS.
type Launchd interface {
	Bootstrap(plist string) error
	Bootout(label string) error
	Kickstart(label string) error
	State(label string) (State, error)
}

// ErrNotMacOS is returned by Host on anything but darwin.
var ErrNotMacOS = errors.New("pitf services manages launchd agents and only runs on macOS")

// Dirs are where pitf writes: the agents, their logs, and the legacy plists
// it moved aside.
type Dirs struct {
	Agents   string // ~/Library/LaunchAgents
	Logs     string // ~/Library/Logs/pitf
	Replaced string // ~/Library/Application Support/pitf/replaced
}

// UserDirs resolves Dirs under the home directory.
func UserDirs() (Dirs, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Dirs{}, err
	}
	lib := filepath.Join(home, "Library")
	return Dirs{
		Agents:   filepath.Join(lib, "LaunchAgents"),
		Logs:     filepath.Join(lib, "Logs", "pitf"),
		Replaced: filepath.Join(lib, "Application Support", "pitf", "replaced"),
	}, nil
}

// PlistPath is where a service's plist lives.
func (d Dirs) PlistPath(name string) string {
	return filepath.Join(d.Agents, Label(name)+".plist")
}

// Host returns the real launchctl on macOS.
func Host() (Launchd, error) {
	if runtime.GOOS != "darwin" {
		return nil, ErrNotMacOS
	}
	return launchctl{domain: "gui/" + strconv.Itoa(os.Getuid())}, nil
}

type launchctl struct{ domain string }

func (l launchctl) run(args ...string) (string, error) {
	var out bytes.Buffer
	c := exec.Command("launchctl", args...)
	c.Stdout, c.Stderr = &out, &out
	err := c.Run()
	if err != nil {
		return out.String(), fmt.Errorf("launchctl %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(out.String()))
	}
	return out.String(), nil
}

func (l launchctl) Bootstrap(plist string) error {
	_, err := l.run("bootstrap", l.domain, plist)
	return err
}

// Bootout of an agent that is not loaded is not an error.
func (l launchctl) Bootout(label string) error {
	if st, _ := l.State(label); !st.Loaded {
		return nil
	}
	_, err := l.run("bootout", l.domain+"/"+label)
	return err
}

func (l launchctl) Kickstart(label string) error {
	_, err := l.run("kickstart", "-k", l.domain+"/"+label)
	return err
}

func (l launchctl) State(label string) (State, error) {
	out, err := exec.Command("launchctl", "print", l.domain+"/"+label).CombinedOutput()
	if err != nil {
		// "Could not find service" is launchctl's way of saying not loaded.
		return State{}, nil
	}
	return parsePrint(string(out)), nil
}

// parsePrint reads the few fields pitf reports from `launchctl print`.
func parsePrint(out string) State {
	st := State{Loaded: true}
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), " = ")
		if !ok {
			continue
		}
		switch k {
		case "state":
			st.Running = v == "running"
		case "pid":
			st.PID, _ = strconv.Atoi(v)
		case "last exit code":
			if st.LastExit == "" {
				st.LastExit = v
			}
		}
	}
	return st
}

// ReadPlist decodes a plist with plutil (XML or binary).
func ReadPlist(path string) (Agent, error) {
	out, err := exec.Command("plutil", "-convert", "json", "-o", "-", path).Output()
	if err != nil {
		return Agent{}, fmt.Errorf("plutil %s: %w", path, err)
	}
	return decodePlistJSON(path, out)
}

func decodePlistJSON(path string, data []byte) (Agent, error) {
	var raw struct {
		Label                string            `json:"Label"`
		Program              string            `json:"Program"`
		ProgramArguments     []string          `json:"ProgramArguments"`
		EnvironmentVariables map[string]string `json:"EnvironmentVariables"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return Agent{}, fmt.Errorf("%s: %w", path, err)
	}
	a := Agent{Path: path, Label: raw.Label, Args: raw.ProgramArguments, Env: raw.EnvironmentVariables}
	if raw.Program != "" && (len(a.Args) == 0 || a.Args[0] != raw.Program) {
		// Program overrides argv[0] for the exec; ProgramArguments[0] is then
		// only the name the process sees.
		if len(a.Args) == 0 {
			a.Args = []string{raw.Program}
		} else {
			a.Args = append([]string{raw.Program}, a.Args[1:]...)
		}
	}
	return a, nil
}
