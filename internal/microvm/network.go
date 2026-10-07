package microvm

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	msb "github.com/superradcompany/microsandbox/sdk/go"
)

// By default microsandbox VMs may reach the public internet and DNS, and
// nothing else: private ranges, loopback, link-local, cloud metadata and the
// host are denied. AllowRules add narrow exceptions, e.g. for a GHES server
// or package mirror on a private network.
//
// Names resolve through microsandbox's DNS proxy, whose rebinding protection
// drops private answers unless an address rule *without a port restriction*
// covers them. A port-restricted rule therefore only helps clients that
// connect by IP. Verified against microsandbox v0.7.7.

// AllowRule permits egress to an address or network, optionally on specific
// ports.
type AllowRule struct {
	// Prefix is the destination; a single address is a /32 or /128.
	Prefix netip.Prefix
	// Port is "" for any port, a port ("443") or a range ("8000-8100").
	Port string
}

func (r AllowRule) String() string {
	dest := r.dest()
	if r.Port == "" {
		return dest
	}
	if r.Prefix.Addr().Is6() {
		return "[" + dest + "]:" + r.Port
	}
	return dest + ":" + r.Port
}

// dest renders the prefix as an address when it covers a single host.
func (r AllowRule) dest() string {
	if r.Prefix.IsSingleIP() {
		return r.Prefix.Addr().String()
	}
	return r.Prefix.String()
}

// ParseAllowRule parses "IP", "CIDR", "IP:port", "CIDR:port-range", and the
// bracketed IPv6 forms "[IPv6]:port" and "[IPv6/len]:port".
func ParseAllowRule(entry string) (AllowRule, error) {
	s := strings.TrimSpace(entry)
	if s == "" {
		return AllowRule{}, fmt.Errorf("empty network allow entry")
	}

	// hasPort records whether a port delimiter was given, so that a
	// delimiter with nothing after it ("10.0.5.10:") fails instead of
	// silently meaning "all ports".
	dest, port, hasPort := s, "", false
	switch {
	case strings.HasPrefix(s, "["):
		end := strings.Index(s, "]")
		if end < 0 {
			return AllowRule{}, fmt.Errorf("%q: missing ]", entry)
		}
		dest, port = s[1:end], s[end+1:]
		if port != "" {
			if !strings.HasPrefix(port, ":") {
				return AllowRule{}, fmt.Errorf("%q: expected :port after ]", entry)
			}
			port, hasPort = port[1:], true
		}
	case strings.Count(s, ":") == 1:
		dest, port, hasPort = s[:strings.Index(s, ":")], s[strings.Index(s, ":")+1:], true
	}

	var prefix netip.Prefix
	if strings.Contains(dest, "/") {
		p, err := netip.ParsePrefix(dest)
		if err != nil {
			return AllowRule{}, fmt.Errorf("%q: %w", entry, err)
		}
		prefix = p.Masked()
	} else {
		a, err := netip.ParseAddr(dest)
		if err != nil {
			return AllowRule{}, fmt.Errorf("%q: not an IP address or CIDR; hostnames can't be allowed because the VM resolves names itself, so allow the host's address instead", entry)
		}
		prefix = netip.PrefixFrom(a, a.BitLen())
	}
	if prefix.Bits() == 0 {
		return AllowRule{}, fmt.Errorf("%q would allow every address; list the specific networks you need", entry)
	}
	if hasPort {
		if err := validatePort(port); err != nil {
			return AllowRule{}, fmt.Errorf("%q: %w", entry, err)
		}
	}
	return AllowRule{Prefix: prefix, Port: port}, nil
}

// ParseAllowRules parses a list of entries, as given on the command line.
func ParseAllowRules(entries []string) ([]AllowRule, error) {
	var rules []AllowRule
	for _, e := range entries {
		r, err := ParseAllowRule(e)
		if err != nil {
			return nil, err
		}
		rules = append(rules, r)
	}
	return rules, nil
}

