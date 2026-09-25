package services

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Manager applies specs to launchd.
type Manager struct {
	Dirs  Dirs
	L     Launchd
	Out   io.Writer
	Probe func(ctx context.Context, url string) error // nil skips probes
}

// Install writes each plist and (re)loads the agents whose plist changed or
// that are not loaded. A legacy router agent, if given, is booted out
// before the new router starts (they want the same port) and its plist is
// moved to Dirs.Replaced, never deleted.
func (m Manager) Install(specs []Spec, legacy *Agent) error {
	for _, d := range []string{m.Dirs.Agents, m.Dirs.Logs} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	if legacy != nil {
		if err := m.retire(*legacy); err != nil {
			return err
		}
	}
	for _, s := range specs {
		path := m.Dirs.PlistPath(s.Name)
		want := Plist(s, m.Dirs.Logs)
		have, _ := os.ReadFile(path)
		changed := !bytes.Equal(have, want)
		if changed {
			// 0600: an adopted router env can carry upstream credentials.
			if err := os.WriteFile(path, want, 0o600); err != nil {
				return err
			}
		}
		st, err := m.L.State(Label(s.Name))
		if err != nil {
			return err
		}
		switch {
		case changed && st.Loaded:
			if err := m.L.Bootout(Label(s.Name)); err != nil {
				return err
			}
			fallthrough
		case !st.Loaded:
			if err := m.L.Bootstrap(path); err != nil {
				return err
			}
			verb := "installed"
			if len(have) > 0 {
				verb = "updated"
			}
			fmt.Fprintf(m.Out, "%-9s %s  %s\n", s.Name, verb, path)
		default:
			fmt.Fprintf(m.Out, "%-9s unchanged\n", s.Name)
		}
	}
	return nil
}

func (m Manager) retire(a Agent) error {
	if a.Label != "" {
		if err := m.L.Bootout(a.Label); err != nil {
			return fmt.Errorf("stopping legacy router %s: %w", a.Label, err)
		}
	}
	if err := os.MkdirAll(m.Dirs.Replaced, 0o700); err != nil {
		return err
	}
	dst := filepath.Join(m.Dirs.Replaced, filepath.Base(a.Path))
	if err := os.Rename(a.Path, dst); err != nil {
		return err
	}
	fmt.Fprintf(m.Out, "legacy    stopped %s; plist moved to %s\n", a.Label, dst)
	return nil
}

// Uninstall boots out and removes every pitf agent. Logs and any replaced
// legacy plists stay.
func (m Manager) Uninstall() error {
	for _, name := range Names {
		path := m.Dirs.PlistPath(name)
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err := m.L.Bootout(Label(name)); err != nil {
			return err
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		fmt.Fprintf(m.Out, "%-9s removed\n", name)
	}
	if entries, _ := os.ReadDir(m.Dirs.Replaced); len(entries) > 0 {
		fmt.Fprintf(m.Out, "\nThe router agent pitf replaced is kept in %s.\n"+
			"To go back: mv it into %s and `launchctl bootstrap gui/$(id -u) <plist>`.\n", m.Dirs.Replaced, m.Dirs.Agents)
	}
	return nil
}

// Installed lists the service names whose plist exists.
func (m Manager) Installed() []string {
	var out []string
	for _, name := range Names {
		if _, err := os.Stat(m.Dirs.PlistPath(name)); err == nil {
			out = append(out, name)
		}
	}
	return out
}

// Up loads any installed agent that is not loaded (after a `services
// stop`, a bootout, or a plist copied in by hand). It returns
// ErrNotInstalled when there is nothing to start.
func (m Manager) Up() error {
	names := m.Installed()
	if len(names) == 0 {
		return ErrNotInstalled
	}
	for _, name := range names {
		st, err := m.L.State(Label(name))
		if err != nil {
			return err
		}
		if st.Loaded {
			continue
		}
		if err := m.L.Bootstrap(m.Dirs.PlistPath(name)); err != nil {
			return err
		}
		fmt.Fprintf(m.Out, "%-9s started\n", name)
	}
	return nil
}

// ErrNotInstalled: `pitf up` before `pitf services install`.
var ErrNotInstalled = errors.New("no pitf services installed; run `pitf services install` first")

// Stop boots out every installed agent without removing it; `pitf up`
// or `pitf services start` brings them back.
func (m Manager) Stop() error {
	for _, name := range m.Installed() {
		if err := m.L.Bootout(Label(name)); err != nil {
			return err
		}
		fmt.Fprintf(m.Out, "%-9s stopped\n", name)
	}
	return nil
}

// Restart kickstarts the named services (all installed when none named).
func (m Manager) Restart(names []string) error {
	if len(names) == 0 {
		names = m.Installed()
	}
	for _, name := range names {
		if _, err := os.Stat(m.Dirs.PlistPath(name)); err != nil {
			return fmt.Errorf("%s is not installed", name)
		}
		if err := m.L.Kickstart(Label(name)); err != nil {
			return err
		}
		fmt.Fprintf(m.Out, "%-9s restarted\n", name)
	}
	return nil
}

// Status prints one line per service: installed?, launchd state, and
// whether its URL answers.
func (m Manager) Status(ctx context.Context, specs []Spec) error {
	for _, s := range specs {
		line := fmt.Sprintf("%-9s ", s.Name)
		if _, err := os.Stat(m.Dirs.PlistPath(s.Name)); err != nil {
			fmt.Fprintln(m.Out, line+"not installed")
			continue
		}
		st, err := m.L.State(Label(s.Name))
		if err != nil {
			return err
		}
		switch {
		case !st.Loaded:
			line += "stopped"
		case st.Running:
			line += fmt.Sprintf("running (pid %d)", st.PID)
		case s.Interval > 0:
			line += fmt.Sprintf("idle, every %s", s.Interval)
		default:
			line += "loaded, not running"
		}
		if st.LastExit != "" && st.LastExit != "0" && st.LastExit != "(never exited)" {
			line += ", last exit " + st.LastExit
		}
		if s.URL != "" && m.Probe != nil && st.Loaded {
			if err := m.Probe(ctx, s.URL); err != nil {
				line += "  ✗ " + s.URL
			} else {
				line += "  ✓ " + s.URL
			}
		}
		fmt.Fprintln(m.Out, line)
	}
	fmt.Fprintf(m.Out, "\nlogs: %s\n", m.Dirs.Logs)
	return nil
}

// WaitFor polls url until it answers or the timeout passes.
func (m Manager) WaitFor(ctx context.Context, url string, timeout time.Duration) error {
	if m.Probe == nil {
		return nil
	}
	deadline := time.Now().Add(timeout)
	for {
		err := m.Probe(ctx, url)
		if err == nil || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}
