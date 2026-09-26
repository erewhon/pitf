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
