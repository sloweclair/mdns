// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MIT

package mdns

import (
	"context"
	"fmt"
	"log"
	"net"
	"strings"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

// ServiceEntry is returned after we query for a service
type ServiceEntry struct {
	Name         string
	Host         string
	AddrV4       net.IP
	AddrV6       net.IP // @Deprecated
	AddrV6IPAddr *net.IPAddr
	Port         int
	Info         string
	InfoFields   []string
	SrcIP        net.IP

	Addr net.IP // @Deprecated

	hasTXT bool
	sent   bool
}

// complete is used to check if we have all the info we need
func (s *ServiceEntry) complete() bool {
	return true
	// return (s.AddrV4 != nil || s.AddrV6 != nil || s.Addr != nil) && s.Port != 0 && s.hasTXT
}

// QueryParam is used to customize how a Lookup is performed
type QueryParam struct {
	Service             string               // Service to lookup
	Domain              string               // Lookup domain, default "local"
	Timeout             time.Duration        // Lookup timeout, default 1 second
	Interface           *net.Interface       // Multicast interface to use
	Entries             chan<- *ServiceEntry // Entries Channel
	WantUnicastResponse bool                 // Unicast response desired, as per 5.4 in RFC
	DisableIPv4         bool                 // Whether to disable usage of IPv4 for MDNS operations. Does not affect discovered addresses.
	DisableIPv6         bool                 // Whether to disable usage of IPv6 for MDNS operations. Does not affect discovered addresses.
	Logger              *log.Logger          // Optionally provide a *log.Logger to better manage log output.
}

// DefaultParams is used to return a default set of QueryParam's
func DefaultParams(service string) *QueryParam {
	return &QueryParam{
		Service:             service,
		Domain:              "local",
		Timeout:             time.Second,
		Entries:             make(chan *ServiceEntry),
		WantUnicastResponse: false, // TODO(reddaly): Change this default.
		DisableIPv4:         false,
		DisableIPv6:         false,
	}
}

// Query looks up a given service, in a domain, waiting at most
// for a timeout before finishing the query. The results are streamed
// to a channel. Sends will not block, so clients should make sure to
// either read or buffer.
func Query(params *[]QueryParam, respChan chan<- *ServiceEntry, queryClient *Client) error {
	return QueryContext(context.Background(), params, respChan, queryClient)
}

// QueryContext looks up a given service, in a domain, waiting at most
// for a timeout before finishing the query. The results are streamed
// to a channel. Sends will not block, so clients should make sure to
// either read or buffer. QueryContext will attempt to stop the query
// on cancellation.
func QueryContext(ctx context.Context, params *[]QueryParam, respChan chan<- *ServiceEntry, queryClient *Client) error {
	for _, par := range *params {
		if par.Domain == "" {
			par.Domain = "local"
		}
		if par.Timeout == 0 {
			par.Timeout = time.Second
		}
	}
	// Ensure defaults are set

	// Run the query
	return queryClient.query(params, respChan)
}

// Lookup is the same as Query, however it uses all the default parameters
//func Lookup(service string, entries chan<- *ServiceEntry) error {
//	params := DefaultParams(service)
//	params.Entries = entries
//	return Query(params, []QueryParam{})
//}

// Client provides a query interface that can be used to
// search for service providers using mDNS
type Client struct {
	use_ipv4 bool
	use_ipv6 bool

	ipv4UnicastConn *net.UDPConn
	ipv6UnicastConn *net.UDPConn

	ipv4MulticastConn *net.UDPConn
	ipv6MulticastConn *net.UDPConn

	closed   int32
	closedCh chan struct{} // TODO(reddaly): This doesn't appear to be used.

	log *log.Logger

	MsgChan chan *msgAddr
}

func NewClient(v4 bool, v6 bool, logger *log.Logger, inter *net.Interface) (*Client, error) {
	return newClient(v4, v6, logger, inter)
} // NewClient creates a new mdns Client that can be used to query
// for records
func newClient(v4 bool, v6 bool, logger *log.Logger, inter *net.Interface) (*Client, error) {
	if !v4 && !v6 {
		return nil, fmt.Errorf("Must enable at least one of IPv4 and IPv6 querying")
	}

	// TODO(reddaly): At least attempt to bind to the port required in the spec.
	// Create a IPv4 listener
	var uconn4 *net.UDPConn
	var uconn6 *net.UDPConn
	var mconn4 *net.UDPConn
	var mconn6 *net.UDPConn
	var err error

	// Establish unicast connections
	if v4 {
		uconn4, err = net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353})
		if err != nil {
			logger.Printf("[ERR] mdns: Failed to bind to udp4 port: %v", err)
		}
	}
	if v6 {
		uconn6, err = net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6zero, Port: 0})
		if err != nil {
			logger.Printf("[ERR] mdns: Failed to bind to udp6 port: %v", err)
		}
	}
	if uconn4 == nil && uconn6 == nil {
		return nil, fmt.Errorf("failed to bind to any unicast udp port")
	}

	// Establish multicast connections
	if v4 {
		mconn4, err = net.ListenMulticastUDP("udp4", nil, ipv4Addr)
		if err != nil {
			logger.Printf("[ERR] mdns: Failed to bind to udp4 port: %v", err)
		}
	}
	if v6 {
		mconn6, err = net.ListenMulticastUDP("udp6", nil, ipv6Addr)
		if err != nil {
			logger.Printf("[ERR] mdns: Failed to bind to udp6 port: %v", err)
		}
	}
	if mconn4 == nil && mconn6 == nil {
		return nil, fmt.Errorf("failed to bind to any multicast udp port")
	}

	// Check that unicast and multicast connections have been made for IPv4 and IPv6
	// and disable the respective protocol if not.
	if uconn4 == nil || mconn4 == nil {
		logger.Printf("[INFO] mdns: Failed to listen to both unicast and multicast on IPv4")
		uconn4 = nil
		mconn4 = nil
		v4 = false
	}
	if !v4 && !v6 {
		return nil, fmt.Errorf("at least one of IPv4 and IPv6 must be enabled for querying")
	}

	c := &Client{
		use_ipv4:          v4,
		use_ipv6:          v6,
		ipv4MulticastConn: mconn4,
		ipv6MulticastConn: mconn6,
		ipv4UnicastConn:   uconn4,
		ipv6UnicastConn:   uconn6,
		closedCh:          make(chan struct{}),
		log:               logger,
	}
	c.MsgChan = make(chan *msgAddr, 32)
	err = c.SetInterface(inter)
	if err != nil {
		return c, err
	}
	go c.recv(c.ipv4UnicastConn, c.MsgChan)
	go c.recv(c.ipv4MulticastConn, c.MsgChan)
	return c, nil
}

