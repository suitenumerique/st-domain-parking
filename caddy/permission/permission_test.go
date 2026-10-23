package permission

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddytls"
)

const ours = "192.0.2.10"

// fakeDNS answers from a map and counts lookups. A missing host is NXDOMAIN.
type fakeDNS struct {
	records map[string][]string
	err     error
	calls   int
}

func (f *fakeDNS) lookup(_ context.Context, host string) ([]netip.Addr, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	records, ok := f.records[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	var addrs []netip.Addr
	for _, r := range records {
		addrs = append(addrs, netip.MustParseAddr(r))
	}
	return addrs, nil
}

// newParking provisions a module over a tree where www.parked.fr is parked.
func newParking(t *testing.T, p *Parking, dns *fakeDNS) *Parking {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "www.parked.fr"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "www.parked.fr", ".parked"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	p.Root = root
	p.lookup = dns.lookup
	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	t.Cleanup(cancel)
	if err := p.Provision(ctx); err != nil {
		t.Fatal(err)
	}
	return p
}

func allowed(t *testing.T, p *Parking, name string) bool {
	t.Helper()
	err := p.CertificateAllowed(context.Background(), name)
	if err != nil && !errors.Is(err, caddytls.ErrPermissionDenied) {
		t.Fatalf("%s: want a denial or nil, got %v", name, err)
	}
	return err == nil
}

func TestParkedHostOnly(t *testing.T) {
	dns := &fakeDNS{}
	p := newParking(t, &Parking{}, dns)

	if !allowed(t, p, "www.parked.fr") {
		t.Error("a parked host is denied")
	}
	if allowed(t, p, "www.other.fr") {
		t.Error("a host that is not parked is allowed")
	}
	if dns.calls != 0 {
		t.Errorf("no public_ips, yet %d DNS lookups", dns.calls)
	}
}

// The name comes from the client's SNI and is joined into a path.
func TestRejectsNonHostnames(t *testing.T) {
	p := newParking(t, &Parking{}, &fakeDNS{})
	for _, name := range []string{
		"", "x/../www.parked.fr", "www.parked.fr/", "../www.parked.fr",
		"www..parked.fr", ".www.parked.fr", "WWW.PARKED.FR", "www.parked.fr.",
	} {
		if allowed(t, p, name) {
			t.Errorf("%q is allowed", name)
		}
	}
}

func TestHelpcenterHosts(t *testing.T) {
	p := newParking(t, &Parking{HelpcenterHosts: "help.example help-alt.example", HelpcenterTarget: "https://help.example/x"}, &fakeDNS{})
	if !allowed(t, p, "help-alt.example") {
		t.Error("a helpcenter host is denied")
	}

	// A half-configured deployment issues nothing for helpcenter hosts.
	p = newParking(t, &Parking{HelpcenterHosts: "help.example"}, &fakeDNS{})
	if allowed(t, p, "help.example") {
		t.Error("a helpcenter host is allowed without a target")
	}
}

func TestDNSMustPointHere(t *testing.T) {
	dns := &fakeDNS{records: map[string][]string{
		"www.parked.fr": {ours, "2001:db8::10"},
	}}
	p := newParking(t, &Parking{PublicIPs: ours + " 2001:db8::10"}, dns)
	if !allowed(t, p, "www.parked.fr") {
		t.Error("a parked host whose records are all ours is denied")
	}

	for name, records := range map[string][]string{
		"elsewhere":          {"198.51.100.1"},
		"one AAAA elsewhere": {ours, "2001:db8::99"},
		"NXDOMAIN":           nil,
	} {
		t.Run(name, func(t *testing.T) {
			dns := &fakeDNS{records: map[string][]string{}}
			if records != nil {
				dns.records["www.parked.fr"] = records
			}
			p := newParking(t, &Parking{PublicIPs: ours + " 2001:db8::10"}, dns)
			if allowed(t, p, "www.parked.fr") {
				t.Errorf("allowed with records %v", records)
			}
		})
	}
}

func TestIPv4MappedAddressesMatch(t *testing.T) {
	dns := &fakeDNS{records: map[string][]string{"www.parked.fr": {"::ffff:" + ours}}}
	p := newParking(t, &Parking{PublicIPs: ours}, dns)
	if !allowed(t, p, "www.parked.fr") {
		t.Error("an IPv4-mapped record of ours is denied")
	}
}

func TestDenialIsCached(t *testing.T) {
	dns := &fakeDNS{records: map[string][]string{"www.parked.fr": {"198.51.100.1"}}}
	p := newParking(t, &Parking{PublicIPs: ours}, dns)
	for range 5 {
		allowed(t, p, "www.parked.fr")
	}
	if dns.calls != 1 {
		t.Errorf("want 1 DNS lookup for 5 denied handshakes, got %d", dns.calls)
	}
}

// A resolver failure must not stop serving certificates already in storage.
func TestResolverFailureAllows(t *testing.T) {
	dns := &fakeDNS{err: &net.DNSError{Err: "i/o timeout", IsTimeout: true}}
	p := newParking(t, &Parking{PublicIPs: ours}, dns)
	if !allowed(t, p, "www.parked.fr") {
		t.Error("denied on a resolver timeout")
	}
	if allowed(t, p, "www.other.fr") {
		t.Error("a host that is not parked is allowed on a resolver timeout")
	}
}

func TestInvalidPublicIP(t *testing.T) {
	p := &Parking{Root: t.TempDir(), PublicIPs: "192.0.2.300"}
	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	defer cancel()
	if err := p.Provision(ctx); err == nil {
		t.Error("an invalid address is accepted")
	}
}

// Placeholders are expanded at provision time, from the environment.
func TestCaddyfile(t *testing.T) {
	t.Setenv("TEST_PUBLIC_IPS", ours)
	d := caddyfile.NewTestDispenser(`parking {
		root /srv/sites
		helpcenter_hosts {env.TEST_HOSTS}
		helpcenter_target {env.TEST_TARGET}
		public_ips {env.TEST_PUBLIC_IPS}
	}`)
	var p Parking
	if err := p.UnmarshalCaddyfile(d); err != nil {
		t.Fatal(err)
	}
	if p.Root != "/srv/sites" || p.PublicIPs != "{env.TEST_PUBLIC_IPS}" {
		t.Fatalf("unexpected fields: root %q, public_ips %q", p.Root, p.PublicIPs)
	}
	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	defer cancel()
	if err := p.Provision(ctx); err != nil {
		t.Fatal(err)
	}
	if !p.publicIPs[netip.MustParseAddr(ours)] || len(p.publicIPs) != 1 {
		t.Errorf("public_ips not expanded: %v", p.publicIPs)
	}
	if len(p.helpcenter) != 0 {
		t.Errorf("unset helpcenter variables gave hosts: %v", p.helpcenter)
	}

	if err := new(Parking).UnmarshalCaddyfile(caddyfile.NewTestDispenser(`parking {
		bogus x
	}`)); err == nil {
		t.Error("an unknown subdirective is accepted")
	}
}
