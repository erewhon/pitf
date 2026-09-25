// Package services runs the laptop stack (router, tokenator UI, the pitf
// dashboard, a periodic tokenator ingest) as per-user launchd agents, each
// one a `pitf …` command line so it resolves the same config, keys and
// cross-link environment as an interactive run.
//
// The pure half (specs, plist rendering, legacy-plist adoption) is
// platform-neutral and tested anywhere; the launchctl half only works on
// macOS.
package services

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// LabelPrefix namespaces every agent pitf owns. Anything else in
// ~/Library/LaunchAgents is left alone, except an adopted legacy router.
const LabelPrefix = "org.erewhon.pitf."

// Defaults for the laptop stack. The router listens on loopback: a laptop
// router usually runs without --api-keys, and anything on the office
// network could otherwise spend its upstream credentials.
const (
	DefaultRouterAddr    = "127.0.0.1:4010"
	DefaultDashboardAddr = "127.0.0.1:4011"
	DefaultIngestEvery   = 5 * time.Minute
)

// Spec is one launchd agent.
type Spec struct {
	Name string   // short name: router, tokens, dashboard, ingest
	Args []string // full argv, argv[0] is the pitf binary
	Env  map[string]string
	// KeepAlive restarts a long-running server whenever it exits. A
	// periodic job sets Interval instead and runs to completion.
	KeepAlive bool
	Interval  time.Duration
	// URL is where the service answers, for status probes ("" = none).
	URL string
}

// Label is the launchd label for a service name.
func Label(name string) string { return LabelPrefix + name }

// Names lists the services in the order they are installed and started.
var Names = []string{"router", "tokens", "dashboard", "ingest"}

// Options are what `pitf services install` decides.
type Options struct {
	Pitf    string // absolute path of the pitf binary the agents run
	Profile string // baked in as --profile when non-empty

	ModelsYAML    string
	RouterAddr    string
	DashboardAddr string
	RouterArgs    []string // extra router flags (after adoption)
	RouterEnv     map[string]string

	IngestEvery time.Duration
	IngestArgs  []string

	// DashboardURL, when set, is exported to the pitf dashboard agent as
	// PITF_DASHBOARD_URL, for configs that name no [tools] dashboard_url.
	DashboardURL string

	Path string // PATH for every agent; launchd's default is too bare
}

// DefaultPath covers Homebrew on both architectures plus the system dirs,
// so api_key_cmd (security, op, pass) and tmux resolve under launchd.
const DefaultPath = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

// Plan turns the options into the four agents.
func Plan(o Options) []Spec {
	base := []string{o.Pitf}
	if o.Profile != "" {
		base = append(base, "--profile", o.Profile)
	}
	with := func(args ...string) []string {
		return append(append([]string(nil), base...), args...)
	}
	path := o.Path
	if path == "" {
		path = DefaultPath
	}
	env := func(extra map[string]string) map[string]string {
		m := map[string]string{"PATH": path}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}

	router := with("router", "serve", "-models-yaml", o.ModelsYAML, "-addr", o.RouterAddr,
		"-dashboard", "-dashboard-addr", o.DashboardAddr)
	router = append(router, o.RouterArgs...)

	dashEnv := map[string]string{}
	if o.DashboardURL != "" {
		dashEnv["PITF_DASHBOARD_URL"] = o.DashboardURL
	}
	every := o.IngestEvery
	if every <= 0 {
		every = DefaultIngestEvery
	}
	return []Spec{
		{Name: "router", Args: router, Env: env(o.RouterEnv), KeepAlive: true,
			URL: "http://" + o.RouterAddr + "/health"},
		{Name: "tokens", Args: with("tokens", "serve"), Env: env(nil), KeepAlive: true,
			URL: "http://127.0.0.1:8990/"},
		{Name: "dashboard", Args: with("dashboard"), Env: env(dashEnv), KeepAlive: true,
			URL: "http://127.0.0.1:8960/"},
		{Name: "ingest", Args: append(with("tokens", "ingest"), o.IngestArgs...), Env: env(nil),
			Interval: every},
	}
}

// Plist renders a spec as a launchd property list. Logs go to
// <logDir>/<name>.log (stdout and stderr together).
func Plist(s Spec, logDir string) []byte {
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
`)
	key := func(k string) { fmt.Fprintf(&b, "\t<key>%s</key>\n", esc(k)) }
	str := func(indent, v string) { fmt.Fprintf(&b, "%s<string>%s</string>\n", indent, esc(v)) }

	key("Label")
	str("\t", Label(s.Name))
	key("ProgramArguments")
	b.WriteString("\t<array>\n")
	for _, a := range s.Args {
		str("\t\t", a)
	}
	b.WriteString("\t</array>\n")
	if len(s.Env) > 0 {
		key("EnvironmentVariables")
		b.WriteString("\t<dict>\n")
		keys := make([]string, 0, len(s.Env))
		for k := range s.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "\t\t<key>%s</key>\n", esc(k))
			str("\t\t", s.Env[k])
		}
		b.WriteString("\t</dict>\n")
	}
	key("RunAtLoad")
	b.WriteString("\t<true/>\n")
	if s.KeepAlive {
		key("KeepAlive")
		b.WriteString("\t<true/>\n")
		// A crash loop (bad models.yaml, port taken) should not spin.
		key("ThrottleInterval")
		b.WriteString("\t<integer>10</integer>\n")
	}
	if s.Interval > 0 {
		key("StartInterval")
		fmt.Fprintf(&b, "\t<integer>%d</integer>\n", int(s.Interval/time.Second))
	}
	log := filepath.Join(logDir, s.Name+".log")
	key("StandardOutPath")
	str("\t", log)
	key("StandardErrorPath")
	str("\t", log)
	key("ProcessType")
	if s.KeepAlive {
		str("\t", "Interactive") // the router is on the latency path of every agent call
	} else {
		str("\t", "Background")
	}
	b.WriteString("</dict>\n</plist>\n")
	return b.Bytes()
}

func esc(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
