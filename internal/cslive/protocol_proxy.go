// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	fmt "fmt"
	url "net/url"
)

func protocol_ValidateProxy(proxy string) error {
	if proxy == "" {
		return nil
	}
	u, e := url.Parse(proxy)
	if e != nil || u.Hostname() == "" || !protocol_StringSet("http", "https", "socks4", "socks4a", "socks5", "socks5h")[u.Scheme] || u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("invalid proxy")
	}
	if u.Port() != "" {
		var p int
		if _, e = fmt.Sscanf(u.Port(), "%d", &p); e != nil || p < 1 || p > 65535 {
			return fmt.Errorf("invalid proxy")
		}
	}
	return nil
}