func validatePort(p string) error {
	lo, hi, isRange := strings.Cut(p, "-")
	a, err := strconv.ParseUint(lo, 10, 16)
	if err != nil || a == 0 {
		return fmt.Errorf("invalid port %q", p)
	}
	if !isRange {
		return nil
	}
	b, err := strconv.ParseUint(hi, 10, 16)
	if err != nil || b == 0 || b < a {
		return fmt.Errorf("invalid port range %q", p)
	}
	return nil
}

// networkPolicy returns microsandbox's default policy (public internet and
// gateway DNS only) with the allow rules and host ports inserted, or nil to
// leave the runtime default untouched when nothing differs from it.
// hostPorts are ports on the host itself (host.microsandbox.internal), which
// the default policy denies.
//
// disableRebind turns off DNS rebinding protection so private DNS answers
// reach the guest. Connections are still limited to the allowed destinations;
// what changes is that port-restricted entries then also work by name. That
// is an operator decision, off by default.
func networkPolicy(allow []AllowRule, hostPorts []string, disableRebind bool, nameservers []string) *msb.NetworkConfig {
	if len(allow) == 0 && len(hostPorts) == 0 && !disableRebind && len(nameservers) == 0 {
		return nil
	}
	// The Public profile is default-deny with only allow rules (gateway DNS
	// and public destinations), so appending further allow rules keeps its
	// meaning regardless of how the SDK orders its own rules.
	cfg := msb.NetworkPolicy.FromProfiles(msb.NetworkProfilePublic)
	for _, a := range allow {
		cfg.Rules = append(cfg.Rules, msb.PolicyRule{
			Action:      msb.PolicyActionAllow,
			Direction:   msb.PolicyDirectionEgress,
			Destination: a.dest(),
			Port:        a.Port,
		})
	}
	for _, port := range hostPorts {
		cfg.Rules = append(cfg.Rules, msb.PolicyRule{
			Action:      msb.PolicyActionAllow,
			Direction:   msb.PolicyDirectionEgress,
			Destination: "host",
			Protocol:    msb.PolicyProtocolTCP,
			Port:        port,
		})
	}
	if disableRebind || len(nameservers) > 0 {
		rebind := !disableRebind
		cfg.DNS = &msb.DNSConfig{RebindProtection: &rebind, Nameservers: nameservers}
	}
	return cfg
}

// ---------------------------------------------------------------------------
// Preflight

// CheckReachable boots a VM with the runner network policy and checks that
// host:port resolves and accepts TCP connections from inside it. It returns
// an error with a hint on what to allow if not. checked is false if the image
// has no tool to test with; then nothing is known and err is nil.
func (p *Provisioner) CheckReachable(ctx context.Context, name, host, port string) (bool, error) {
	checked := false
	err := withTempVM(ctx, p, name, func() (*msb.Sandbox, error) {
		sb, err := p.createTempSandbox(ctx, name, append(p.sandboxOptions(name, p.ImageRef()),
			msb.WithPullPolicy(msb.PullPolicyIfMissing), msb.WithReplace())...)
		if err != nil {
			return nil, fmt.Errorf("boot connectivity check VM: %w", err)
		}
		return sb, nil
	}, func(sb *msb.Sandbox) error {
		out, err := sb.Exec(ctx, "sh", []string{"-c", reachScript, "reach", host, port}, msb.WithExecTimeout(reachTimeout))
		if err != nil {
			return fmt.Errorf("run connectivity check: %w", err)
		}
		switch out.ExitCode() {
		case reachOK:
			checked = true
			return nil
		case reachUnavailable:
			return nil
		case reachUnresolved:
			checked = true
			return fmt.Errorf("%s does not resolve inside the runner VMs%s", host, p.hint(host, port, true))
		default:
			checked = true
			return fmt.Errorf("%s:%s is not reachable from inside the runner VMs%s", host, port, p.hint(host, port, false))
		}
	})
	return checked, err
}

// Exit codes of reachScript.
const (
	reachOK          = 0
	reachFailed      = 1
	reachUnresolved  = 2
	reachUnavailable = 3 // no usable tool; nothing was tested
)