// Close is used to cleanup the Client
func (c *Client) Close() error {
	if !atomic.CompareAndSwapInt32(&c.closed, 0, 1) {
		// something else already closed it
		return nil
	}

	c.log.Printf("[INFO] mdns: Closing Client %v", *c)
	close(c.closedCh)

	if c.ipv4UnicastConn != nil {
		c.ipv4UnicastConn.Close()
	}
	if c.ipv6UnicastConn != nil {
		c.ipv6UnicastConn.Close()
	}
	if c.ipv4MulticastConn != nil {
		c.ipv4MulticastConn.Close()
	}
	if c.ipv6MulticastConn != nil {
		c.ipv6MulticastConn.Close()
	}

	return nil
}

// setInterface is used to set the query interface, uses system
// default if not provided
func (c *Client) SetInterface(iface *net.Interface) error {
	if c.use_ipv4 {
		p := ipv4.NewPacketConn(c.ipv4UnicastConn)
		if err := p.SetMulticastInterface(iface); err != nil {
			return err
		}
		p = ipv4.NewPacketConn(c.ipv4MulticastConn)
		if err := p.SetMulticastInterface(iface); err != nil {
			return err
		}
	}
	if c.use_ipv6 {
		p2 := ipv6.NewPacketConn(c.ipv6UnicastConn)
		if err := p2.SetMulticastInterface(iface); err != nil {
			return err
		}
		p2 = ipv6.NewPacketConn(c.ipv6MulticastConn)
		if err := p2.SetMulticastInterface(iface); err != nil {
			return err
		}
	}
	return nil
}

// msgAddr carries the message and source address from recv to message processing.
type msgAddr struct {
	msg *dns.Msg
	src *net.UDPAddr
}

