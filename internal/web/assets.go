package web

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
)

// versions holds a short hash of each static file's contents, computed once.
//
// The files are embedded, so they cannot change while the process runs, and
// the hash changes exactly when a file does. A deploy that touches only Go
// code leaves every URL alone, and every phone keeps its cache.
var versions = func() map[string]string {
	out := map[string]string{}
	entries, err := fs.ReadDir(Static, "static")
	if err != nil {
		panic("static files: " + err.Error())
	}
	for _, e := range entries {
		b, err := Static.ReadFile("static/" + e.Name())
		if err != nil {
			panic("static file " + e.Name() + ": " + err.Error())
		}
		sum := sha256.Sum256(b)
		out[e.Name()] = hex.EncodeToString(sum[:])[:12]
	}
	return out
}()

// Version is the content version of a static file, or empty for none.
func Version(name string) string { return versions[name] }

// Asset is the URL a page uses for a static file.
//
// The version is in the URL so that the URL changes when the file does. A
// fixed /static/keep.js cached for an hour meant a phone ran the old script
// against a new server for up to that hour after an update -- and keep.js is
// the code that holds unsent work.
func Asset(name string) string {
	if v := versions[name]; v != "" {
		return "/static/" + name + "?v=" + v
	}
	return "/static/" + name
}
