# Agent rules

- Keep the source code under 7,000 lines (Go files without tests).
  Check before committing: `find . -name '*.go' ! -name '*_test.go' | xargs cat | wc -l`
- Keep the project language English (UI, docs, comments).
- Run `gofmt -l .`, `go vet ./...` and `go test ./...` before committing; CI runs the same checks.