// query is used to perform a lookup and stream results
func (c *Client) query(params *[]QueryParam, respChan chan<- *ServiceEntry) error {
	// Send the query
	for _, par := range *params {
		m := new(dns.Msg)
		serviceAddr := fmt.Sprintf("%s.%s.", trimDot(par.Service), trimDot(par.Domain))
		m.SetQuestion(serviceAddr, dns.TypePTR)
		// RFC 6762, section 18.12.  Repurposing of Top Bit of qclass in Question
		// Section
		//
		// In the Question Section of a Multicast DNS query, the top bit of the qclass
		// field is used to indicate that unicast responses are preferred for this
		// particular question.  (See Section 5.4.)
		if par.WantUnicastResponse {
			m.Question[0].Qclass |= 1 << 15
		}
		m.RecursionDesired = false
		if err := c.sendQuery(m); err != nil {
			return err
		}
	}

	// Map the in-progress responses
	inprogress := make(map[string]*ServiceEntry)

	// Listen until we reach the timeout
	finish := time.After(2 * time.Second)
	for {
		select {
		case resp := <-c.MsgChan:
			var inp *ServiceEntry
			for _, answer := range append(resp.msg.Answer, resp.msg.Extra...) {
				// TODO(reddaly): Check that response corresponds to serviceAddr?
				switch rr := answer.(type) {
				case *dns.PTR:
					// Create new entry for this
					inp = ensureName(inprogress, rr.Ptr)

				case *dns.SRV:
					// Check for a target mismatch
					if rr.Target != rr.Hdr.Name {
						alias(inprogress, rr.Hdr.Name, rr.Target)
					}

					// Get the port
					inp = ensureName(inprogress, rr.Hdr.Name)
					inp.Host = rr.Target
					inp.Port = int(rr.Port)

				case *dns.TXT:
					// Pull out the txt
					inp = ensureName(inprogress, rr.Hdr.Name)
					inp.Info = strings.Join(rr.Txt, "|")
					inp.InfoFields = rr.Txt
					inp.hasTXT = true

				case *dns.A:
					// Pull out the IP
					inp = ensureName(inprogress, rr.Hdr.Name)
					inp.Addr = rr.A // @Deprecated
					inp.AddrV4 = rr.A

				case *dns.AAAA:
					// Pull out the IP
					inp = ensureName(inprogress, rr.Hdr.Name)
					inp.Addr = rr.AAAA   // @Deprecated
					inp.AddrV6 = rr.AAAA // @Deprecated
					inp.AddrV6IPAddr = &net.IPAddr{IP: rr.AAAA}
					// link-local IPv6 addresses must be qualified with a zone (interface). Zone is
					// specific to this machine/network-namespace and so won't be carried in the
					// mDNS message itself. We borrow the zone from the source address of the UDP
					// packet, as the link-local address should be valid on that interface.
					if rr.AAAA.IsLinkLocalUnicast() || rr.AAAA.IsLinkLocalMulticast() {
						inp.AddrV6IPAddr.Zone = resp.src.Zone
					}
				}
			}

			if inp == nil {
				continue
			}
			inp.SrcIP = resp.src.IP

			// Check if this entry is complete
			if inp.complete() {
				if inp.sent {
					continue
				}
				inp.sent = true
				select {
				case respChan <- inp:
				default:
				}
			} else {
				// Fire off a node specific query
				m := new(dns.Msg)
				m.SetQuestion(inp.Name, dns.TypePTR)
				m.RecursionDesired = false
				if err := c.sendQuery(m); err != nil {
					c.log.Printf("[ERR] mdns: Failed to query instance %s: %v", inp.Name, err)
				}
			}
		case <-finish:
			return nil
		}
	}
}

// sendQuery is used to multicast a query out
func (c *Client) sendQuery(q *dns.Msg) error {
	buf, err := q.Pack()
	if err != nil {
		return err
	}
	if c.ipv4UnicastConn != nil {
		_, err = c.ipv4UnicastConn.WriteToUDP(buf, ipv4Addr)
		if err != nil {
			return err
		}
	}
	if c.ipv6UnicastConn != nil {
		_, err = c.ipv6UnicastConn.WriteToUDP(buf, ipv6Addr)
		if err != nil {
			return err
		}
	}
	return nil
}

// recv is used to receive until we get a shutdown
func (c *Client) recv(l *net.UDPConn, msgCh chan *msgAddr) {
	if l == nil {
		return
	}
	buf := make([]byte, 65536)
	for atomic.LoadInt32(&c.closed) == 0 {
		n, addr, err := l.ReadFromUDP(buf)

		//fmt.Println("msg", n, addr, err)

		if atomic.LoadInt32(&c.closed) == 1 {
			return
		}

		if err != nil {
			c.log.Printf("[ERR] mdns: Failed to read packet: %v", err)
			continue
		}
		msg := new(dns.Msg)
		if err := msg.Unpack(buf[:n]); err != nil {
			c.log.Printf("[ERR] mdns: Failed to unpack packet: %v", err)
			continue
		}
		select {
		case msgCh <- &msgAddr{
			msg: msg,
			src: addr,
		}:
		case <-c.closedCh:
			return
		}
	}
}

// ensureName is used to ensure the named node is in progress
func ensureName(inprogress map[string]*ServiceEntry, name string) *ServiceEntry {
	if inp, ok := inprogress[name]; ok {
		return inp
	}
	inp := &ServiceEntry{
		Name: name,
	}
	inprogress[name] = inp
	return inp
}

// alias is used to setup an alias between two entries
func alias(inprogress map[string]*ServiceEntry, src, dst string) {
	srcEntry := ensureName(inprogress, src)
	inprogress[dst] = srcEntry
}
