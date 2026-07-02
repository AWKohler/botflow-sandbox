package egress

import (
	"errors"
	"net/netip"
	"strings"
)

type Rule struct {
	Domain       string   `json:"domain"`
	Methods      []string `json:"methods,omitempty"`
	PathPrefixes []string `json:"pathPrefixes,omitempty"`
}

type Policy struct {
	Rules []Rule `json:"rules"`
}

type SessionPolicy struct {
	SessionID string `json:"sessionId"`
	GuestIP   string `json:"guestIp"`
	Policy    Policy `json:"policy"`
}

func (p Policy) Validate() error {
	if len(p.Rules) > 128 {
		return errors.New("too many egress rules")
	}
	for _, rule := range p.Rules {
		if !validPattern(rule.Domain) {
			return errors.New("invalid domain pattern: " + rule.Domain)
		}
		for _, method := range rule.Methods {
			if method == "" || strings.ToUpper(method) != method {
				return errors.New("methods must be non-empty uppercase values")
			}
		}
		for _, prefix := range rule.PathPrefixes {
			if !strings.HasPrefix(prefix, "/") {
				return errors.New("path prefixes must begin with /")
			}
		}
	}
	return nil
}

func (p Policy) Allows(host, method, path string) bool {
	host = normalizeHost(host)
	for _, rule := range p.Rules {
		if !domainMatches(rule.Domain, host) {
			continue
		}
		if len(rule.Methods) > 0 && !containsFold(rule.Methods, method) {
			continue
		}
		if len(rule.PathPrefixes) > 0 {
			matched := false
			for _, prefix := range rule.PathPrefixes {
				if strings.HasPrefix(path, prefix) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		return true
	}
	return false
}

func Intersect(requested, ceiling Policy) Policy {
	if len(requested.Rules) == 0 {
		return Policy{}
	}
	result := Policy{Rules: make([]Rule, 0, len(requested.Rules))}
	for _, req := range requested.Rules {
		for _, cap := range ceiling.Rules {
			if patternContainedBy(req.Domain, cap.Domain) {
				combined := req
				combined.Methods = intersectStrings(req.Methods, cap.Methods)
				combined.PathPrefixes = intersectPrefixes(req.PathPrefixes, cap.PathPrefixes)
				if len(req.Methods) > 0 && len(cap.Methods) > 0 && len(combined.Methods) == 0 {
					continue
				}
				if len(req.PathPrefixes) > 0 && len(cap.PathPrefixes) > 0 && len(combined.PathPrefixes) == 0 {
					continue
				}
				result.Rules = append(result.Rules, combined)
				break
			}
		}
	}
	return result
}

func intersectStrings(requested, ceiling []string) []string {
	if len(ceiling) == 0 {
		return append([]string(nil), requested...)
	}
	if len(requested) == 0 {
		return append([]string(nil), ceiling...)
	}
	var result []string
	for _, value := range requested {
		if containsFold(ceiling, value) {
			result = append(result, value)
		}
	}
	return result
}

func intersectPrefixes(requested, ceiling []string) []string {
	if len(ceiling) == 0 {
		return append([]string(nil), requested...)
	}
	if len(requested) == 0 {
		return append([]string(nil), ceiling...)
	}
	var result []string
	for _, req := range requested {
		for _, cap := range ceiling {
			if strings.HasPrefix(req, cap) {
				result = append(result, req)
			} else if strings.HasPrefix(cap, req) {
				result = append(result, cap)
			}
		}
	}
	return result
}

func validPattern(pattern string) bool {
	pattern = strings.ToLower(strings.TrimSuffix(pattern, "."))
	if pattern == "" || pattern == "*" || strings.ContainsAny(pattern, "/:@[] ") {
		return false
	}
	if strings.HasPrefix(pattern, "*.") {
		pattern = strings.TrimPrefix(pattern, "*.")
	}
	if strings.Contains(pattern, "*") || !strings.Contains(pattern, ".") {
		return false
	}
	for _, label := range strings.Split(pattern, ".") {
		if label == "" || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		for _, r := range label {
			if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-') {
				return false
			}
		}
	}
	return true
}

func domainMatches(pattern, host string) bool {
	pattern = normalizeHost(pattern)
	host = normalizeHost(host)
	if strings.HasPrefix(pattern, "*.") {
		base := strings.TrimPrefix(pattern, "*.")
		return host != base && strings.HasSuffix(host, "."+base)
	}
	return host == pattern
}

func patternContainedBy(requested, ceiling string) bool {
	requested = normalizeHost(requested)
	ceiling = normalizeHost(ceiling)
	if requested == ceiling {
		return true
	}
	if strings.HasPrefix(ceiling, "*.") {
		base := strings.TrimPrefix(ceiling, "*.")
		if strings.HasPrefix(requested, "*.") {
			requested = strings.TrimPrefix(requested, "*.")
		}
		return requested != base && strings.HasSuffix(requested, "."+base)
	}
	return false
}

func normalizeHost(host string) string {
	host = strings.TrimSpace(strings.ToLower(strings.TrimSuffix(host, ".")))
	if strings.HasPrefix(host, "[") {
		if end := strings.IndexByte(host, ']'); end >= 0 {
			return host[1:end]
		}
	}
	if colon := strings.LastIndexByte(host, ':'); colon > 0 && !strings.Contains(host[:colon], ":") {
		host = host[:colon]
	}
	return host
}

func containsFold(values []string, value string) bool {
	for _, candidate := range values {
		if strings.EqualFold(candidate, value) {
			return true
		}
	}
	return false
}

var deniedPrefixes = mustPrefixes(
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
	"172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.168.0.0/16", "198.18.0.0/15",
	"198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
	"::/128", "::1/128", "::ffff:0:0/96", "64:ff9b::/96", "100::/64", "2001:db8::/32",
	"fc00::/7", "fe80::/10", "ff00::/8",
)

func IsPublicAddress(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || !addr.IsGlobalUnicast() {
		return false
	}
	for _, prefix := range deniedPrefixes {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}

func mustPrefixes(values ...string) []netip.Prefix {
	result := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		result = append(result, netip.MustParsePrefix(value))
	}
	return result
}
