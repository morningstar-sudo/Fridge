package objstore

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"syscall"
)

// ErrEndpointBlocked is deliberately vague: it must not say which rule matched.
var ErrEndpointBlocked = errors.New("endpoint không được phép")

// BlockedNames are Fridge's own services, refused by name before any lookup.
var BlockedNames = []string{"fridge", "fridge-meta", "traefik", "fridge-traefik"}

// Guard keeps a user-supplied S3 endpoint away from Fridge's own infrastructure.
type Guard struct {
	Blocked []netip.Prefix // Docker subnets of Fridge (control network and every node network)
	Allow   []string       // hosts an admin has exempted; compared case-insensitively
}

func (g *Guard) allowed(host string) bool {
	for _, h := range g.Allow {
		if strings.EqualFold(h, host) {
			return true
		}
	}
	return false
}

// CheckIP refuses loopback, link-local, unspecified and Fridge's own subnets. Other private ranges pass.
func (g *Guard) CheckIP(ip netip.Addr) error {
	ip = ip.Unmap()
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return ErrEndpointBlocked
	}
	for _, p := range g.Blocked {
		if p.Contains(ip) {
			return ErrEndpointBlocked
		}
	}
	return nil
}

// CheckHost vets a host name or IP literal (no port). A name must resolve only to allowed addresses.
func (g *Guard) CheckHost(ctx context.Context, host string) error {
	host = strings.TrimSuffix(host, ".")
	if g.allowed(host) {
		return nil
	}
	for _, n := range BlockedNames {
		if strings.EqualFold(n, host) {
			return ErrEndpointBlocked
		}
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return g.CheckIP(ip)
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(ips) == 0 {
		return nil // unusable now; if it ever resolves, DialControl vets the address it resolves to
	}
	for _, ip := range ips {
		if err := g.CheckIP(ip); err != nil {
			return err
		}
	}
	return nil
}

// DialControl is a net.Dialer.Control: it vets the address actually being connected to, so a name
// that resolved to something harmless at creation time (DNS rebinding) is caught at connect time.
func (g *Guard) DialControl(network, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return ErrEndpointBlocked
	}
	return g.CheckIP(ap.Addr())
}
