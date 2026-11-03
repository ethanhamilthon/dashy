.PHONY: install dev test clean

BINDIR ?= $(HOME)/.local/bin
DEV_HOME ?= $(HOME)/.dashydev

# Build dashy into BINDIR (default ~/.local/bin) so it can be run from anywhere.
install:
	mkdir -p $(BINDIR)
	go build -o $(BINDIR)/dashy .
	@echo "installed $(BINDIR)/dashy"

# Run from source with an isolated dev home.
dev:
	DASHY_HOME=$(DEV_HOME) go run . $(ARGS)

# The checks CI runs: formatting, vet and the tests with the race detector.
test:
	@unformatted=$$(gofmt -l .); if [ -n "$$unformatted" ]; then echo "gofmt needs to format:"; echo "$$unformatted"; exit 1; fi
	go vet ./...
	go test -race ./...

# Remove the dev home.
clean:
	rm -rf $(DEV_HOME)
