package services

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

// HTTPProbe is the default status Probe: any HTTP answer means the service
// is up.
func HTTPProbe(ctx context.Context, target string) error {
	ctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// CheckLoopback refuses listen addresses other than loopback for the
// laptop router dashboard: it has no auth of its own, and it proxies
// tokenator and agent-monitor, which have none either.
func CheckLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("listen address %q: %w", addr, err)
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("listen address %q is not loopback; the router dashboard has no auth on a laptop and only listens on 127.0.0.1, ::1 or localhost", addr)
}
