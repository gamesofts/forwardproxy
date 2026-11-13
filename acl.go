package forwardproxy

import (
	"errors"
	"net"
	"strings"
)

// defaultDeniedIPRanges protects the proxy from reaching local, special-use,
// and non-routable destinations unless explicitly permitted by an ACL rule.
// User-specified ACL rules retain their existing precedence over these defaults.
var defaultDeniedIPRanges = []string{
	"0.0.0.0/8",        // unspecified / current network
	"10.0.0.0/8",       // private
	"100.64.0.0/10",    // shared address space (CGNAT)
	"127.0.0.0/8",      // loopback
	"169.254.0.0/16",   // link-local; cloud metadata endpoints
	"172.16.0.0/12",    // private
	"192.0.0.0/24",     // IETF protocol assignments
	"192.0.2.0/24",     // documentation
	"192.88.99.0/24",   // deprecated 6to4 relay
	"192.168.0.0/16",   // private
	"198.18.0.0/15",    // benchmarking
	"198.51.100.0/24", // documentation
	"203.0.113.0/24",  // documentation
	"224.0.0.0/4",     // multicast
	"240.0.0.0/4",     // reserved and broadcast
	"::/128",          // unspecified
	"::1/128",         // loopback
	"64:ff9b::/96",    // well-known NAT64 translation prefix
	"100::/64",        // discard-only
	"2001::/32",       // Teredo
	"2001:db8::/32",   // documentation
	"2002::/16",       // 6to4
	"fc00::/7",        // unique local
	"fe80::/10",       // link-local
	"ff00::/8",        // multicast
}

// ACLRule describes an ACL rule.
type ACLRule struct {
	Subjects []string `json:"subjects,omitempty"`
	Allow    bool     `json:"allow,omitempty"`
}

type aclDecision uint8

const (
	aclDecisionAllow = iota
	aclDecisionDeny
	aclDecisionNoMatch
)

type aclRule interface {
	tryMatch(ip net.IP, domain string) aclDecision
}

type aclIPRule struct {
	net   net.IPNet
	allow bool
}

func (a *aclIPRule) tryMatch(ip net.IP, domain string) aclDecision {
	if !a.net.Contains(ip) {
		return aclDecisionNoMatch
	}
	if a.allow {
		return aclDecisionAllow
	}
	return aclDecisionDeny
}

type aclDomainRule struct {
	domain            string
	subdomainsAllowed bool
	allow             bool
}

func (a *aclDomainRule) tryMatch(ip net.IP, domain string) aclDecision {
	domain = strings.TrimPrefix(domain, ".")

	if domain == a.domain ||
		a.subdomainsAllowed && strings.HasSuffix(domain, "."+a.domain) {
		if a.allow {
			return aclDecisionAllow
		}
		return aclDecisionDeny
	}
	return aclDecisionNoMatch
}

type aclAllRule struct {
	allow bool
}

func (a *aclAllRule) tryMatch(ip net.IP, domain string) aclDecision {
	if a.allow {
		return aclDecisionAllow
	}
	return aclDecisionDeny
}

func newACLRule(ruleSubject string, allow bool) (aclRule, error) {
	if ruleSubject == "all" {
		return &aclAllRule{allow: allow}, nil
	}
	_, ipNet, err := net.ParseCIDR(ruleSubject)
	if err != nil {
		ip := net.ParseIP(ruleSubject)
		// support specifying just an IP
		if ip.To4() != nil {
			_, ipNet, err = net.ParseCIDR(ruleSubject + "/32")
		} else if ip.To16() != nil {
			_, ipNet, err = net.ParseCIDR(ruleSubject + "/128")
		}
	}
	if err == nil {
		return &aclIPRule{net: *ipNet, allow: allow}, nil
	}

	subdomainsAllowed := false
	if strings.HasPrefix(ruleSubject, `*.`) {
		subdomainsAllowed = true
		ruleSubject = ruleSubject[2:]
	}
	err = isValidDomainLite(ruleSubject)
	if err != nil {
		return nil, errors.New(ruleSubject + " could not be parsed as either IP, IP network, or domain: " + err.Error())
	}
	return &aclDomainRule{domain: ruleSubject, subdomainsAllowed: subdomainsAllowed, allow: allow}, nil
}

// isValidDomainLite shamelessly rejects non-LDH names. returns nil if domains seems valid
func isValidDomainLite(domain string) error {
	for i := 0; i < len(domain); i++ {
		c := domain[i]
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || c == '_' || '0' <= c && c <= '9' ||
			c == '-' || c == '.' {
			continue
		}
		return errors.New("character " + string(c) + " is not allowed")
	}
	sections := strings.Split(domain, ".")
	for _, s := range sections {
		if len(s) == 0 {
			return errors.New("empty section between dots in domain name or trailing dot")
		}
		if len(s) > 63 {
			return errors.New("domain name section is too long")
		}
	}
	return nil
}
