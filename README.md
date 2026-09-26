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
pitf qual …         # llm-router-qual      (Python, run through uv in its checkout)
pitf forge …        # forge                (Python, run through uv in its checkout)
pitf meta …         # meta                 (Python, run through uv in its checkout)
pitf doctor         # what is missing, and where to get it
```

Three ways a subcommand exists:

- **Built in.** Go tools are imported and mounted. Each tool exposes a
  small public `cli` package with `Run(ctx, args) error`; pitf calls that
  and nothing else. Everything after the mount's name is the tool's own
  argv, parsed by the tool's own flags, so `pitf monitor --help` is
  agent-monitor's help. Global flags go before the name. Help exits 0 under
  pitf even for the router binaries, whose standalone convention is 2; every
  other exit status and message is the tool's. Ctrl-C keeps each tool's
  standalone meaning (agent-monitor cancels a context; tokenator's `serve`
  and the router daemons handle signals themselves).
- **Python, through uv.** `pitf qual`, `pitf forge` and `pitf meta` stay
  Python. pitf finds the tool's checkout under `[tools].smithy_dir`
  (default `~/code/smithy`; `PITF_SMITHY_DIR` overrides), applies the
  resolved profile environment, and `exec`s
  `uv run -q --project <checkout> <console-script> …`. `uv run` syncs the
  project's `.venv` on first use, so a fresh clone needs no install step.
  A missing checkout or a missing `uv` exits 127 with the clone URL or the
  install one-liner; `pitf doctor` reports the same for all three at once.
- **External, git-style.** Any executable named `pitf-<name>` on `PATH`
  answers to `pitf <name>`. pitf `exec`s it with the remaining arguments,
  the environment, and the terminal, so its exit status is its own. Built-ins
  always win over an external of the same name (`pitf doctor` flags shims
  that a built-in shadows). `pitf help` lists the externals that would run.

The old `pitf-qual` / `pitf-forge` / `pitf-meta` shell shims and
`pitf bench-py` are retired: the shims are now built in as above, and the
legacy `llm-router-bench` comparison runs from the llm-router checkout with
`uv run llm-router-bench` when it is still wanted.

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

`api_key_cmd` is any shell command that prints the secret, so it can be
whatever the machine has. `ho secret get llm-router/api-key` at home; on a
Mac `security find-generic-password -s llm-router -w` (after
`security add-generic-password -s llm-router -a $USER -w`); with 1Password
`op read "op://Private/llm-router/credential"`; with `pass`,
`pass show llm-router/api-key`; or simply `cat ~/.config/pitf/router-key`
on a file you `chmod 600`. A literal `api_key = "…"` in the config also
works (keep the file 600, which `pitf config init` does). Each profile can
use a different source, so home and work never share a command.

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
pitf bench import rows.jsonl --dry-run   # then without --dry-run, from home
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

`pitf bench import <rows.jsonl>` appends those rows to the matrix through
the Nous daemon named in `[nous]` (`url` plus `api_key` or `api_key_cmd`;
`NOUS_DAEMON_URL` / `NOUS_API_KEY` also work). It drops the `measure`
object, skips rows that failed every leg unless `--include-failed`, and
`--dry-run` prints what would be posted. Forge is usually only reachable
from home, so the workflow is: sweep at work with `--json`, import at home.

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
pitf session 0d3e4b2a        # tokenator profile + transcript, monitor agent, router requests
pitf session 0d3e4b2a --open
pitf model glm-fast          # dashboard catalog, what /v1/models says, tokenator /model page
```

Targets come from `[tools]` in the config (`monitor_url`, `tokens_url`,
`dashboard_url`; profile-overridable; `PITF_*_URL` env wins). monitor and
tokens default to their loopback ports; the dashboard has no default. An
unconfigured or unreachable target is reported on its line, never fatal.
The same section is exported to every subcommand as `AGENT_MONITOR_TOKENS_URL`
and `TOKENATOR_MONITOR_URL` (plus `PITF_MONITOR_URL`, `PITF_TOKENS_URL`,
`PITF_DASHBOARD_URL`), so `pitf monitor` and `pitf tokens serve` cross-link
with no flags.
The router logs the session id each harness sends (Claude Code and opencode
do; Pi sends none), so the session jump includes the dashboard's Requests
tab; the model jump includes tokenator's `/model/<alias>` page, which matches
the alias the caller sent the router. Both need llm-router-go and tokenator
builds from 2026-09-23 or later. In the UIs themselves,
`agent-monitor --tokens-url` and `tokenator serve --monitor-url` add the
reciprocal links.

