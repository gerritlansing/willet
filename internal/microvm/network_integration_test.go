//go:build integration

package microvm

import (
	"net"
	"strconv"
	"strings"
	"testing"
)

// hostPrivateIPv4 returns a private IPv4 address of this host, avoiding
// container bridges, or "" if there is none.
func hostPrivateIPv4(t *testing.T) string {
	t.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || strings.HasPrefix(iface.Name, "docker") || strings.HasPrefix(iface.Name, "br-") || strings.HasPrefix(iface.Name, "veth") {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && ipn.IP.IsPrivate() {
				return ipn.IP.String()
			}
		}
	}
	return ""
}

// listen accepts and closes connections on all interfaces; it returns the port.
func listen(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	return strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
}

// F10: VMs reach only the public internet by default; --network-allow opens
// exactly the listed address and ports, and nothing next to them.
func TestNetworkAllow(t *testing.T) {
	ip := hostPrivateIPv4(t)
	if ip == "" {
		t.Skip("host has no private IPv4 address")
	}
	portA, portB := listen(t), listen(t)

	withAllow := func(entries ...string) *Provisioner {
		p := newProvFor(t, "it00000000ff")
		rules, err := ParseAllowRules(entries)
		if err != nil {
			t.Fatal(err)
		}
		p.cfg.NetworkAllow = rules
		return p
	}
	reach := func(p *Provisioner, host, port string) error {
		t.Helper()
		checked, err := p.CheckReachable(t.Context(), "willet-it-net", host, port)
		if !checked && err == nil {
			t.Fatal("the stock runner image must support the connectivity check")
		}
		return err
	}

	t.Run("default", func(t *testing.T) {
		p := withAllow()
		if err := reach(p, "api.github.com", "443"); err != nil {
			t.Fatalf("public internet blocked: %v", err)
		}
		err := reach(p, ip, portA)
		if err == nil {
			t.Fatalf("host's private address %s reachable by default", ip)
		}
		if !strings.Contains(err.Error(), "WILLET_NETWORK_ALLOW") {
			t.Errorf("error should say what to allow: %v", err)
		}
		t.Logf("default denial: %v", err)
	})

	t.Run("address and port", func(t *testing.T) {
		p := withAllow(ip + ":" + portA)
		if err := reach(p, ip, portA); err != nil {
			t.Fatalf("allowed %s:%s unreachable: %v", ip, portA, err)
		}
		if err := reach(p, ip, portB); err == nil {
			t.Fatalf("port %s reachable though only %s was allowed", portB, portA)
		}
		// Same listener via the Docker bridge: a neighbouring private address.
		if err := reach(p, "172.17.0.1", portA); err == nil {
			t.Fatal("neighbouring private address reachable")
		}
		if err := reach(p, "api.github.com", "443"); err != nil {
			t.Fatalf("public internet blocked after adding a rule: %v", err)
		}
	})

	t.Run("address, any port", func(t *testing.T) {
		p := withAllow(ip)
		for _, port := range []string{portA, portB} {
			if err := reach(p, ip, port); err != nil {
				t.Fatalf("%s:%s unreachable: %v", ip, port, err)
			}
		}
	})

	if n := countOwnedBy(t, "it00000000ff"); n != 0 {
		t.Fatalf("%d sandboxes left", n)
	}
}

// The operator's DNS rebinding choice: with protection on, a port-restricted
// entry doesn't make a private host reachable by name; with it off it does,
// and still only on the allowed port.
func TestDNSRebindProtectionSetting(t *testing.T) {
	ip := hostPrivateIPv4(t)
	if ip == "" {
		t.Skip("host has no private IPv4 address")
	}
	// nip.io answers <a-b-c-d>.nip.io with a.b.c.d: a public name with a
	// private answer, like a GHES hostname. Resolve via a public resolver,
	// since local resolvers often filter such answers.
	name := strings.ReplaceAll(ip, ".", "-") + ".nip.io"
	portA, portB := listen(t), listen(t)

	prov := func(disableRebind bool) *Provisioner {
		p := newProvFor(t, "it0000000100")
		p.cfg.NetworkAllow, _ = ParseAllowRules([]string{ip + ":" + portA})
		p.cfg.DisableDNSRebindProtection = disableRebind
		p.nameservers = []string{"1.1.1.1:53"}
		return p
	}

	on := prov(false)
	_, err := on.CheckReachable(t.Context(), "willet-it-dns", name, portA)
	if err == nil {
		t.Fatal("with rebind protection on, a port-restricted entry made a private name resolve")
	}
	if !strings.Contains(err.Error(), "does not resolve") || !strings.Contains(err.Error(), "WILLET_DNS_REBIND_PROTECTION") {
		t.Fatalf("want a resolution failure, got: %v", err)
	}
	t.Logf("protection on: %v", err)

	off := prov(true)
	if _, err := off.CheckReachable(t.Context(), "willet-it-dns", name, portA); err != nil {
		t.Fatalf("with rebind protection off, allowed %s:%s unreachable by name: %v", name, portA, err)
	}
	if _, err := off.CheckReachable(t.Context(), "willet-it-dns", name, portB); err == nil {
		t.Fatal("rebind protection off opened ports that were not allowed")
	}
	if _, err := off.CheckReachable(t.Context(), "willet-it-dns", "172.17.0.1", portA); err == nil {
		t.Fatal("rebind protection off opened a neighbouring private address")
	}
}
