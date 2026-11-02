.PHONY: install dev clean

BINDIR ?= $(HOME)/.local/bin
DEV_HOME ?= $(HOME)/.dashydev

# Build dashy into BINDIR (default ~/.local/bin) so it can be run from anywhere.
install:
	mkdir -p $(BINDIR)
	go build -o $(BINDIR)/dashy .
	@echo "installed $(BINDIR)/dashy"

# Run from source with an isolated dev home (dashy.toml, sessions).
dev:
	DASHY_HOME=$(DEV_HOME) go run . $(ARGS)

# Remove the dev home.
clean:
	rm -rf $(DEV_HOME)
