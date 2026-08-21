package resolver

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// A minimal DNS client, used to interrogate a *running* resolver: "jm
// doctor" asks the process that serves the guest, over the wire (ADR 0008).
//
// It builds the packets itself rather than going through net.Resolver on
// purpose. The Go resolver answers /etc/hosts names out of this process,
// which is precisely the sort of name a parity probe is made of, and it
// expands the search list: either would produce an answer without the
// resolver under test ever being asked, which is the failure this whole
// check exists to catch.

// askTimeout bounds one query when the caller's context has no deadline.
const askTimeout = 5 * time.Second

// AskAddrs asks the resolver at addr for name's IPv4 addresses.
func AskAddrs(ctx context.Context, addr, name string) ([]netip.Addr, error) {
	answers, err := askResolver(ctx, addr, name, dnsmessage.TypeA)
	if err != nil {
		return nil, err
	}
	var out []netip.Addr
	for _, a := range answers {
		switch body := a.Body.(type) {
		case *dnsmessage.AResource:
			out = append(out, netip.AddrFrom4(body.A))
		case *dnsmessage.AAAAResource:
			out = append(out, netip.AddrFrom16(body.AAAA).Unmap())
		}
	}
	return out, nil
}

// AskMode asks the resolver at addr how it resolves names: ModeHost, the
// host operating system's own resolver, or ModeGo, which sees no scoped,
// hosts-file or .local name at all.
func AskMode(ctx context.Context, addr string) (string, error) {
	answers, err := askResolver(ctx, addr, StatusName, dnsmessage.TypeTXT)
	if err != nil {
		return "", err
	}
	var txt []string
	for _, a := range answers {
		if body, ok := a.Body.(*dnsmessage.TXTResource); ok {
			txt = append(txt, body.TXT...)
		}
	}
	mode, ok := ParseStatus(txt)
	if !ok {
		return "", fmt.Errorf("%s answered nothing that names a resolution path", StatusName)
	}
	return mode, nil
}

// askResolver puts one question to the resolver at addr and returns its
// answers, retrying over TCP when the reply did not fit a datagram.
func askResolver(ctx context.Context, addr, name string, typ dnsmessage.Type) ([]dnsmessage.Resource, error) {
	query, id, err := askQuery(name, typ)
	if err != nil {
		return nil, err
	}
	reply, err := askUDP(ctx, addr, query, id)
	if err != nil {
		return nil, err
	}
	answers, truncated, err := parseReply(reply, id, name)
	if err != nil || !truncated {
		return answers, err
	}
	if reply, err = askTCP(ctx, addr, query); err != nil {
		return nil, err
	}
	answers, _, err = parseReply(reply, id, name)
	return answers, err
}

// askQuery renders one question, with an EDNS(0) record so a large answer
// arrives without a second round trip.
func askQuery(name string, typ dnsmessage.Type) ([]byte, uint16, error) {
	n, err := dnsmessage.NewName(canonical(name))
	if err != nil {
		return nil, 0, fmt.Errorf("resolver: %q is not a name: %w", name, err)
	}
	var seed [2]byte
	if _, err := rand.Read(seed[:]); err != nil {
		return nil, 0, err
	}
	id := binary.BigEndian.Uint16(seed[:])
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: id, RecursionDesired: true})
	b.EnableCompression()
	if err := b.StartQuestions(); err != nil {
		return nil, 0, err
	}
	if err := b.Question(dnsmessage.Question{Name: n, Type: typ, Class: dnsmessage.ClassINET}); err != nil {
		return nil, 0, err
	}
	if err := b.StartAdditionals(); err != nil {
		return nil, 0, err
	}
	var opt dnsmessage.ResourceHeader
	if err := opt.SetEDNS0(MaxUDPSize, dnsmessage.RCodeSuccess, false); err != nil {
		return nil, 0, err
	}
	if err := b.OPTResource(opt, dnsmessage.OPTResource{}); err != nil {
		return nil, 0, err
	}
	msg, err := b.Finish()
	return msg, id, err
}

func askUDP(ctx context.Context, addr string, query []byte, id uint16) ([]byte, error) {
	conn, deadline, err := askDial(ctx, "udp", addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if _, err := conn.Write(query); err != nil {
		return nil, err
	}
	buf := make([]byte, MaxUDPSize)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return nil, err
		}
		// A datagram from somewhere else, or a late duplicate, is not an
		// answer to this question.
		if n >= 2 && binary.BigEndian.Uint16(buf[:2]) == id {
			return buf[:n], nil
		}
		if !time.Now().Before(deadline) {
			return nil, errors.New("resolver: no reply matching the query")
		}
	}
}

func askTCP(ctx context.Context, addr string, query []byte) ([]byte, error) {
	conn, _, err := askDial(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	framed := make([]byte, 2+len(query))
	binary.BigEndian.PutUint16(framed, uint16(len(query)))
	copy(framed[2:], query)
	if _, err := conn.Write(framed); err != nil {
		return nil, err
	}
	var length [2]byte
	if _, err := readFull(conn, length[:]); err != nil {
		return nil, err
	}
	reply := make([]byte, binary.BigEndian.Uint16(length[:]))
	if _, err := readFull(conn, reply); err != nil {
		return nil, err
	}
	return reply, nil
}

// askDial connects to addr and gives the connection the caller's deadline.
func askDial(ctx context.Context, network, addr string) (net.Conn, time.Time, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(askTimeout)
	}
	conn, err := (&net.Dialer{Deadline: deadline}).DialContext(ctx, network, addr)
	if err != nil {
		return nil, deadline, err
	}
	if err := conn.SetDeadline(deadline); err != nil {
		_ = conn.Close()
		return nil, deadline, err
	}
	return conn, deadline, nil
}

func readFull(conn net.Conn, buf []byte) (int, error) {
	read := 0
	for read < len(buf) {
		n, err := conn.Read(buf[read:])
		read += n
		if err != nil {
			return read, err
		}
	}
	return read, nil
}

// parseReply checks the reply belongs to the query and returns its answer
// section, plus whether the server truncated it. A response code other than
// success is an error, "no such host" among them, so callers can tell a
// missing name from a broken resolver exactly as net.Resolver does.
func parseReply(msg []byte, id uint16, name string) ([]dnsmessage.Resource, bool, error) {
	var p dnsmessage.Parser
	hdr, err := p.Start(msg)
	if err != nil {
		return nil, false, fmt.Errorf("resolver: unparsable reply: %w", err)
	}
	if hdr.ID != id || !hdr.Response {
		return nil, false, errors.New("resolver: reply does not answer the query")
	}
	if err := p.SkipAllQuestions(); err != nil {
		return nil, hdr.Truncated, err
	}
	if hdr.RCode != dnsmessage.RCodeSuccess {
		return nil, hdr.Truncated, &net.DNSError{
			Err:        rcodeError(hdr.RCode),
			Name:       name,
			IsNotFound: hdr.RCode == dnsmessage.RCodeNameError,
		}
	}
	answers, err := p.AllAnswers()
	if err != nil && !errors.Is(err, dnsmessage.ErrSectionDone) {
		return nil, hdr.Truncated, err
	}
	return answers, hdr.Truncated, nil
}

func rcodeError(rcode dnsmessage.RCode) string {
	if rcode == dnsmessage.RCodeNameError {
		return "no such host"
	}
	return fmt.Sprintf("the resolver answered %v", rcode)
}
