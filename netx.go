// Package netx provides additional libraries that extend some of the behaviors
// in the net standard package.
package netx

import (
	"bytes"
	"context"
	"math/rand"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/getlantern/errors"
	"github.com/getlantern/golog"
	"github.com/getlantern/iptool"
)

var (
	log = golog.LoggerFor("netx")
)

var (
	dial                  atomic.Value
	dialUDP               atomic.Value
	listenUDP             atomic.Value
	resolveIPs            atomic.Value
	enableNAT64Once       sync.Once
	nat64Prefix           []byte
	nat64PrefixMx         sync.RWMutex
	updateNAT64PrefixCh   = make(chan interface{}, 1)
	defaultDialTimeout    = 1 * time.Minute
	minNAT64QueryInterval = 10 * time.Second
	zero                  = []byte{0}
	ipt                   iptool.Tool
)

func init() {
	ipt, _ = iptool.New()
	Reset()
}

// EnableNAT64 enables automatic discovery of NAT64 prefix using DNS query for ipv4only.arpa.
// Once enabled, netx will automatically dial IPv4 addresses via IPv6 using this prefix
// if it is available
func EnableNAT64AutoDiscovery() {
	enableNAT64Once.Do(func() {
		log.Debug("Enabling NAT64 auto-discovery")
		go func() {
			var priorNAT64Prefix []byte
			for {
				log.Debugf("Checking for updated NAT64 prefix")
				updateNAT64Prefix()
				nextNAT64Prefix := getNAT64Prefix()
				if !bytes.Equal(priorNAT64Prefix, nextNAT64Prefix) {
					log.Debugf("NAT64 prefix changed from %v to %v", priorNAT64Prefix, nextNAT64Prefix)
					priorNAT64Prefix = nextNAT64Prefix
				}
				// Don't updat NAT64 prefix too often
				time.Sleep(minNAT64QueryInterval)
				// Only update NAT64 Prefix again if it's necessary
				<-updateNAT64PrefixCh
			}
		}()
	})
}

func updateNAT64Prefix() {
	ips, err := resolveIPs.Load().(func(string) ([]net.IP, error))("ipv4only.arpa")
	if err != nil {
		log.Errorf("Error checking for updated nat64 prefix: %v", err)
		return
	}
	for _, ip := range ips {
		if ip.To4() == nil {
			prefix := ip[:12]
			if bytes.Count(prefix, zero) < 12 {
				// before setting new prefix, test it for validity by trying to dial 1.1.1.1
				addr := convertAddressDNS64(prefix, "1.1.1.1:53")
				timeOut := 1 * time.Second
				conn, err := net.DialTimeout("tcp", addr, timeOut)
				if err != nil {
					// if we can't dial, we don't trust the prefix
					continue
				}
				_ = conn.Close()
				nat64PrefixMx.Lock()
				nat64Prefix = prefix
				nat64PrefixMx.Unlock()
				return
			}
		}
	}

	nat64PrefixMx.Lock()
	nat64Prefix = nil
	nat64PrefixMx.Unlock()
}

func refreshNAT64Prefix() {
	select {
	case updateNAT64PrefixCh <- nil:
		// requested refresh of NAT64 prefx
	default:
		// refresh already pending
	}
}

// getNAT64Prefix returns previously fetched ipv6 prefix, or gets a fresh one using DNS lookup
func getNAT64Prefix() []byte {
	nat64PrefixMx.RLock()
	defer nat64PrefixMx.RUnlock()
	return nat64Prefix
}

// convertAddressDNS64 takes the IP address, converts it to ipv6 and applies DNS64 prefix
func convertAddressDNS64(prefix []byte, addr string) string {
	if prefix == nil {
		return addr
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	ip := net.ParseIP(host)
	if ip.To4() == nil { // if it's ipv6 already - don't do anything
		return addr
	}
	if ipt.IsPrivate(&net.IPAddr{
		IP: ip,
	}) {
		// don't mess with private IP addresses
		return addr
	}
	ipv6 := ip.To16()
	copy(ipv6[:12], prefix)
	return net.JoinHostPort(ipv6.String(), port)
}

// Dial is like DialTimeout using a default timeout of 1 minute.
func Dial(network string, addr string) (net.Conn, error) {
	return DialTimeout(network, addr, defaultDialTimeout)
}

// DialUDP acts like Dial but for UDP networks.
func DialUDP(network string, laddr, raddr *net.UDPAddr) (*net.UDPConn, error) {
	return dialUDP.Load().(func(string, *net.UDPAddr, *net.UDPAddr) (*net.UDPConn, error))(network, laddr, raddr)
}

// DialTimeout dials the given addr on the given net type using the configured
// dial function, timing out after the given timeout.
func DialTimeout(network string, addr string, timeout time.Duration) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	conn, err := DialContext(ctx, network, addr)

	cancel()
	return conn, err
}

