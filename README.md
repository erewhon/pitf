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
pitf bench sweep …  # pp/tg throughput sweep (built in, Go)
pitf bench-py …     # llm-router-bench     (external: legacy multi-target compare)
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

## Benchmarks

`pitf bench sweep` measures prompt-processing and token-generation
throughput for model aliases behind the configured router, using the
streaming chat API. It runs three legs per model (pp512, pp2048, tg128 by
default), several runs each, and reports the median. Output is a table on
stdout and, with `--json`, one JSON object per row whose keys are exactly the
Forge Model Performance Matrix columns (plus a `measure` object with every
sample), so a file saved at work replays into the matrix from home.

```
pitf bench sweep --model qwen38-27b --model glm-fast
pitf --profile work bench sweep --all --match 'qwen*' --json rows.jsonl
pitf bench sweep --model ling3 --registry ~/code/smithy/llm-router/models.yaml
pitf bench show rows.jsonl
```

How the numbers are taken, and what to trust:

- **llama-server seats** report `timings` on the final chunk; pp and tg come
  from those (prompt_n/prompt_ms, predicted_n/predicted_ms) and exclude the
  network and the router hop. Rows say `(server timings)` per leg.
- **Everything else** (vLLM, cloud) is measured at the client: pp is
  prompt_tokens over time-to-first-token, tg is completion_tokens over the
  window from first token to the end. Rows say `(client)`.
- Every prompt starts with a random nonce so no prefix cache can shortcut
  the prefill; the chat-template header llama-server caches anyway is
  tolerated and excluded from the rate.
- A seat behind a proxy that streams only after it has the whole answer
  (the router's tool proxy does this) shows up as a "buffered stream" error
  on client-measured legs, because those numbers would be meaningless.
  Server timings are unaffected.
- The generation leg asks for an essay and counts a run only if the model
  reached at least three quarters of the cut-off, so a two-token "OK" never
  becomes a decode figure.

This is a streaming probe, not llama-bench; the Flags column says so, and
the matrix's llama-bench rows are not directly comparable. `--registry`
fills HF Repo, Quant, Host, Engine and Context from models.yaml when you
have it; otherwise those stay empty for the operator.

## Jumping between tools

Two keys are shared across the tools, and pitf builds the URLs for them:

- **session id** (Claude Code's session UUID, prefixes accepted). tokenator
  keys its sessions on it. agent-monitor learns it from the hooks Claude Code
  runs (`agent-monitor hooks install` prints hooks that post `session_id`)
  and reports it in `/api/agents`.
- **model alias** (a router alias). The router dashboard's catalog tab
  deep-links on it.

```
pitf session                 # agents agent-monitor knows, with session ids
pitf session 0d3e4b2a        # tokenator profile + transcript, matching monitor agent
pitf session 0d3e4b2a --open
pitf model glm-fast          # dashboard catalog page + what /v1/models says
```

Targets come from `[tools]` in the config (`monitor_url`, `tokens_url`,
`dashboard_url`; profile-overridable; `PITF_*_URL` env wins). monitor and
tokens default to their loopback ports; the dashboard has no default. An
unconfigured or unreachable target is reported on its line, never fatal.
The router's request log has no session identity, so there is no session
jump into the router; tokenator groups usage by the harness's model name,
so there is no alias jump into tokenator. In the UIs themselves,
`agent-monitor --tokens-url` and `tokenator serve --monitor-url` add the
reciprocal links.

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
