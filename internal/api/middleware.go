package api

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"fridge/internal/auth"
)

const cookieName = "fridge_session"

type ctxKey struct{}

func principal(r *http.Request) (auth.Principal, bool) {
	p, ok := r.Context().Value(ctxKey{}).(auth.Principal)
	return p, ok
}

// headers sets the security headers on every response.
func headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; frame-ancestors 'none'")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

// requireLogin resolves the principal (Bearer session, else cookie) and enforces CSRF
// for cookie-authenticated unsafe methods. Unknown routes fall through so they 404.
func (s *server) requireLogin(next *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := next.Handler(r); pattern == "" {
			next.ServeHTTP(w, r)
			return
		}
		sid, viaCookie := "", false
		if b, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			sid = b
		} else if c, err := r.Cookie(cookieName); err == nil {
			sid, viaCookie = c.Value, true
		}
		var p auth.Principal
		var err error = auth.ErrUnauthenticated
		if sid != "" {
			p, err = s.au.Authenticate(r.Context(), sid)
		}
		switch {
		case err == nil:
		case errors.Is(err, auth.ErrUnauthenticated):
			if need, nerr := s.au.SetupNeeded(r.Context()); nerr != nil {
				fail(w, nerr)
			} else if need {
				writeJSON(w, 401, map[string]string{"error": "setup required"})
			} else {
				writeJSON(w, 401, map[string]string{"error": "unauthorized"})
			}
			return
		default:
			fail(w, err)
			return
		}
		if viaCookie && !safeMethod(r.Method) && !sameOrigin(r) {
			writeJSON(w, 403, map[string]string{"error": "cross-origin request refused"})
			return
		}
		s.audited(next).ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, p)))
	})
}

func safeMethod(m string) bool { return m == "GET" || m == "HEAD" || m == "OPTIONS" }

func sameOrigin(r *http.Request) bool {
	if o := r.Header.Get("Origin"); o != "" {
		return o == "https://"+r.Host
	}
	return r.Header.Get("Sec-Fetch-Site") == "same-origin"
}

// clientIP is the peer address; when the peer is a trusted proxy, the last X-Forwarded-For element.
func clientIP(r *http.Request, trusted func(net.IP) bool) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer := net.ParseIP(host)
	if peer == nil {
		return host
	}
	if trusted == nil {
		trusted = TrustCIDRs(nil)
	}
	if !trusted(peer) {
		return host
	}
	xff := r.Header.Values("X-Forwarded-For")
	if len(xff) == 0 {
		return host
	}
	parts := strings.Split(xff[len(xff)-1], ",")
	last := strings.TrimSpace(parts[len(parts)-1])
	if net.ParseIP(last) == nil {
		return host
	}
	return last
}

// TrustCIDRs trusts peers inside any prefix, plus loopback.
func TrustCIDRs(prefixes []netip.Prefix) func(net.IP) bool {
	return func(ip net.IP) bool {
		a, ok := netip.AddrFromSlice(ip)
		if !ok {
			return false
		}
		a = a.Unmap()
		if a.IsLoopback() {
			return true
		}
		for _, p := range prefixes {
			if p.Contains(a) {
				return true
			}
		}
		return false
	}
}

// ParseCIDRs parses a comma-separated CIDR list; empty gives nil.
func ParseCIDRs(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f == "" {
			continue
		}
		p, err := netip.ParsePrefix(f)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}
