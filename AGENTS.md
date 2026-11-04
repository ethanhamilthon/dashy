# Dashy - minimalistic AI coding agent

- Keep the source code under 7,000 lines (Go files without tests).
  Check before committing: `find . -name '*.go' ! -name '*_test.go' | xargs cat | wc -l`
- Keep the project language English (UI, docs, comments).
- Run `make test` (gofmt, vet, tests with -race) before committing; CI runs the same checks.
- Do git commit after any new feature, bug fix
