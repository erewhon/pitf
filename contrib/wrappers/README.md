# External subcommand wrappers

Anything named `pitf-<name>` on `PATH` becomes `pitf <name>`. The Python tools
live here as shims that run the tool's own console script inside its uv
project, from whatever directory you are in:

| wrapper      | runs               | project (under `$PITF_SMITHY_DIR`, default `~/code/smithy`) |
|--------------|--------------------|-------------------------------------------------------------|
| `pitf-bench-py` | `llm-router-bench` (legacy multi-target compare; `pitf bench` is the Go sweep) | `llm-router`                                                |
| `pitf-qual`  | `llm-router-qual`  | `llm-router`                                                |
| `pitf-forge` | `forge`            | `forge`                                                     |
| `pitf-meta`  | `meta`             | `meta`                                                      |

`uv run --project` syncs the project's `.venv` on first use, so a fresh
checkout works without a manual install step. Set `PITF_SMITHY_DIR` where the
checkouts live somewhere other than `~/code/smithy` (e.g. the work laptop).

Install with `just install-wrappers`; smoke-test with `just check-wrappers`.

## Adding one

Copy any of the four, change the three names in the header comment, `proj=`,
and the `exec` line, `chmod 755`, and re-run `just install-wrappers`. If the
tool is not a uv project, the shim can be anything executable; the only
contract is the `pitf-<name>` filename and passing `"$@"` through.
