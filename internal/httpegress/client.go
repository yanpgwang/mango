// Package httpegress provides HTTP clients that enforce the public-network
// boundary for tenant-configured outbound endpoints.
package httpegress

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"time"

	"github.com/yanpgwang/mango/internal/domain"
)

var ErrNonPublicAddress = errors.New("target resolved to a non-public address")

func NewPublicClient(timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Tenant-configured endpoints must not inherit a process-wide proxy that
	// can bypass target-IP validation.
	transport.Proxy = nil
	transport.DialContext = DialPublic
	return &http.Client{Timeout: timeout, Transport: transport}
}

func DialPublic(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("public egress: parse address: %w", err)
	}
	var addresses []net.IP
	if literal := net.ParseIP(host); literal != nil {
		addresses = []net.IP{literal}
	} else {
		resolved, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
		if err != nil {
			return nil, fmt.Errorf("public egress: resolve %s: %w", host, err)
		}
		addresses = resolved
	}

	dialer := &net.Dialer{Timeout: 15 * time.Second}
	var lastErr error
	allowed := false
	for _, candidate := range addresses {
		parsed, ok := netipAddr(candidate)
		if !ok || !domain.MCPAddressAllowed(parsed) {
			continue
		}
		allowed = true
		connection, err := dialer.DialContext(
			ctx,
			network,
			net.JoinHostPort(parsed.String(), port),
		)
		if err == nil {
			return connection, nil
		}
		lastErr = err
	}
	if !allowed {
		return nil, fmt.Errorf("public egress: %s: %w", host, ErrNonPublicAddress)
	}
	return nil, fmt.Errorf("public egress: dial %s: %w", host, lastErr)
}

func netipAddr(value net.IP) (address netip.Addr, ok bool) {
	address, ok = netip.AddrFromSlice(value)
	if ok {
		address = address.Unmap()
	}
	return address, ok
}