### One page: the router dashboard

The router dashboard is the web UI for the whole stack. It reverse-proxies
tokenator at `/tokens/` and agent-monitor at `/monitor/` (llm-router-go
`--dashboard-tokens-url` / `--dashboard-monitor-url`), so its **Tokens** and
**Agents** tabs work behind the front door's SSO at home and on a laptop
router alike, with one login, one theme and one deep-link grammar
(`#requests?session=…`, `#tokens?session=…`, `#tokens?model=…`,
`#catalog?model=…`). `pitf router serve --dashboard` wires both proxies by
itself: pitf exports `PITF_TOKENS_URL` and `PITF_MONITOR_URL` and the router
falls back to them. `pitf session` / `pitf model` print the dashboard links
beside the direct tokenator ones; `pitf doctor` reports whether each proxy
is wired.

`pitf dashboard` (a loopback page that framed the three tools) is retired;
the command prints the router dashboard's address for one release and
`pitf services install` removes the old agent.

## The laptop stack: `pitf services` and `pitf up` (macOS)

On a laptop that runs its own router, two commands bring up everything:

```
pitf --profile work services install   # once; re-run after changing flags
pitf up                                 # each working session
```

`services install` writes per-user launchd agents
(`~/Library/LaunchAgents/org.erewhon.pitf.*.plist`) that start at login and
restart if they exit:

