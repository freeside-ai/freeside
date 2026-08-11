package elaborate

import (
	"net"
	"testing"
)

func TestPublicResearchIPRejectsSpecialUseSpace(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"0.1.2.3",
		"10.0.0.1",
		"100.64.0.1",
		"127.0.0.1",
		"169.254.1.1",
		"192.0.2.1",
		"198.18.0.1",
		"198.51.100.1",
		"203.0.113.1",
		"240.0.0.1",
		"::1",
		"64:ff9b::c0a8:1",
		"100::1",
		"2001:db8::1",
		"2002:c0a8:1::",
		"fc00::1",
		"fe80::1",
	} {
		if publicResearchIP(net.ParseIP(raw)) {
			t.Errorf("publicResearchIP(%q) = true, want false", raw)
		}
	}
	for _, raw := range []string{"93.184.216.34", "2606:4700:4700::1111"} {
		if !publicResearchIP(net.ParseIP(raw)) {
			t.Errorf("publicResearchIP(%q) = false, want true", raw)
		}
	}
}
