# pitf

One command over the smithy LLM tools. Built for one operator who uses the
router and agent-monitor every day, at home and at work; if it is useful to
anyone else, that is a bonus, not a design constraint.

The name ties to [pitf.dev](https://pitf.dev).

## What it does

```
pitf monitor …      # agent-monitor        (compiled in)
pitf tokens …       # tokenator            (compiled in)
pitf router serve|node-agent|gpu-exporter|tool-proxy|say …
                    # llm-router-go cmds   (compiled in)
pitf bench …        # llm-router-bench     (external: pitf-bench on PATH)
pitf qual …         # llm-router-qual      (external: pitf-qual on PATH)
pitf forge …        # forge                (external: pitf-forge on PATH)
pitf meta …         # meta                 (external: pitf-meta on PATH)
```

Two ways a subcommand exists:

- **Built in.** Go tools are imported and mounted. Each tool exposes a
  small public `cli` package with `Run(ctx, args) error`; pitf calls that
  and nothing else. Everything after the mount's name is the tool's own
  argv, parsed by the tool's own flags, so `pitf monitor --help` is
  agent-monitor's help. Global flags go before the name. Help exits 0 under
  pitf even for the router binaries, whose standalone convention is 2; every
  other exit status and message is the tool's. Ctrl-C keeps each tool's
  standalone meaning (agent-monitor cancels a context; tokenator's `serve`
  and the router daemons handle signals themselves).
- **External, git-style.** Any executable named `pitf-<name>` on `PATH`
  answers to `pitf <name>`. pitf `exec`s it with the remaining arguments,
  the environment, and the terminal, so its exit status is its own. Built-ins
  always win over an external of the same name. `pitf help` lists whatever
  externals it can see.

The Python tools stay Python behind wrappers in `contrib/wrappers/` for as
long as that is the right answer. Each shim runs the tool's console script
inside its uv project (`uv run --project`), from any directory; set
`PITF_SMITHY_DIR` if the checkouts are not under `~/code/smithy`.
`just install-wrappers` puts them on `PATH`.

## One operator, one config

The friction this removes is operator-level: too many entry points, per-tool
environment variables, no shared idea of a session or a model. One file,
`~/.config/pitf/config.toml` (`$XDG_CONFIG_HOME` honoured, `PITF_CONFIG`
overrides the path), says where the router is and how to authenticate, with
profiles for home and work:

```toml
default_profile = "home"

[router]
url = "https://llm.bcc.sh"
api_key_cmd = "ho secret get llm-router/api-key"   # run lazily, once

[profiles.work.router]
url = "https://router.example.corp"
api_key_cmd = "op read op://work/router/credential"

[profiles.work.env]                # anything else every subcommand should see
CODE_REVIEWER_OPENAI_BASE_URL = "https://router.example.corp/v1"
```

```
pitf config init                 # write a starter file
pitf config show                 # resolved values, secrets redacted
pitf config path
pitf --profile work config env   # export lines, for eval "$(…)"
pitf --profile work bench …      # any subcommand, external or built in
```

Precedence, highest first: `--profile` / `--router-url` flags, then
`PITF_PROFILE` / `PITF_ROUTER_URL` / `PITF_ROUTER_API_KEY`, then a
`ROUTER_API_KEY` you already exported by hand (only when no profile was
chosen explicitly; choosing one means "use that router"), then the
profile, then the file's top-level defaults.

Every subcommand receives the result as environment: `PITF_PROFILE`,
`PITF_ROUTER_URL`, `PITF_ROUTER_API_KEY`, and `ROUTER_API_KEY` (what the
llm-router Python tools read today), plus the `[env]` tables. Tools do not
need to know pitf exists. The key command runs only when something needs a
key and nothing already supplies one. See the Forge project **PITF** for
the task tree.

## Building

```
just build      # ./bin/pitf
just test
just install    # ~/.local/bin/pitf
```

The Go tools are sibling checkouts under `~/code/smithy`. A `go.work` there
lists `pitf`, `agent-monitor`, `tokenator`, and `llm-router-go`, so mounts
build against the local trees without changing any module path. The
`require` lines for those three modules carry a placeholder version: they
resolve through the workspace, and `go mod tidy` would try to fetch them
from the network, so do not run it (`go get` the third-party deps by name
instead). A CI build outside that tree needs published tags or `replace`
directives. Until the `pitf-cli-export` branches in the three repos are
merged, they must be the checked-out branch for pitf to compile.

## Layout

```
cmd/pitf/          main: signal context, version stamp, exit codes
internal/cli/      root command, external lookup and dispatch
contrib/wrappers/  pitf-* shims for the Python tools
```
