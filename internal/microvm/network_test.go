package microvm

import (
	"net/netip"
	"slices"
	"strings"
	"testing"

	msb "github.com/superradcompany/microsandbox/sdk/go"
)

func TestParseAllowRule(t *testing.T) {
	valid := map[string]string{ // entry -> canonical String()
		"10.0.5.10":             "10.0.5.10",
		" 10.0.5.10 ":           "10.0.5.10",
		"10.0.5.10:443":         "10.0.5.10:443",
		"10.0.5.10/32":          "10.0.5.10",
		"10.1.0.0/16":           "10.1.0.0/16",
		"10.1.2.3/16":           "10.1.0.0/16", // masked
		"10.1.0.0/16:8000-8100": "10.1.0.0/16:8000-8100",
		"192.168.1.1:22":        "192.168.1.1:22",
		"fd00::1":               "fd00::1",
		"fd00::/8":              "fd00::/8",
		"[fd00::1]:443":         "[fd00::1]:443",
		"[fd00::/8]:443":        "[fd00::/8]:443",
		"[fd00::1]":             "fd00::1",
	}
	for in, want := range valid {
		r, err := ParseAllowRule(in)
		if err != nil {
			t.Errorf("%q: unexpected error: %v", in, err)
			continue
		}
		if r.String() != want {
			t.Errorf("%q: got %s, want %s", in, r, want)
		}
	}

	invalid := map[string]string{
		"":                      "empty",
		"ghes.corp.example":     "hostnames can't be allowed",
		"ghes.corp.example:443": "hostnames can't be allowed",
		"private":               "hostnames can't be allowed",
		"*":                     "hostnames can't be allowed",
		"0.0.0.0/0":             "every address",
		"::/0":                  "every address",
		"[::/0]:443":            "every address",
		"10.0.0.1:0":            "invalid port",
		"10.0.5.10:":            "invalid port",
		"10.1.0.0/16:":          "invalid port",
		"[fd00::1]:":            "invalid port",
		"[fd00::/8]:":           "invalid port",
		"10.0.0.1:70000":        "invalid port",
		"10.0.0.1:abc":          "invalid port",
		"10.0.0.1:9000-8000":    "invalid port range",
		"10.0.0.1:1-":           "invalid port range",
		"10.0.0.0/33":           "10.0.0.0/33",
		"[fd00::1":              "missing ]",
		"[fd00::1]443":          "expected :port",
	}
	for in, want := range invalid {
		_, err := ParseAllowRule(in)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: want error containing %q, got %v", in, want, err)
		}
	}
}

func TestNetworkPolicy(t *testing.T) {
	if networkPolicy(nil, false, nil) != nil {
		t.Fatal("no allow rules must leave the runtime default untouched")
	}

	rules, err := ParseAllowRules([]string{"10.0.5.10", "10.1.0.0/16:443"})
	if err != nil {
		t.Fatal(err)
	}
	got := networkPolicy(rules, false, nil)
	base := msb.NetworkPolicy.FromProfiles(msb.NetworkProfilePublic)

	// The base policy is kept intact: same defaults, all of its rules.
	if got.DefaultEgress != msb.PolicyActionDeny || got.DefaultEgress != base.DefaultEgress || got.DefaultIngress != base.DefaultIngress {
		t.Fatalf("defaults changed: egress %s ingress %s", got.DefaultEgress, got.DefaultIngress)
	}
	if got.DNS != nil {
		t.Fatalf("DNS settings changed without being asked: %+v", got.DNS)
	}
	for _, br := range base.Rules {
		if !slices.ContainsFunc(got.Rules, func(r msb.PolicyRule) bool { return sameRule(r, br) }) {
			t.Errorf("base rule dropped: %+v", br)
		}
	}
	// Found by content, not position: the gateway DNS allowance.
	dns := slices.ContainsFunc(got.Rules, func(r msb.PolicyRule) bool {
		return r.Action == msb.PolicyActionAllow && r.Destination == "host" && r.Port == "53"
	})
	if !dns {
		t.Error("gateway DNS allowance missing")
	}

	// Exactly the requested exceptions were added, as egress allows.
	want := []msb.PolicyRule{
		{Action: msb.PolicyActionAllow, Direction: msb.PolicyDirectionEgress, Destination: "10.0.5.10"},
		{Action: msb.PolicyActionAllow, Direction: msb.PolicyDirectionEgress, Destination: "10.1.0.0/16", Port: "443"},
	}
	if len(got.Rules) != len(base.Rules)+len(want) {
		t.Fatalf("got %d rules, want %d base + %d added", len(got.Rules), len(base.Rules), len(want))
	}
	for _, w := range want {
		if !slices.ContainsFunc(got.Rules, func(r msb.PolicyRule) bool { return sameRule(r, w) }) {
			t.Errorf("exception missing: %+v", w)
		}
	}
	// Nothing opens a whole destination group except the base DNS rule.
	for _, r := range got.Rules {
		if (r.Destination == "private" || r.Destination == "*") || (r.Destination == "host" && r.Port != "53") {
			t.Fatalf("policy opens a whole group: %+v", r)
		}
	}
}