// reachTimeout bounds the whole check; each tool also has its own limit.
const reachTimeout = time.Minute

// reachScript tests a TCP connection to $1:$2 with bash, nc or curl,
// whichever the image has. Exit codes are listed above. It never reports
// success unless a connection was actually established.
const reachScript = `h=$1 p=$2
case $h in *:*) ip=$h ;; *[!0-9.]*) ip= ;; *) ip=$h ;; esac
if [ -z "$ip" ] && command -v getent >/dev/null 2>&1; then
  getent hosts "$h" >/dev/null 2>&1 || exit 2
fi
t=; command -v timeout >/dev/null 2>&1 && t="timeout 10"
if command -v bash >/dev/null 2>&1; then
  $t bash -c "</dev/tcp/$h/$p" >/dev/null 2>&1 && exit 0
  exit 1
elif command -v nc >/dev/null 2>&1; then
  nc -z -w 10 "$h" "$p" >/dev/null 2>&1 && exit 0
  exit 1
elif command -v curl >/dev/null 2>&1; then
  case $h in *:*) u="[$h]" ;; *) u=$h ;; esac
  # curl's telnet mode waits for the peer until --max-time, so exit 28 with a
  # nonzero connect time still means the TCP connection succeeded.
  c=$(curl -s -o /dev/null --connect-timeout 8 --max-time 10 -w '%{time_connect}' "telnet://$u:$p" </dev/null)
  case $? in
    0) exit 0 ;;
    6) exit 2 ;;
    7) exit 1 ;;
    28) case $c in *[1-9]*) exit 0 ;; *) exit 1 ;; esac ;;
    *) exit 3 ;; # unsupported protocol, bad option, proxy trouble: untested
  esac
fi
exit 3`

// hint explains what to add to the allow list, based on how the host
// resolves outside the VM.
func (p *Provisioner) hint(host, port string, resolveFailed bool) string {
	addrs, _ := net.LookupHost(host)
	var private []string
	for _, a := range addrs {
		if ip, err := netip.ParseAddr(a); err == nil && !isPublic(ip) {
			private = append(private, ip.String())
		}
	}
	if len(private) == 0 {
		if resolveFailed && len(addrs) == 0 && !p.cfg.DisableDNSRebindProtection {
			// The host can't tell us the address either.
			return ". If it resolves to a private address, add that address without a port to WILLET_NETWORK_ALLOW, or set WILLET_DNS_REBIND_PROTECTION=false to allow a port-restricted entry to work by name; otherwise check DNS"
		}
		return ". Check the host's firewall and DNS"
	}
	target := strings.Join(private, ", ")
	if (resolveFailed || p.coveredOnlyWithPort(private)) && !p.cfg.DisableDNSRebindProtection {
		return fmt.Sprintf(". %s is a private address, and microsandbox's DNS rebinding protection only lets a private answer through when an allow entry covers the address without a port. Either add %s (no port) to WILLET_NETWORK_ALLOW, or keep a port-restricted entry and set WILLET_DNS_REBIND_PROTECTION=false (see the README for the trade-off)", target, target)
	}
	return fmt.Sprintf(". %s is a private address, which runner VMs can't reach by default. Add %s (or %s:%s) to WILLET_NETWORK_ALLOW", target, target, private[0], port)
}

// coveredOnlyWithPort reports whether some address is allowed only by
// port-restricted rules, which let connections through but not DNS answers.
func (p *Provisioner) coveredOnlyWithPort(addrs []string) bool {
	for _, a := range addrs {
		ip, _ := netip.ParseAddr(a)
		withPort, withoutPort := false, false
		for _, r := range p.cfg.NetworkAllow {
			if r.Prefix.Contains(ip) {
				if r.Port == "" {
					withoutPort = true
				} else {
					withPort = true
				}
			}
		}
		if withPort && !withoutPort {
			return true
		}
	}
	return false
}

func isPublic(ip netip.Addr) bool {
	return ip.IsGlobalUnicast() && !ip.IsPrivate()
}
