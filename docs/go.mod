// This file keeps the docs site out of tug's Go module. Some npm packages
// ship .go files, and without a go.mod here `go build ./...`, `go vet ./...`
// and golangci-lint would try to compile them from docs/node_modules.
module github.com/paingha/tugcli/docs

go 1.26.0
