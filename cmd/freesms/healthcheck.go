package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"time"
)

// healthcheck asks the local server whether it is healthy and reports the
// answer as an exit code.
//
// It exists because the runtime image contains the binary and nothing else --
// no shell, no curl, no wget -- so a container health check has to be the
// program itself. The alternative is adding a shell to the image, which trades
// a real reduction in attack surface for the convenience of one line of YAML.
func healthcheck() error {
	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("HTTP_ADDR %q is not host:port: %w", addr, err)
	}
	// A server bound to every interface is reached over the loopback, not over
	// the empty string.
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}

	client := &http.Client{Timeout: 4 * time.Second}
	url := "http://" + net.JoinHostPort(host, port) + "/healthz"

	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s returned %s", url, resp.Status)
	}
	return nil
}
