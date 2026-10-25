package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"strings"

	"fridge/internal/auth"
	"fridge/internal/objstore"
)

const endpointAllowlistKey = "s3_endpoint_allowlist"

// blockedEndpoint is both an invalid input and objstore.ErrEndpointBlocked, with the vague message.
type blockedEndpoint struct{}

func (blockedEndpoint) Error() string { return objstore.ErrEndpointBlocked.Error() }
func (blockedEndpoint) Is(target error) bool {
	return target == ErrInvalid || target == objstore.ErrEndpointBlocked
}

var hostRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$`)

// EndpointAllowlist is the hosts exempt from the SSRF guard (admin setting).
func (a *App) EndpointAllowlist(ctx context.Context) ([]string, error) {
	v, ok, err := a.Store.GetSetting(ctx, endpointAllowlistKey)
	if err != nil || !ok {
		return []string{}, err
	}
	var out []string
	if err := json.Unmarshal([]byte(v), &out); err != nil {
		return nil, fmt.Errorf("corrupt %s setting: %w", endpointAllowlistKey, err)
	}
	return out, nil
}

// SetEndpointAllowlist replaces the list; every entry is a bare host name or IP (no scheme, no port).
func (a *App) SetEndpointAllowlist(ctx context.Context, hosts []string) error {
	clean := make([]string, 0, len(hosts))
	seen := map[string]bool{}
	for _, h := range hosts {
		h = strings.ToLower(strings.TrimSpace(h))
		_, ipErr := netip.ParseAddr(h)
		if (ipErr != nil && !hostRe.MatchString(h)) || strings.Contains(h, "..") {
			return invalid("bad host %q", h)
		}
		if !seen[h] {
			seen[h] = true
			clean = append(clean, h)
		}
	}
	b, _ := json.Marshal(clean)
	return a.Store.SetSetting(ctx, endpointAllowlistKey, string(b))
}

// endpointGuard is what keeps a non-admin's destination away from Fridge's own networks.
func (a *App) endpointGuard(ctx context.Context) (*objstore.Guard, error) {
	subnets, err := a.RT.FridgeSubnets(ctx)
	if err != nil {
		return nil, err
	}
	allow, err := a.EndpointAllowlist(ctx)
	if err != nil {
		return nil, err
	}
	return &objstore.Guard{Blocked: subnets, Allow: allow}, nil
}

// CheckEndpoint refuses, for non-admins, an endpoint that points into Fridge's infrastructure.
// The error never says why.
func (a *App) CheckEndpoint(ctx context.Context, p auth.Principal, endpoint string) error {
	if p.IsAdmin() {
		return nil
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Hostname() == "" {
		return blockedEndpoint{}
	}
	g, err := a.endpointGuard(ctx)
	if err != nil {
		return err
	}
	if g.CheckHost(ctx, u.Hostname()) != nil {
		return blockedEndpoint{}
	}
	return nil
}