func sameRule(a, b msb.PolicyRule) bool {
	return a.Action == b.Action && a.Direction == b.Direction && a.Destination == b.Destination &&
		a.Port == b.Port && slices.Equal(a.Protocols, b.Protocols) && a.Protocol == b.Protocol
}

func TestCoveredOnlyWithPort(t *testing.T) {
	p := &Provisioner{}
	p.cfg.NetworkAllow, _ = ParseAllowRules([]string{"10.0.5.10:443"})
	if !p.coveredOnlyWithPort([]string{"10.0.5.10"}) {
		t.Error("port-restricted entry should be flagged: its DNS answers are dropped")
	}
	p.cfg.NetworkAllow, _ = ParseAllowRules([]string{"10.0.5.10:443", "10.0.0.0/16"})
	if p.coveredOnlyWithPort([]string{"10.0.5.10"}) {
		t.Error("a port-less entry covers the address; not a DNS problem")
	}
}

func TestIsPublic(t *testing.T) {
	for addr, want := range map[string]bool{
		"140.82.113.6": true, "10.0.0.1": false, "192.168.1.1": false,
		"172.17.0.1": false, "127.0.0.1": false, "169.254.169.254": false, "fd00::1": false,
	} {
		if got := isPublic(netip.MustParseAddr(addr)); got != want {
			t.Errorf("%s: got %v", addr, got)
		}
	}
}

func TestNetworkPolicyRebindSetting(t *testing.T) {
	got := networkPolicy(nil, true, nil)
	if got == nil || got.DNS == nil || got.DNS.RebindProtection == nil || *got.DNS.RebindProtection {
		t.Fatalf("disabling rebind protection not applied: %+v", got)
	}
	// Everything else stays the default policy.
	def := msb.NetworkPolicy.FromProfiles(msb.NetworkProfilePublic)
	if got.DefaultEgress != def.DefaultEgress || len(got.Rules) != len(def.Rules) {
		t.Fatalf("policy widened: %+v", got)
	}
}

func TestHintOffersBothOptions(t *testing.T) {
	p := &Provisioner{}
	p.cfg.NetworkAllow, _ = ParseAllowRules([]string{"10.0.5.10:443"})
	// Uses a literal IP so the lookup needs no DNS.
	h := p.hint("10.0.5.10", "443", true)
	if !strings.Contains(h, "10.0.5.10 (no port)") || !strings.Contains(h, "WILLET_DNS_REBIND_PROTECTION=false") {
		t.Errorf("hint should offer both fixes: %s", h)
	}
	p.cfg.DisableDNSRebindProtection = true
	if h := p.hint("10.0.5.10", "443", true); strings.Contains(h, "REBIND") {
		t.Errorf("rebind protection is already off; hint shouldn't suggest it: %s", h)
	}
}
