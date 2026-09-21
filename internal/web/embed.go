// Package web holds the templates and static files, embedded in the binary.
package web

import "embed"

// Templates are parsed once at startup.
//
//go:embed templates/*.html
var Templates embed.FS

// Static is served from /static/. htmx is vendored rather than loaded from a
// content delivery network: a workshop's connection is the weak link, and a
// self-hosted system should not stop working because someone else's server
// does.
//
//go:embed static/*
var Static embed.FS
