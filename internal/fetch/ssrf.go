package fetch

import (
	"fmt"
	"net"
	"strings"
)

// AllowList holds the local-address escape hatch: targets that resolve to a
// private/reserved address are refused by default UNLESS they match an entry
// here. Entries are classified at parse time into hostnames, IPs, and CIDRs.
type AllowList struct {
	hosts map[string]bool
	ips   []net.IP
	nets  []*net.IPNet
}

// ParseAllowList classifies each entry as a CIDR, an IP, or (failing those) a
// hostname.
func ParseAllowList(entries []string) AllowList {
	a := AllowList{hosts: map[string]bool{}}
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if _, n, err := net.ParseCIDR(e); err == nil {
			a.nets = append(a.nets, n)
			continue
		}
		if ip := net.ParseIP(e); ip != nil {
			a.ips = append(a.ips, ip)
			continue
		}
		a.hosts[strings.ToLower(e)] = true
	}
	return a
}

// HostAllowed reports whether host was explicitly allowlisted by name.
func (a AllowList) HostAllowed(host string) bool {
	return a.hosts[strings.ToLower(strings.TrimSuffix(host, "."))]
}

// IPAllowed reports whether ip matches an allowlisted IP or CIDR.
func (a AllowList) IPAllowed(ip net.IP) bool {
	for _, x := range a.ips {
		if x.Equal(ip) {
			return true
		}
	}
	for _, n := range a.nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// permitted decides whether a connection to ip (for request host) is allowed:
// any public IP is fine; a private/reserved IP only if the host or the IP is
// on the allowlist. Hostname allowlist entries deliberately trust that name's
// DNS: any blocked-range address it resolves to is permitted.
func (a AllowList) permitted(host string, ip net.IP) bool {
	if !isBlockedIP(ip) {
		return true
	}
	return a.HostAllowed(host) || a.IPAllowed(ip)
}

// isBlockedIP reports whether ip is in a range SSRF protection refuses by
// default: loopback, private, link-local (incl. the cloud metadata address
// 169.254.169.254), multicast, unspecified, CGNAT, benchmarking,
// documentation, and other special-use/reserved ranges.
func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return true
	}
	for _, n := range blockedSpecialNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

var blockedSpecialNets = []*net.IPNet{
	mustCIDR("0.0.0.0/8"),          // current network / software
	mustCIDR("100.64.0.0/10"),      // carrier-grade NAT
	mustCIDR("192.0.0.0/24"),       // IETF protocol assignments
	mustCIDR("192.0.2.0/24"),       // TEST-NET-1 documentation
	mustCIDR("198.18.0.0/15"),      // benchmarking
	mustCIDR("198.51.100.0/24"),    // TEST-NET-2 documentation
	mustCIDR("203.0.113.0/24"),     // TEST-NET-3 documentation
	mustCIDR("240.0.0.0/4"),        // reserved for future use
	mustCIDR("255.255.255.255/32"), // limited broadcast
	mustCIDR("64:ff9b::/96"),       // IPv4/IPv6 translation
	mustCIDR("64:ff9b:1::/48"),     // local-use IPv4/IPv6 translation
	mustCIDR("100::/64"),           // discard-only prefix
	mustCIDR("2001::/23"),          // IETF protocol assignments
	mustCIDR("2001:db8::/32"),      // documentation
	mustCIDR("2002::/16"),          // 6to4
}

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

// SSRFBlockedError is returned when a connection attempt is refused by SSRF
// protection. Its Error() message is safe to show to the model (no hostname
// or internal detail); the full message with the blocked host is available via
// Full() for operator-visible logging.
type SSRFBlockedError struct {
	Host string
}

func (e *SSRFBlockedError) Error() string {
	return "web_fetch: host is blocked by SSRF protection; add it to allow_local_hosts in config.json or ZOT_WEB_ALLOW_LOCAL_HOSTS to permit"
}

// Full returns the detailed message with the blocked hostname, for operator
// logging.
func (e *SSRFBlockedError) Full() string {
	return fmt.Sprintf("ssrf block: %q resolves only to private/reserved addresses and is not on the allowlist", e.Host)
}
