# External subcommand wrappers

Anything named `pitf-<name>` on `PATH` becomes `pitf <name>`. The Python tools
(bench, qualeval, forge, meta) live here as thin shims until they are either
ported or deliberately left as Python forever. Filed as the Forge task
"Wrap the Python tools as pitf-bench; pitf-qual; pitf-forge; pitf-meta".

`just install-wrappers` copies every `pitf-*` in this directory to `~/.local/bin`.
