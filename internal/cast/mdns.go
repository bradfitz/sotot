// Package cast implements the minimum of Google Cast (CASTV2) needed to
// show images on a Chromecast: mDNS discovery, launching the Default Media
// Receiver, and loading media URLs into it.
package cast

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// Device is a Cast device found on the local network.
type Device struct {
	Name  string // friendly name ("fn" TXT record), e.g. "Garage TV"
	Model string // model name ("md" TXT record), e.g. "Chromecast Ultra"
	Addr  netip.AddrPort
}

const castService = "_googlecast._tcp.local."

var mdnsGroup = &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}

// Discover browses for Cast devices until ctx is done and returns those found.
//
// It sends queries from an ephemeral port, which makes them "legacy" mDNS
// queries (RFC 6762 section 6.7) that responders answer by unicast. That
// avoids needing to share port 5353 with a system mDNS daemon.
func Discover(ctx context.Context) ([]Device, error) {
	pc, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return nil, err
	}
	defer pc.Close()
	stop := context.AfterFunc(ctx, func() { pc.Close() })
	defer stop()

	q := dnsmessage.Message{
		Questions: []dnsmessage.Question{{
			Name:  dnsmessage.MustNewName(castService),
			Type:  dnsmessage.TypePTR,
			Class: dnsmessage.ClassINET,
		}},
	}
	pkt, err := q.Pack()
	if err != nil {
		return nil, err
	}
	go func() {
		// Repeat the query a few times in case of packet loss.
		for i := 0; i < 3; i++ {
			if _, err := pc.WriteTo(pkt, mdnsGroup); err != nil {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(i+1) * 250 * time.Millisecond):
			}
		}
	}()

	type instance struct {
		target string
		port   uint16
		txt    map[string]string
		from   netip.Addr
	}
	instances := map[string]*instance{} // keyed by lowercase instance name
	inst := func(name string) *instance {
		k := strings.ToLower(name)
		if instances[k] == nil {
			instances[k] = &instance{txt: map[string]string{}}
		}
		return instances[k]
	}
	hostIPs := map[string]netip.Addr{} // keyed by lowercase host name

	buf := make([]byte, 9000)
	for {
		n, from, err := pc.ReadFromUDPAddrPort(buf)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			return nil, err
		}
		var m dnsmessage.Message
		if err := m.Unpack(buf[:n]); err != nil || !m.Header.Response {
			continue
		}
		var rrs []dnsmessage.Resource
		rrs = append(rrs, m.Answers...)
		rrs = append(rrs, m.Additionals...)
		for _, rr := range rrs {
			name := rr.Header.Name.String()
			switch b := rr.Body.(type) {
			case *dnsmessage.PTRResource:
				if strings.EqualFold(name, castService) {
					inst(b.PTR.String()).from = from.Addr().Unmap()
				}
			case *dnsmessage.SRVResource:
				in := inst(name)
				in.target, in.port = strings.ToLower(b.Target.String()), b.Port
			case *dnsmessage.TXTResource:
				in := inst(name)
				for _, kv := range b.TXT {
					k, v, _ := strings.Cut(kv, "=")
					in.txt[k] = v
				}
			case *dnsmessage.AResource:
				hostIPs[strings.ToLower(name)] = netip.AddrFrom4(b.A)
			}
		}
	}

	var devs []Device
	for _, in := range instances {
		if in.txt["fn"] == "" || in.port == 0 {
			continue
		}
		ip, ok := hostIPs[in.target]
		if !ok {
			ip = in.from
		}
		if !ip.IsValid() {
			continue
		}
		devs = append(devs, Device{
			Name:  in.txt["fn"],
			Model: in.txt["md"],
			Addr:  netip.AddrPortFrom(ip, in.port),
		})
	}
	sort.Slice(devs, func(i, j int) bool { return devs[i].Name < devs[j].Name })
	return devs, nil
}

// Find discovers devices for up to timeout and returns the one whose
// friendly name matches name, ignoring case.
func Find(ctx context.Context, name string, timeout time.Duration) (Device, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	devs, err := Discover(ctx)
	if err != nil {
		return Device{}, err
	}
	for _, d := range devs {
		if strings.EqualFold(d.Name, name) {
			return d, nil
		}
	}
	var names []string
	for _, d := range devs {
		names = append(names, d.Name)
	}
	if len(names) == 0 {
		return Device{}, errors.New("no Cast devices found")
	}
	return Device{}, fmt.Errorf("no Cast device named %q; found: %s", name, strings.Join(names, ", "))
}