| agent | runs | where |
|---|---|---|
| router | `pitf router serve --dashboard -models-yaml … -addr 127.0.0.1:4010` | API :4010, dashboard :4011 |
| tokens | `pitf tokens serve` | :8990 |
| ingest | `pitf tokens ingest`, every 5 minutes | (tokenator's UI only shows ingested data) |

Each agent runs pitf itself, so it resolves the same config, key command
and cross-link environment as an interactive run; an explicit `--profile`
(or `PITF_PROFILE`) is baked in, otherwise they follow `default_profile`.
Everything listens on loopback: a laptop router usually runs without
`--api-keys`. `--models-yaml` defaults to `~/.config/llm-router/models.yaml`;
`--router-arg` / `--ingest-arg` (repeatable) add flags, `--dry-run` prints the
plists. Logs are in `~/Library/Logs/pitf/`.

Settings live in the config so a bare `services install` always rebuilds the
same agents; flags override its scalars and append to its lists:

```toml
[profiles.work.services]
models_yaml = "~/.config/llm-router/models.yaml"
router_env_files = ["~/.config/llm-router/router.env"]
router_args = ["-log-format=text"]
# router_addr = "127.0.0.1:4010"   dashboard_addr = "127.0.0.1:4011"
# ingest_every = "5m"              ingest_args = ["-regime=metered"]
```

(`[services]` at the top level sets defaults; a profile's list replaces the
default list.) `pitf config show` prints the merged settings, with values
of key/secret/token/dsn flags masked.

The router agent serves `/.well-known/opencode` by default, so OpenCode
pointed at it (`opencode auth login http://127.0.0.1:4010`) finds its
provider: provider id `llm`, base URL `http://127.0.0.1:4010/v1` (the
IPv4 loopback: the agent does not listen on `::1`, where `localhost` may
resolve). Override with `router_args = ["-wellknown-provider-id=work"]`; an
empty id turns it off.

Upstream keys (the env vars models.yaml's `api_key:` names) go in an env
file: `--router-env-file PATH` (repeatable), defaulting to
`~/.config/llm-router/router.env` when it exists. Only the router agent gets
it, as `pitf --env-file PATH router serve …`, so the keys stay in that file
(read at every start; `chmod 600` it), never in a plist. Rotate a key by
editing the file and running `pitf services restart router`. The file is
dotenv/systemd style: `KEY=VALUE` lines, `#` comments, optional `export`,
quoted values. The same `--env-file` works on any pitf command, e.g.
`pitf --env-file ~/.config/llm-router/router.env router serve …` in a
terminal.

An existing launchd agent for the standalone `llm-router` is **adopted**: its
flags (except `-addr` and the dashboard flags, which pitf now owns) and its
`EnvironmentVariables` (upstream keys such as `AWS_BEARER_TOKEN_BEDROCK`)
move into the pitf router agent, it is stopped, and its plist is moved to
`~/Library/Application Support/pitf/replaced/`, never deleted. A legacy agent
that runs the router through a shell script cannot be parsed; install stops
and says so (move its flags to `--router-arg` and its secrets to the profile's
`[env]`, then pass `--replace-legacy`).

`pitf up` loads any agent that is stopped, waits for the router's `/health`,
prints the status, opens the router dashboard in the browser (`--no-open` to
skip), then runs agent-monitor in the terminal (a TUI, so not an
agent; arguments after `--` go to it). `--no-monitor` stops after the status.

```
pitf services status            # launchd state + whether each URL answers
pitf services restart router    # after editing models.yaml
pitf services stop | start      # stop everything / bring it back
pitf services uninstall         # remove the agents (logs and a replaced plist stay)
```

## Installing

```
brew install erewhon/tap/pitf      # macOS and Linux
```

Or build from source (below). Either way, `pitf config init` first, then
`pitf doctor`: it checks the config, the router and its key, the tool UIs
the jumps point at, `uv` and the Python checkouts, `tmux`, and stale
`pitf-*` shims on `PATH`, with a fix-it hint per line. Exit 1 only on a
FAIL (router unreachable, key rejected); missing optional pieces are
warnings.

What works from the binary alone, and what needs more:

| command | needs |
|---|---|
| `pitf config`, `pitf session`, `pitf model` | nothing (tools reachable by URL) |
| `pitf bench sweep`, `pitf bench show` | a router URL and key in the config |
| `pitf monitor`, `pitf tokens …`, `pitf router …` | nothing: compiled in (`monitor` needs tmux, `router` needs a models.yaml) |
| `pitf qual`, `pitf forge`, `pitf meta` | `uv`, and the Python checkouts under `~/code/smithy` (`[tools].smithy_dir` / `PITF_SMITHY_DIR`) — `pitf doctor` says which are missing |
| `pitf doctor` | nothing; it is how you find out what the rest needs |

## Building

```
just build      # ./bin/pitf
just test
just install    # ~/.local/bin/pitf
```

The Go tools are sibling checkouts under `~/code/smithy`. A `go.work` there
lists `pitf`, `agent-monitor`, `tokenator`, and `llm-router-go`, so day-to-day
builds use the local trees. `go.mod` pins each of the three to a commit on
GitHub (pseudo-versions), which is what CI and GoReleaser build from with no
workspace (`GOWORK=off go build ./...` reproduces that locally). After
landing a change in one of the tools, push it and bump the pin:
`GOWORK=off go get github.com/erewhon/<tool>@<commit> && GOWORK=off go mod tidy`. Always tidy with `GOWORK=off`; inside the workspace it would try to resolve the siblings differently.

Releases: tag `vX.Y.Z` on GitHub and the release workflow builds
darwin/linux archives and updates `Formula/pitf.rb` in `erewhon/homebrew-tap`
(needs the `HOMEBREW_TAP_GITHUB_TOKEN` repo secret).

## Pi

`contrib/pi/router-session.ts` is a Pi extension that sends the Pi session
id to the router as `X-Session-Id` on every provider request, so the
router's request log, tokenator's gateway pairing and `pitf session` can
attribute Pi traffic. Install by symlinking it into
`~/.pi/agent/extensions/`; nothing else is needed (the router already reads
that header). tokenator does not ingest Pi sessions yet, so the rows carry
an id but pair with nothing.

## Layout

```
cmd/pitf/          main: signal context, version stamp, exit codes
internal/cli/      root command, mounts (Go and Python-via-uv), external dispatch, doctor
internal/services/  pitf services / up: launchd agents for the laptop stack
contrib/pi/        Pi extension: X-Session-Id on router requests
```
