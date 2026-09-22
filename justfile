set shell := ["bash", "-c"]

version := `jj log -r @ --no-graph -T 'change_id.short()' 2>/dev/null || echo dev`

# default: list recipes
default:
    @just --list

# build the binary into ./bin/
build:
    go build -ldflags "-X main.version={{version}}" -o bin/pitf ./cmd/pitf

# run tests (the dispatch tests build the binary themselves)
test:
    go test ./...

# vet + gofmt check
lint:
    go vet ./...
    test -z "$(gofmt -l .)" || (gofmt -l . && exit 1)

# build + test + install to ~/.local/bin
install: build test
    install -m 755 bin/pitf ~/.local/bin/pitf

# install the Python-tool wrappers (pitf-bench, pitf-qual, pitf-forge, pitf-meta) to ~/.local/bin
install-wrappers:
    for f in contrib/wrappers/pitf-*; do install -m 755 "$f" ~/.local/bin/; done

# smoke-test the installed wrappers: each must answer `pitf <name> --help`
check-wrappers:
    for n in bench-py qual forge meta; do pitf "$n" --help >/dev/null || exit 1; echo "pitf $n ok"; done
