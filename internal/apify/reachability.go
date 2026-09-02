package apify

import (
	"context"
	"net"
	"net/url"
	"sync"
	"time"
)

// Facebook hands out delivery URLs on the CDN node nearest the resolving
// client. Some of those nodes (the ISP-hosted *.fna.fbcdn.net appliances)
// publish only an AAAA record, and Arker's hosts have no IPv6 route, so the
// bytes are unreachable from here even though the URL is perfectly valid.
// The node varies run to run, so the fix is to notice before downloading and
// resolve again rather than burn the download attempt.

// hostResolver looks up a host's addresses; swapped in tests.
type hostResolver func(ctx context.Context, host string) ([]net.IP, error)

func defaultHostResolver(ctx context.Context, host string) ([]net.IP, error) {
	return net.DefaultResolver.LookupIP(ctx, "ip", host)
}

// ipv6Egress reports whether this process can open an IPv6 TCP connection
// at all, probed once per process, by connecting to a public resolver's
// HTTPS port.
var ipv6Egress = sync.OnceValue(func() bool {
	conn, err := net.DialTimeout("tcp6", "[2606:4700:4700::1111]:443", 3*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
})

// unreachableHost returns the first delivery host among the URLs that this
// process cannot connect to, or "" when they are all reachable. A lookup
// failure is treated as reachable: DNS trouble is transient and the download
// will report it properly.
func (c *Client) unreachableHost(ctx context.Context, rawURLs ...string) string {
	resolve := c.resolveHost
	if resolve == nil {
		resolve = defaultHostResolver
	}
	hasIPv6 := c.ipv6
	if hasIPv6 == nil {
		hasIPv6 = ipv6Egress
	}
	seen := map[string]bool{}
	for _, rawURL := range rawURLs {
		if rawURL == "" {
			continue
		}
		parsed, err := url.Parse(rawURL)
		if err != nil || parsed.Hostname() == "" || seen[parsed.Hostname()] {
			continue
		}
		host := parsed.Hostname()
		seen[host] = true
		lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		ips, err := resolve(lookupCtx, host)
		cancel()
		if err != nil || len(ips) == 0 {
			continue
		}
		v4 := false
		for _, ip := range ips {
			if ip.To4() != nil {
				v4 = true
				break
			}
		}
		if !v4 && !hasIPv6() {
			return host
		}
	}
	return ""
}
