// Package permission registers the "parking" on-demand TLS permission.
//
// A certificate is allowed for a host the builder parked (<root>/<host>/.parked
// exists) or for a helpcenter host, and, when public IPs are configured, only
// once the host's DNS points at them. Without the DNS check, anyone could make
// us order certificates for a parked host whose DNS points elsewhere: every
// order would fail validation, and enough of them exhaust the ACME account's
// rate limits for every other host.
//
// Caddy asks before it even loads a certificate from storage, so this runs on
// the first handshake for every host that is not in the in-memory cache.
package permission

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddytls"
	"go.uber.org/zap"
)

// How long a DNS denial is remembered, so a client retrying handshakes for a
// misconfigured host does not cost a lookup each time.
const deniedFor = time.Minute

const lookupTimeout = 5 * time.Second

func init() {
	caddy.RegisterModule(new(Parking))
}

// Parking is the tls.permission.parking module. The string fields accept
// placeholders, expanded once at provision time: an {env.*} value is data,
// never Caddyfile text.
type Parking struct {
	// Root of the tree the builder writes.
	Root string `json:"root,omitempty"`
	// Space-separated hosts redirected to the helpcenter. They are only
	// allowed when HelpcenterTarget is set too.
	HelpcenterHosts  string `json:"helpcenter_hosts,omitempty"`
	HelpcenterTarget string `json:"helpcenter_target,omitempty"`
	// Space-separated addresses every A and AAAA record of a host must be
	// one of. Empty disables the DNS check.
	PublicIPs string `json:"public_ips,omitempty"`

	root       string
	helpcenter map[string]bool
	publicIPs  map[netip.Addr]bool
	lookup     func(ctx context.Context, host string) ([]netip.Addr, error)
	logger     *zap.Logger

	mu     sync.Mutex
	denied map[string]time.Time
}

// CaddyModule returns the Caddy module information.
func (*Parking) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "tls.permission.parking",
		New: func() caddy.Module { return new(Parking) },
	}
}

// UnmarshalCaddyfile implements caddyfile.Unmarshaler:
//
//	permission parking {
//		root              <dir>
//		helpcenter_hosts  <hosts>
//		helpcenter_target <url>
//		public_ips        <addresses>
//	}
func (p *Parking) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next() // the module name
	for d.NextBlock(0) {
		var field *string
		switch d.Val() {
		case "root":
			field = &p.Root
		case "helpcenter_hosts":
			field = &p.HelpcenterHosts
		case "helpcenter_target":
			field = &p.HelpcenterTarget
		case "public_ips":
			field = &p.PublicIPs
		default:
			return d.Errf("unknown subdirective %q", d.Val())
		}
		if !d.AllArgs(field) {
			return d.ArgErr()
		}
	}
	return nil
}

// Provision implements caddy.Provisioner.
func (p *Parking) Provision(ctx caddy.Context) error {
	p.logger = ctx.Logger()
	repl := caddy.NewReplacer()

	p.root = repl.ReplaceAll(p.Root, "")
	if p.root == "" {
		return errors.New("root is required")
	}

	p.helpcenter = map[string]bool{}
	if repl.ReplaceAll(p.HelpcenterTarget, "") != "" {
		for _, host := range strings.Fields(repl.ReplaceAll(p.HelpcenterHosts, "")) {
			p.helpcenter[host] = true
		}
	}

	p.publicIPs = map[netip.Addr]bool{}
	for _, field := range strings.Fields(repl.ReplaceAll(p.PublicIPs, "")) {
		addr, err := netip.ParseAddr(field)
		if err != nil {
			return fmt.Errorf("public_ips: %w", err)
		}
		p.publicIPs[addr.Unmap()] = true
	}
	if len(p.publicIPs) == 0 {
		p.logger.Warn("no public_ips: certificates are ordered without checking that DNS points here")
	}

	if p.lookup == nil {
		p.lookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	p.denied = map[string]time.Time{}
	return nil
}

// CertificateAllowed implements caddytls.OnDemandPermission.
func (p *Parking) CertificateAllowed(ctx context.Context, name string) error {
	if !validHost(name) {
		return fmt.Errorf("%w: %q is not a hostname", caddytls.ErrPermissionDenied, name)
	}
	if !p.helpcenter[name] {
		_, err := os.Stat(filepath.Join(p.root, name, ".parked"))
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: %s is not parked", caddytls.ErrPermissionDenied, name)
		}
		if err != nil {
			return err
		}
	}
	if len(p.publicIPs) == 0 {
		return nil
	}
	return p.checkDNS(ctx, name)
}

func (p *Parking) checkDNS(ctx context.Context, name string) error {
	p.mu.Lock()
	until, ok := p.denied[name]
	if ok && time.Now().After(until) {
		delete(p.denied, name)
		ok = false
	}
	p.mu.Unlock()
	if ok {
		return fmt.Errorf("%w: DNS for %s does not point here (cached)", caddytls.ErrPermissionDenied, name)
	}

	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	addrs, err := p.lookup(ctx, name)

	var dnsErr *net.DNSError
	if err != nil && !(errors.As(err, &dnsErr) && dnsErr.IsNotFound) {
		// A resolver failure is not an answer. Permission is asked before
		// a certificate is even loaded from storage, so denying here would
		// stop serving valid certificates whenever the resolver hiccups.
		p.logger.Warn("DNS lookup failed, allowing the certificate",
			zap.String("host", name), zap.Error(err))
		return nil
	}
	if err == nil && len(addrs) > 0 && p.allOurs(addrs) {
		return nil
	}

	p.mu.Lock()
	p.denied[name] = time.Now().Add(deniedFor)
	p.mu.Unlock()
	p.logger.Info("DNS does not point here, no certificate",
		zap.String("host", name), zap.Stringers("addresses", addrs))
	return fmt.Errorf("%w: DNS for %s does not point here", caddytls.ErrPermissionDenied, name)
}

// allOurs is true when every address is one of ours: with a single record
// elsewhere, the CA may validate against it and fail.
func (p *Parking) allOurs(addrs []netip.Addr) bool {
	for _, addr := range addrs {
		if !p.publicIPs[addr.Unmap()] {
			return false
		}
	}
	return true
}

// validHost accepts what the builder can have written: lowercase ASCII
// labels of letters, digits and hyphens. Anything else, a "/" or ".." in
// particular, never reaches the filesystem.
func validHost(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}
	for label := range strings.SplitSeq(name, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for _, c := range label {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	return true
}

var (
	_ caddy.Provisioner           = (*Parking)(nil)
	_ caddyfile.Unmarshaler       = (*Parking)(nil)
	_ caddytls.OnDemandPermission = (*Parking)(nil)
)
