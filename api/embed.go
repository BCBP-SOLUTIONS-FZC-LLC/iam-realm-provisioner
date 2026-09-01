// Package api embeds this service's machine-readable event contract so the
// running binary can serve it (GET /asyncapi, GET /asyncapi.yaml) without
// depending on the source tree being present at runtime — the Dockerfile's
// final stage copies only the compiled binaries into the distroless image.
package api

import _ "embed"

// AsyncAPISpec is the verbatim contents of asyncapi.yaml, embedded at
// compile time.
//
//go:embed asyncapi.yaml
var AsyncAPISpec []byte
