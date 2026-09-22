# pitf

One command over the smithy LLM tools. Built for one operator who uses the
router and agent-monitor every day, at home and at work; if it is useful to
anyone else, that is a bonus, not a design constraint.

The name ties to [pitf.dev](https://pitf.dev).

## What it does

```
pitf monitor …      # agent-monitor        (compiled in, once mounted)
pitf tokens …       # tokenator            (compiled in, once mounted)
pitf router …       # llm-router-go cmds   (compiled in, once mounted)
pitf bench …        # llm-router-bench     (external: pitf-bench on PATH)
pitf qual …         # llm-router-qual      (external: pitf-qual on PATH)
pitf forge …        # forge                (external: pitf-forge on PATH)
pitf meta …         # meta                 (external: pitf-meta on PATH)
```

Two ways a subcommand exists:

- **Built in.** Go tools are imported and mounted as cobra subcommands.
  Each tool exposes a small public `cli` package with
  `Run(ctx, args) error`; pitf calls that and nothing else.
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

## Intent: one operator, one config

The friction this removes is operator-level: too many entry points, per-tool
environment variables, no shared idea of a session or a model. The next
piece is one config file (`~/.config/pitf/config.toml`, with home/work
profiles and secrets via `ho secret`) that every subcommand reads. See the
Forge project **PITF** for the task tree.

## Building

```
just build      # ./bin/pitf
just test
just install    # ~/.local/bin/pitf
```

The Go tools are sibling checkouts under `~/code/smithy`. A `go.work` there
lists `pitf`, `agent-monitor`, `tokenator`, and `llm-router-go`, so mounts
build against the local trees without changing any module path. A CI build
outside that tree needs published tags or `replace` directives.

## Layout

```
cmd/pitf/          main: signal context, version stamp, exit codes
internal/cli/      root command, external lookup and dispatch
contrib/wrappers/  pitf-* shims for the Python tools
```