// DialContext dials the given addr on the given net type using the configured
// dial function, with the given context.
func DialContext(ctx context.Context, network string, addr string) (net.Conn, error) {
	// always convert IPv4 addresses to use a NAT64 prefix if we're on a NAT64 network
	// if EnableNAT64Autodiscovery hasn't been called, if addr is an IPv6 address, if
	// addr is a local address or if we haven't autodiscovered a NAT64 prefix, this is a
	// no-op.
	prefix := getNAT64Prefix()
	addr = convertAddressDNS64(prefix, addr)
	dialer := dial.Load().(func(context.Context, string, string) (net.Conn, error))
	conn, err := dialer(ctx, network, addr)
	if err != nil {
		// error might be because we're now on a NAT64 network (or a different NAT64 network)
		// request a refresh of the NAT64 prefix
		refreshNAT64Prefix()
	}
	return conn, err
}

// ListenUDP acts like ListenPacket for UDP networks.
func ListenUDP(network string, laddr *net.UDPAddr) (*net.UDPConn, error) {
	return listenUDP.Load().(func(network string, laddr *net.UDPAddr) (*net.UDPConn, error))(network, laddr)
}

// OverrideDial overrides the global dial function.
func OverrideDial(dialFN func(ctx context.Context, net string, addr string) (net.Conn, error)) {
	dial.Store(dialFN)
}

// OverrideDialUDP overrides the global dialUDP function.
func OverrideDialUDP(dialFN func(net string, laddr, raddr *net.UDPAddr) (*net.UDPConn, error)) {
	dialUDP.Store(dialFN)
}

// OverrideListenUDP overrides the global listenUDP function.
func OverrideListenUDP(listenFN func(network string, laddr *net.UDPAddr) (*net.UDPConn, error)) {
	listenUDP.Store(listenFN)
}

// Resolve resolves the given tcp address using the configured resolve function.
func Resolve(network string, addr string) (*net.TCPAddr, error) {
	switch network {
	case "tcp", "tcp4", "tcp6":
		break
	case "":
		network = "tcp"
	default:
		return nil, errors.New("Unsupported network: %v", network)
	}

	ip, port, err := resolve(addr)
	if err != nil {
		return nil, err
	}

	return &net.TCPAddr{IP: ip, Port: port}, nil
}

func ResolveUDPAddr(network string, addr string) (*net.UDPAddr, error) {
	switch network {
	case "udp", "udp4", "udp6":
		break
	case "":
		network = "udp"
	default:
		return nil, errors.New("Unsupported network: %v", network)
	}

	ip, port, err := resolve(addr)
	if err != nil {
		return nil, err
	}

	return &net.UDPAddr{IP: ip, Port: port}, nil
}

func resolve(addr string) (net.IP, int, error) {
	host, _port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, 0, errors.New("Unable to parse addr %v: %v", addr, err)
	}
	port, err := strconv.Atoi(_port)
	if err != nil {
		return nil, 0, errors.New("Unable to convert port %v to integer: %v", _port, err)
	}
	ips, err := resolveIPs.Load().(func(string) ([]net.IP, error))(host)
	if err != nil {
		return nil, 0, errors.New("Unable to resolve IP for %v: %v", host, err)
	}
	ip, err := pickRandomIP(ips)
	if err != nil {
		return nil, 0, err
	}
	return ip, port, nil
}

// OverrideResolveIPs overrides the global IP resolution function.
func OverrideResolveIPs(resolveFN func(host string) ([]net.IP, error)) {
	resolveIPs.Store(resolveFN)
}

// Reset resets netx to its default settings
func Reset() {
	var d net.Dialer
	OverrideDial(d.DialContext)
	OverrideDialUDP(net.DialUDP)
	OverrideListenUDP(net.ListenUDP)
	OverrideResolveIPs(net.LookupIP)
}

func pickRandomIP(ips []net.IP) (net.IP, error) {
	length := len(ips)
	if length < 1 {
		return nil, errors.New("no IP address")
	}
	return ips[rand.Intn(length)], nil
}
