//go:build tools

// Package deps pins the approved third-party packages in go.mod so that
// `go mod tidy` keeps them available to all packages (docs/ARCHITECTURE.md 4).
// Adding a module here needs an explicit decision.
package deps

import (
	_ "github.com/miekg/dns"
	_ "golang.org/x/crypto/argon2"
	_ "golang.org/x/crypto/chacha20poly1305"
	_ "golang.org/x/net/idna"
	_ "golang.org/x/net/publicsuffix"
	_ "golang.org/x/sync/errgroup"
	_ "golang.org/x/sync/singleflight"
	_ "golang.org/x/sys/unix"
	_ "golang.org/x/time/rate"
	_ "modernc.org/sqlite"
)
