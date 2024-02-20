/*
 *
 * Copyright 2018 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

// Package dns implements a dns resolver to be installed as the default resolver
// in grpc.
package grpccustomdns

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc/grpclog"
	"google.golang.org/grpc/resolver"
)

type NetResolver interface {
	LookupHost(ctx context.Context, host string) (addrs []string, err error)
}

const (
	defaultPort       = "443"
	defaultDNSSvrPort = "53"
	golang            = "GO"
)

var logger = grpclog.Component("dns")

func init() {
	resolver.Register(&customDnsBuilder{})
}

const MinResolutionRate = 1 * time.Second

// NetResolver groups the methods on net.Resolver that are used by the DNS
// resolver implementation. This allows the default net.Resolver instance to be
// overidden from tests.
var (
	// ErrMissingAddr is the error returned when building a DNS resolver when
	// the provided target name is empty.
	ErrMissingAddr = errors.New("dns resolver: missing address")

	// ErrEndsWithColon is the error returned when building a DNS resolver when
	// the provided target name ends with a colon that is supposed to be the
	// separator between host and port.  E.g. "::" is a valid address as it is
	// an IPv6 address (host only) and "[::]:" is invalid as it ends with a
	// colon as the host and port separator
	ErrEndsWithColon = errors.New("dns resolver: missing port after port-separator colon")
)

var addressDialer = func(address string) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, network, address)
	}
}

var newNetResolver = func(authority string) (NetResolver, error) {
	if authority == "" {
		return net.DefaultResolver, nil
	}

	host, port, err := parseTarget(authority, defaultDNSSvrPort)
	if err != nil {
		return nil, err
	}

	authorityWithPort := net.JoinHostPort(host, port)

	return &net.Resolver{
		PreferGo: true,
		Dial:     addressDialer(authorityWithPort),
	}, nil
}

type customDnsBuilder struct{}

// Build creates and starts a DNS resolver that watches the name resolution of
// the target.
func (b *customDnsBuilder) Build(target resolver.Target, cc resolver.ClientConn, opts resolver.BuildOptions) (resolver.Resolver, error) {
	host, port, err := parseTarget(target.Endpoint(), defaultPort)
	if err != nil {
		return nil, err
	}

	// IP address.
	if ipAddr, ok := formatIP(host); ok {
		addr := []resolver.Address{{Addr: ipAddr + ":" + port}}
		err = cc.UpdateState(resolver.State{Addresses: addr})
		return deadResolver{}, err
	}

	// DNS address (non-IP).
	ctx, cancel := context.WithCancel(context.Background())
	d := &dnsResolver{
		host:                 host,
		port:                 port,
		ctx:                  ctx,
		cancel:               cancel,
		cc:                   cc,
		rn:                   make(chan struct{}, 1),
		disableServiceConfig: opts.DisableServiceConfig,
	}

	d.resolver, err = newNetResolver(target.URL.Host)
	if err != nil {
		return nil, err
	}

	d.wg.Add(1)
	go d.watcher()
	return d, nil
}

// Scheme returns the naming scheme of this resolver builder, which is "dns".
func (b *customDnsBuilder) Scheme() string {
	return "custom-dns"
}

// deadResolver is a resolver that does nothing.
type deadResolver struct{}

func (deadResolver) ResolveNow(resolver.ResolveNowOptions) {}

func (deadResolver) Close() {}

// dnsResolver watches for the name resolution update for a non-IP target.
type dnsResolver struct {
	host     string
	port     string
	resolver NetResolver
	ctx      context.Context
	cancel   context.CancelFunc
	cc       resolver.ClientConn
	// rn channel is used by ResolveNow() to force an immediate resolution of the
	// target.
	rn chan struct{}
	// wg is used to enforce Close() to return after the watcher() goroutine has
	// finished. Otherwise, data race will be possible. [Race Example] in
	// dns_resolver_test we replace the real lookup functions with mocked ones to
	// facilitate testing. If Close() doesn't wait for watcher() goroutine
	// finishes, race detector sometimes will warns lookup (READ the lookup
	// function pointers) inside watcher() goroutine has data race with
	// replaceNetFunc (WRITE the lookup function pointers).
	wg                   sync.WaitGroup
	disableServiceConfig bool
}

// ResolveNow invoke an immediate resolution of the target that this
// dnsResolver watches.
func (d *dnsResolver) ResolveNow(resolver.ResolveNowOptions) {
	select {
	case d.rn <- struct{}{}:
	default:
	}
}

// Close closes the dnsResolver.
func (d *dnsResolver) Close() {
	d.cancel()
	d.wg.Wait()
}

func (d *dnsResolver) watcher() {
	defer d.wg.Done()
	for {
		state, err := d.lookup()
		if err != nil {
			// Report error to the underlying grpc.ClientConn.
			d.cc.ReportError(err)
		} else {
			err = d.cc.UpdateState(*state)
		}

		if err == nil {
			select {
			case <-d.ctx.Done():
				return
			case <-d.rn:
			}
		}
		select {
		case <-d.ctx.Done():
			return
		case <-time.After(MinResolutionRate):
		}
	}
}

func handleDNSError(err error, lookupType string) error {
	dnsErr, ok := err.(*net.DNSError)
	if ok && !dnsErr.IsTimeout && !dnsErr.IsTemporary {
		// Timeouts and temporary errors should be communicated to gRPC to
		// attempt another DNS query (with backoff).  Other errors should be
		// suppressed (they may represent the absence of a TXT record).
		return nil
	}
	if err != nil {
		err = fmt.Errorf("dns: %v record lookup error: %v", lookupType, err)
		logger.Info(err)
	}
	return err
}

func (d *dnsResolver) lookupHost() ([]resolver.Address, error) {
	addrs, err := d.resolver.LookupHost(d.ctx, d.host)
	if err != nil {
		err = handleDNSError(err, "A")
		return nil, err
	}
	newAddrs := make([]resolver.Address, 0, len(addrs))
	for _, a := range addrs {
		ip, ok := formatIP(a)
		if !ok {
			return nil, fmt.Errorf("dns: error parsing A record IP address %v", a)
		}
		addr := ip + ":" + d.port
		newAddrs = append(newAddrs, resolver.Address{Addr: addr})
	}
	return newAddrs, nil
}

func (d *dnsResolver) lookup() (*resolver.State, error) {
	addrs, err := d.lookupHost()
	if err != nil {
		return nil, err
	}

	return &resolver.State{Addresses: addrs}, nil
}

// formatIP returns ok = false if addr is not a valid textual representation of
// an IP address. If addr is an IPv4 address, return the addr and ok = true.
// If addr is an IPv6 address, return the addr enclosed in square brackets and
// ok = true.
func formatIP(addr string) (addrIP string, ok bool) {
	ip := net.ParseIP(addr)
	if ip == nil {
		return "", false
	}
	if ip.To4() != nil {
		return addr, true
	}
	return "[" + addr + "]", true
}

// parseTarget takes the user input target string and default port, returns
// formatted host and port info. If target doesn't specify a port, set the port
// to be the defaultPort. If target is in IPv6 format and host-name is enclosed
// in square brackets, brackets are stripped when setting the host.
// examples:
// target: "www.google.com" defaultPort: "443" returns host: "www.google.com", port: "443"
// target: "ipv4-host:80" defaultPort: "443" returns host: "ipv4-host", port: "80"
// target: "[ipv6-host]" defaultPort: "443" returns host: "ipv6-host", port: "443"
// target: ":80" defaultPort: "443" returns host: "localhost", port: "80"
func parseTarget(target, defaultPort string) (host, port string, err error) {
	if target == "" {
		return "", "", ErrMissingAddr
	}
	if ip := net.ParseIP(target); ip != nil {
		// target is an IPv4 or IPv6(without brackets) address
		return target, defaultPort, nil
	}
	if host, port, err = net.SplitHostPort(target); err == nil {
		if port == "" {
			// If the port field is empty (target ends with colon), e.g. "[::1]:",
			// this is an error.
			return "", "", ErrEndsWithColon
		}
		// target has port, i.e ipv4-host:port, [ipv6-host]:port, host-name:port
		if host == "" {
			// Keep consistent with net.Dial(): If the host is empty, as in ":80",
			// the local system is assumed.
			host = "localhost"
		}
		return host, port, nil
	}
	if host, port, err = net.SplitHostPort(target + ":" + defaultPort); err == nil {
		// target doesn't have port
		return host, port, nil
	}
	return "", "", fmt.Errorf("invalid target address %v, error info: %v", target, err)
}
