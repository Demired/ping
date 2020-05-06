package ping

import (
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

const (
	// DefaultTXTimeout is socket send timeout
	DefaultTXTimeout int64 = 2000
	// ProtocolIPv4ICMP is IANA ICMP IPv4
	ProtocolIPv4ICMP = 1
	// ProtocolIPv6ICMP is IANA ICMP IPv6
	ProtocolIPv6ICMP = 58

	// IPv4ICMPTypeEchoReply is ICMPv4 Echo Reply
	IPv4ICMPTypeEchoReply = 0
	// IPv4ICMPTypeDestinationUnreachable is ICMPv4 Destination Unreachable
	IPv4ICMPTypeDestinationUnreachable = 3
	// IPv4ICMPTypeTimeExceeded is ICMPv4 Time Exceeded
	IPv4ICMPTypeTimeExceeded = 11

	// IPv6ICMPTypeEchoReply is ICMPv6 Echo Reply
	IPv6ICMPTypeEchoReply = 129
	// IPv6ICMPTypeDestinationUnreachable is ICMPv6 Destination Unreachable
	IPv6ICMPTypeDestinationUnreachable = 1
	//IPv6ICMPTypeTimeExceeded is ICMPv6 Time Exceeded
	IPv6ICMPTypeTimeExceeded = 3
)

// packet represents ping packet
type packet struct {
	bytes []byte
	addr  net.Addr
	ttl   int
	err   error
}

// Response represent ping response
type Response struct {
	RTT      float64
	Size     int
	TTL      int
	Sequence int
	Addr     string
	Timeout  bool
	Error    error
}

// Ping represents ping
type Ping struct {
	m         icmp.Message
	id        int
	seq       int
	pSize     int
	ttl       int
	count     int
	addr      *net.IPAddr
	addrs     []net.IP
	target    string
	isV4Avail bool
	forceV4   bool
	forceV6   bool
	network   string
	source    string
	timeout   time.Duration
	interval  time.Duration
}

// NewPing constructs ping object
func NewPing(target string) (*Ping, error) {
	var err error

	p := Ping{
		id:        rand.Intn(0xffff),
		seq:       -1,
		pSize:     64,
		ttl:       64,
		target:    target,
		isV4Avail: false,
		count:     1,
		forceV4:   false,
		forceV6:   false,
		network:   "ip",
		source:    "",
	}

	// resolve host
	ips, err := net.LookupIP(target)
	if err != nil {
		return nil, err
	}
	p.addrs = ips
	if err := p.SetIP(ips); err != nil {
		return nil, err
	}

	if p.timeout, err = time.ParseDuration("2s"); err != nil {
		log.Fatal(err)
	}

	return &p, nil
}

// SetCount sets the count packets
func (p *Ping) SetCount(c int) {
	p.count = c
}

// SetTTL sets the IPv4 packet TTL or IPv6 hop-limit
func (p *Ping) SetTTL(c int) {
	p.ttl = c
}

// SetPacketSize sets the ICMP packet size
func (p *Ping) SetPacketSize(s int) {
	p.pSize = s
}

// SetForceV4 sets force v4
func (p *Ping) SetForceV4() {
	p.forceV4 = true
	p.forceV6 = false
}

// SetForceV6 sets force v6
func (p *Ping) SetForceV6() {
	p.forceV4 = false
	p.forceV6 = true
}

// SetIP set ip address
func (p *Ping) SetIP(ips []net.IP) error {
	for _, ip := range ips {
		if IsIPv4(ip) && !p.forceV6 {
			p.addr = &net.IPAddr{IP: ip}
			p.isV4Avail = true
			return nil
		} else if IsIPv6(ip) && !p.forceV4 {
			p.addr = &net.IPAddr{IP: ip}
			p.isV4Avail = false
			return nil
		}
	}
	return fmt.Errorf("there is not  A or AAAA record")
}

// IsIPv4 returns true if ip version is v4
func IsIPv4(ip net.IP) bool {
	return len(ip.To4()) == net.IPv4len
}

// IsIPv6 returns true if ip version is v6
func IsIPv6(ip net.IP) bool {
	if r := strings.Index(ip.String(), ":"); r != -1 {
		return true
	}
	return false
}

// Run sends the ICMP message to destination / target
func (p *Ping) Run() chan Response {
	var r = make(chan Response, 1)
	go func() {
		for n := 0; n < p.count; n++ {
			p.Ping(r)
			if n != p.count-1 {
				time.Sleep(p.interval)
			}
		}
		close(r)
	}()
	return r
}

// listen starts to listen incoming icmp
func (p *Ping) listen(network string) (*icmp.PacketConn, error) {
	c, err := icmp.ListenPacket(network, p.source)
	if err != nil {
		return c, err
	}
	return c, nil
}

// recv reads icmp message
func (p *Ping) recv(conn *icmp.PacketConn, rcvdChan chan<- Response) {
	var (
		err              error
		src              net.Addr
		ts               = time.Now()
		n, ttl, icmpType int
	)

	bytes := make([]byte, 1500)
	conn.SetReadDeadline(time.Now().Add(p.timeout))

	for {
		if p.isV4Avail {
			var cm *ipv4.ControlMessage
			n, cm, src, err = conn.IPv4PacketConn().ReadFrom(bytes)
			if cm != nil {
				ttl = cm.TTL
			}
		} else {
			var cm *ipv6.ControlMessage
			n, cm, src, err = conn.IPv6PacketConn().ReadFrom(bytes)
			if cm != nil {
				ttl = cm.HopLimit
			}
		}
		if err != nil {
			if neterr, ok := err.(*net.OpError); ok {
				if neterr.Timeout() {
					err = errors.New("Request timeout")
				}
			}
		}

		bytes = bytes[:n]

		if n > 0 {
			icmpType = int(bytes[0])
		}

		switch icmpType {
		case IPv4ICMPTypeTimeExceeded:
			if n >= 28 && p.isMyTimeExceeded(bytes) {
				err = errors.New("Time exceeded")
				rcvdChan <- Response{Addr: src.String(), TTL: ttl, Sequence: p.seq, Error: err}
				return
			}
		case IPv4ICMPTypeEchoReply, IPv6ICMPTypeEchoReply:
			if n >= 8 && p.isMyReply(bytes) {
				rtt := float64(time.Now().UnixNano()-getTimeStamp(bytes)) / 1000000
				rcvdChan <- Response{Addr: src.String(), TTL: ttl, Sequence: p.seq, RTT: rtt, Error: err}
				return
			}
		case IPv4ICMPTypeDestinationUnreachable:
			if n >= 28 && p.isMyDestUnreach(bytes) {
				err = errors.New(unreachableError(bytes))
				rcvdChan <- Response{Addr: src.String(), TTL: ttl, Sequence: p.seq, Error: err}
				return
			}

		default:
		}

		if time.Since(ts) < p.timeout {
			continue
		}

		err = errors.New("Request timeout")
		rcvdChan <- Response{Error: err}
		break
	}
}

func (p *Ping) send(conn *icmp.PacketConn) {
	var (
		wg sync.WaitGroup
	)
	var icmpType icmp.Type
	if IsIPv4(p.addr.IP) {
		icmpType = ipv4.ICMPTypeEcho
		conn.IPv4PacketConn().SetTTL(p.ttl)
		conn.IPv4PacketConn().SetControlMessage(ipv4.FlagTTL, true)
	} else {
		icmpType = ipv6.ICMPTypeEchoRequest
		conn.IPv6PacketConn().SetHopLimit(p.ttl)
		conn.IPv6PacketConn().SetControlMessage(ipv6.FlagHopLimit, true)
	}

	p.seq++
	bytes, err := (&icmp.Message{
		Type: icmpType, Code: 0,
		Body: &icmp.Echo{
			ID:   p.id,
			Seq:  p.seq,
			Data: p.payload(),
		},
	}).Marshal(nil)
	if err != nil {
		println(err.Error())
	}

	wg.Add(1)
	go func(conn *icmp.PacketConn, dest net.Addr, b []byte) {
		defer wg.Done()
		for {
			if _, err := conn.WriteTo(bytes, dest); err != nil {
				println(err.Error())
				if neterr, ok := err.(*net.OpError); ok {
					if neterr.Err == syscall.ENOBUFS {
						continue
					}
				}
			}
			break
		}
	}(conn, p.addr, bytes)

	wg.Wait()
}

func (p *Ping) payload() []byte {
	timeBytes := make([]byte, 8)
	ts := time.Now().UnixNano()
	for i := uint8(0); i < 8; i++ {
		timeBytes[i] = byte((ts >> (i * 8)) & 0xff)
	}
	payload := make([]byte, p.pSize-16)
	payload = append(payload, timeBytes...)
	return payload
}

func (p *Ping) parseMessage(m *packet) (*ipv4.Header, *icmp.Message, error) {
	var proto = ProtocolIPv4ICMP
	if !p.isV4Avail {
		proto = ProtocolIPv6ICMP
	}
	msg, err := icmp.ParseMessage(proto, m.bytes)
	if err != nil {
		return nil, nil, err
	}

	bytes, _ := msg.Body.Marshal(msg.Type.Protocol())
	h, err := icmp.ParseIPv4Header(bytes)
	return h, msg, err
}

// Ping tries to send and receive ICMP packets
func (p *Ping) Ping(out chan Response) {
	var (
		conn *icmp.PacketConn
		err  error
		addr string = p.addr.String()
	)

	if p.isV4Avail {
		if conn, err = p.listen("ip4:icmp"); err != nil {
			out <- Response{Error: err, Addr: addr}
			return
		}
		defer conn.Close()
	} else {
		if conn, err = p.listen("ip6:ipv6-icmp"); err != nil {
			out <- Response{Error: err, Addr: addr}
			return
		}
		defer conn.Close()
	}

	p.send(conn)
	p.recv(conn, out)
}

func (p *Ping) isMyTimeExceeded(bytes []byte) bool {
	n := 28
	respID := int(bytes[n+4])<<8 | int(bytes[n+5])
	respSq := int(bytes[n+6])<<8 | int(bytes[n+7])

	if p.id == respID && p.seq == respSq {
		return true
	}

	return false
}

func (p *Ping) isMyDestUnreach(bytes []byte) bool {
	n := 28
	respID := int(bytes[n+4])<<8 | int(bytes[n+5])
	respSq := int(bytes[n+6])<<8 | int(bytes[n+7])

	if p.id == respID && p.seq == respSq {
		return true
	}

	return false
}

func (p *Ping) isMyReply(bytes []byte) bool {
	respID := int(bytes[4])<<8 | int(bytes[5])
	respSq := int(bytes[6])<<8 | int(bytes[7])
	if respID == p.id && respSq == p.seq {
		return true
	}

	return false
}

func getTimeStamp(m []byte) int64 {
	var ts int64
	for i := uint(0); i < 8; i++ {
		ts += int64(m[uint(len(m))-8+i]) << (i * 8)
	}
	return ts
}

func unreachableError(bytes []byte) string {
	code := int(bytes[1])
	mtu := int(bytes[6])<<8 | int(bytes[7])
	var errors = []string{
		"Network unreachable",
		"Network unreachable",
		"Protocol unreachable",
		"Port unreachable",
		"The datagram is too big - next-hop MTU:" + string(mtu),
		"Source route failed",
		"Destination network unknown",
		"Destination host unknown",
		"Source host isolated",
		"The destination network is administratively prohibited",
		"The destination host is administratively prohibited",
		"The network is unreachable for Type Of Service",
		"The host is unreachable for Type Of Service",
		"Communication administratively prohibited",
		"Host precedence violation",
		"Precedence cutoff in effect",
	}

	return errors[code]
}
