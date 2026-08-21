//go:build tools

package main

// This file never compiles into the binary: the tools build tag is never set.
// Its sole purpose is the blank import below, which puts the ent codegen tool
// into the module's import graph so that go mod tidy and go generate agree on
// whether it is needed. Without it the two commands fight over go.sum: tidy
// prunes the tool's hashes as unreachable, the next generate re-adds them to
// verify its download, and the file churns on every schema touch.
import (
	_ "entgo.io/ent/cmd/ent"
)
