package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"

	"github.com/erewhon/pitf/internal/config"
)

// pyTool is a Python tool pitf runs in place through uv: `pitf <name> …`
// becomes `uv run -q --project <smithy_dir>/<project> <script> …`. The
// tools stay Python; pitf only finds the checkout and hands them the
// resolved profile environment, which the old pitf-<name> shell shims
// never did. The checkout root is [tools].smithy_dir / PITF_SMITHY_DIR
// (default ~/code/smithy).
type pyTool struct {
	name    string // pitf subcommand
	short   string
	project string // directory under smithy_dir holding pyproject.toml
	script  string // console script from that project's [project.scripts]
	repo    string // where to clone it from, for `pitf doctor`
}

var pyTools = []pyTool{
	{name: "qual", short: "llm-router-qual: quality evals against the router (Python, via uv)",
		project: "llm-router", script: "llm-router-qual", repo: "ssh://code-mesh.m.bcc.sh:23231/erewhon/llm-router.git"},
	{name: "forge", short: "forge: the coding pipeline and Forge task tools (Python, via uv)",
		project: "forge", script: "forge", repo: "https://github.com/erewhon/forge.git"},
	{name: "meta", short: "meta: the life-automation agents front door (Python, via uv)",
		project: "meta", script: "meta", repo: "ssh://code-mesh.m.bcc.sh:23231/smithy/meta.git"},
}

func pyToolNames() []string {
	names := make([]string, 0, len(pyTools))
	for _, t := range pyTools {
		names = append(names, t.name)
	}
	sort.Strings(names)
	return names
}

// smithyDir is the checkout root after runMount applied the config: the
// exported PITF_SMITHY_DIR, else the default. Reading the environment (not
// the config) keeps the same precedence as every other tool variable.
func smithyDir() string {
	if d := os.Getenv(config.EnvPitfSmithyDir); d != "" {
		return config.ExpandHome(d)
	}
	return config.DefaultSmithyDir()
}

// projectDir is where t's pyproject.toml should be under root.
func (t pyTool) projectDir(root string) string { return filepath.Join(root, t.project) }

// present reports whether the checkout exists (pyproject.toml is the test,
// as the shims did).
func (t pyTool) present(root string) bool {
	return fileExists(filepath.Join(t.projectDir(root), "pyproject.toml"))
}

// missingHint is the one line that tells the operator how to fix a missing
// checkout.
func (t pyTool) missingHint(root string) string {
	return fmt.Sprintf("git clone %s %s (or set [tools].smithy_dir / PITF_SMITHY_DIR)", t.repo, t.projectDir(root))
}

const uvInstallHint = "install uv: brew install uv, or curl -LsSf https://astral.sh/uv/install.sh | sh"

// run execs uv for the tool. Missing prerequisites exit 127 like a shell
// would for a command it cannot find; everything else is uv's and the
// tool's own.
func (t pyTool) run(_ context.Context, args []string) error {
	root := smithyDir()
	if !t.present(root) {
		return &ExitError{Code: 127, Msg: fmt.Sprintf("pitf %s: project not found at %s\n  %s", t.name, t.projectDir(root), t.missingHint(root))}
	}
	uv, err := exec.LookPath("uv")
	if err != nil {
		return &ExitError{Code: 127, Msg: fmt.Sprintf("pitf %s: uv not on PATH\n  %s", t.name, uvInstallHint)}
	}
	argv := append([]string{"run", "-q", "--project", t.projectDir(root), t.script}, args...)
	return runExternal(context.Background(), uv, argv)
}

func init() {
	for _, t := range pyTools {
		t := t
		registerMount(&mount{
			name:  t.name,
			short: t.short,
			run:   t.run,
			// exec(2) replaces the process, so signals and exit status are
			// the tool's own; the only errors that come back are ours.
			signals: false,
			exit: func(err error) error {
				var ee *ExitError
				if err == nil {
					return nil
				}
				if errors.As(err, &ee) {
					return ee
				}
				return &ExitError{Code: 1, Msg: err.Error()}
			},
		})
	}
}
