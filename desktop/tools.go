//go:build tools

// Keep the MyGo CLI (run via `go run` in checks and CI) in the module graph,
// so `go mod tidy` retains its dependencies in go.sum.
package tools

import _ "github.com/egoist/mygo/cmd/mygo"
