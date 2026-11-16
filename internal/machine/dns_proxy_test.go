package machine

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/gfdelarue/kiln-vm/internal/config"
)

var dnsProxyFailureQuery = []byte{
	0x6b, 0x69, // transaction ID
	0x01, 0x00, // recursion desired
	0x00, 0x01, // one question
	0x00, 0x00, // no answers
	0x00, 0x00, // no authority records
	0x00, 0x00, // no additional records
	0x07, 'e', 'x', 'a', 'm', 'p', 'l', 'e',
	0x03, 'c', 'o', 'm',
	0x00,
	0x00, 0x01, // A
	0x00, 0x01, // IN
}

func TestDNSProxyForwardsUDPToUpstream(t *testing.T) {
	upstream, err := net.ListenUDP("udp4", mustResolveUDPAddr(t, "127.0.0.1:0"))
	if err != nil {
		t.Fatalf("listen upstream UDP: %v", err)
	}
	defer upstream.Close()

	query := append([]byte(nil), dnsProxyFailureQuery...)
	response := validDNSResponseForTest(query)
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 512)
		_ = upstream.SetDeadline(time.Now().Add(2 * time.Second))
		n, client, err := upstream.ReadFromUDP(buf)
		if err != nil {
			t.Errorf("read upstream UDP: %v", err)
			return
		}
		if !reflect.DeepEqual(buf[:n], query) {
			t.Errorf("upstream UDP query = %#v, want %#v", buf[:n], query)
			return
		}
		if _, err := upstream.WriteToUDP(response, client); err != nil {
			t.Errorf("write upstream UDP response: %v", err)
		}
	}()

	proxy := startProxyOnFreePort(t, []string{upstream.LocalAddr().String()})
	defer proxy.Close()

	client, err := net.Dial("udp4", proxy.udp.LocalAddr().String())
	if err != nil {
		t.Fatalf("dial proxy UDP: %v", err)
	}
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := client.Write(query); err != nil {
		t.Fatalf("write proxy UDP query: %v", err)
	}
	buf := make([]byte, 512)
	n, err := client.Read(buf)
	if err != nil {
		t.Fatalf("read proxy UDP response: %v", err)
	}
	if !reflect.DeepEqual(buf[:n], response) {
		t.Fatalf("proxy UDP response = %#v, want %#v", buf[:n], response)
	}
	<-done
}

func TestDNSProxyForwardsTCPFrameToUpstream(t *testing.T) {
	upstream, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen upstream TCP: %v", err)
	}
	defer upstream.Close()

	query := append([]byte(nil), dnsProxyFailureQuery...)
	response := validDNSResponseForTest(query)
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := upstream.Accept()
		if err != nil {
			t.Errorf("accept upstream TCP: %v", err)
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		got, err := readDNSFrame(conn)
		if err != nil {
			t.Errorf("read upstream TCP query: %v", err)
			return
		}
		if !reflect.DeepEqual(got, query) {
			t.Errorf("upstream TCP query = %#v, want %#v", got, query)
			return
		}
		if err := writeDNSFrame(conn, response); err != nil {
			t.Errorf("write upstream TCP response: %v", err)
		}
	}()

	proxy := startProxyOnFreePort(t, []string{upstream.Addr().String()})
	defer proxy.Close()

	client, err := net.Dial("tcp4", proxy.tcp.Addr().String())
	if err != nil {
		t.Fatalf("dial proxy TCP: %v", err)
	}
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(2 * time.Second))
	if err := writeDNSFrame(client, query); err != nil {
		t.Fatalf("write proxy TCP query: %v", err)
	}
	got, err := readDNSFrame(client)
	if err != nil {
		t.Fatalf("read proxy TCP response: %v", err)
	}
	if !reflect.DeepEqual(got, response) {
		t.Fatalf("proxy TCP response = %#v, want %#v", got, response)
	}
	<-done
}

func TestDNSProxyForwardsEDNSQueryAndAcceptsCaseEquivalentQuestion(t *testing.T) {
	queryMessage := new(dns.Msg)
	queryMessage.Id = 0x4544
	queryMessage.RecursionDesired = true
	queryMessage.Question = []dns.Question{{Name: "ExAmPlE.com.", Qtype: dns.TypeAAAA, Qclass: dns.ClassINET}}
	queryMessage.SetEdns0(1232, true)
	query, err := queryMessage.Pack()
	if err != nil {
		t.Fatalf("pack EDNS query: %v", err)
	}
	responseMessage := new(dns.Msg)
	responseMessage.SetReply(queryMessage)
	responseMessage.Question[0].Name = "EXAMPLE.COM."
	responseMessage.RecursionAvailable = true
	responseMessage.SetEdns0(1232, true)
	response, err := responseMessage.Pack()
	if err != nil {
		t.Fatalf("pack EDNS response: %v", err)
	}

	upstream, done := startOneShotUDPUpstreamForTest(t, query, response)
	defer upstream.Close()
	got, err := exchangeThroughProxyUDPForTest(t, query, []string{upstream.LocalAddr().String()})
	if err != nil {
		t.Fatalf("exchange EDNS query: %v", err)
	}
	if !bytes.Equal(got, response) {
		t.Fatalf("EDNS response changed: got %x want %x", got, response)
	}
	if err := <-done; err != nil {
		t.Fatalf("EDNS upstream: %v", err)
	}
}

func TestDNSProxyReturnsSERVFAILOverUDPWhenAllUpstreamsFail(t *testing.T) {
	upstream := closedLocalUDPAddress(t)
	proxy := startProxyOnFreePort(t, []string{upstream})
	defer proxy.Close()

	client, err := net.Dial("udp4", proxy.udp.LocalAddr().String())
	if err != nil {
		t.Fatalf("dial proxy UDP: %v", err)
	}
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := client.Write(dnsProxyFailureQuery); err != nil {
		t.Fatalf("write proxy UDP query: %v", err)
	}

	response := make([]byte, 512)
	n, err := client.Read(response)
	if err != nil {
		t.Fatalf("read proxy UDP SERVFAIL response: %v", err)
	}
	requireDNSSERVFAIL(t, dnsProxyFailureQuery, response[:n])
}

func TestDNSProxyReturnsSERVFAILOverTCPWhenAllUpstreamsFail(t *testing.T) {
	upstream := net.JoinHostPort("127.0.0.1", reserveLocalTCPPort(t))
	proxy := startProxyOnFreePort(t, []string{upstream})
	defer proxy.Close()

	client, err := net.Dial("tcp4", proxy.tcp.Addr().String())
	if err != nil {
		t.Fatalf("dial proxy TCP: %v", err)
	}
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(2 * time.Second))
	if err := writeDNSFrame(client, dnsProxyFailureQuery); err != nil {
		t.Fatalf("write proxy TCP query: %v", err)
	}

	response, err := readDNSFrame(client)
	if err != nil {
		t.Fatalf("read proxy TCP SERVFAIL response: %v", err)
	}
	requireDNSSERVFAIL(t, dnsProxyFailureQuery, response)
}

func TestDNSProxyUDPRejectsInvalidUpstreamResponsesAndFailsOver(t *testing.T) {
	validResponse := validDNSResponseForTest(dnsProxyFailureQuery)
	for _, test := range invalidDNSResponsesForTest(dnsProxyFailureQuery) {
		t.Run(test.name, func(t *testing.T) {
			invalidUpstream, invalidDone := startOneShotUDPUpstreamForTest(t, dnsProxyFailureQuery, test.response)
			defer invalidUpstream.Close()
			validUpstream, validDone := startOneShotUDPUpstreamForTest(t, dnsProxyFailureQuery, validResponse)
			defer validUpstream.Close()

			got, err := exchangeThroughProxyUDPForTest(t, dnsProxyFailureQuery, []string{
				invalidUpstream.LocalAddr().String(),
				validUpstream.LocalAddr().String(),
			})
			if err != nil {
				t.Fatalf("exchange DNS over UDP: %v", err)
			}
			if err := <-invalidDone; err != nil {
				t.Fatalf("invalid UDP upstream: %v", err)
			}
			if !bytes.Equal(got, validResponse) {
				t.Fatalf("proxy accepted invalid UDP response %x instead of failing over to %x", got, validResponse)
			}
			if err := <-validDone; err != nil {
				t.Fatalf("valid UDP upstream: %v", err)
			}
		})
	}
}

func TestDNSProxyTCPRejectsInvalidUpstreamResponsesAndFailsOver(t *testing.T) {
	validResponse := validDNSResponseForTest(dnsProxyFailureQuery)
	for _, test := range invalidDNSResponsesForTest(dnsProxyFailureQuery) {
		t.Run(test.name, func(t *testing.T) {
			invalidUpstream, invalidDone := startOneShotTCPUpstreamForTest(t, dnsProxyFailureQuery, test.response)
			defer invalidUpstream.Close()
			validUpstream, validDone := startOneShotTCPUpstreamForTest(t, dnsProxyFailureQuery, validResponse)
			defer validUpstream.Close()

			got, err := exchangeThroughProxyTCPForTest(t, dnsProxyFailureQuery, []string{
				invalidUpstream.Addr().String(),
				validUpstream.Addr().String(),
			})
			if err != nil {
				t.Fatalf("exchange DNS over TCP: %v", err)
			}
			if err := <-invalidDone; err != nil {
				t.Fatalf("invalid TCP upstream: %v", err)
			}
			if !bytes.Equal(got, validResponse) {
				t.Fatalf("proxy accepted invalid TCP response %x instead of failing over to %x", got, validResponse)
			}
			if err := <-validDone; err != nil {
				t.Fatalf("valid TCP upstream: %v", err)
			}
		})
	}
}

type invalidDNSResponseForTest struct {
	name     string
	response []byte
}

func invalidDNSResponsesForTest(query []byte) []invalidDNSResponseForTest {
	missingQR := append([]byte(nil), query...)
	wrongTransaction := validDNSResponseForTest(query)
	wrongTransaction[0] ^= 0xff
	wrongOpcode := validDNSResponseForTest(query)
	wrongOpcode[2] |= byte(dns.OpcodeStatus << 3)
	wrongQuestion := validDNSResponseForTest(query)
	wrongQuestion[13] = 'x'
	malformedAnswer := validDNSResponseForTest(query)
	binary.BigEndian.PutUint16(malformedAnswer[6:8], 1)
	trailingGarbage := append(validDNSResponseForTest(query), 0xde, 0xad)
	reservedFlag := validDNSResponseForTest(query)
	reservedFlag[3] |= 0x40
	return []invalidDNSResponseForTest{
		{name: "short_header", response: []byte{query[0], query[1], 0x81}},
		{name: "missing_response_bit", response: missingQR},
		{name: "wrong_transaction_id", response: wrongTransaction},
		{name: "wrong_opcode", response: wrongOpcode},
		{name: "wrong_question", response: wrongQuestion},
		{name: "malformed_answer", response: malformedAnswer},
		{name: "trailing_garbage", response: trailingGarbage},
		{name: "reserved_flag", response: reservedFlag},
	}
}

func validDNSResponseForTest(query []byte) []byte {
	response := append([]byte(nil), query...)
	response[2] = 0x81
	response[3] = 0x80
	return response
}

func TestDNSProxyPreservesValidNegativePolicyAndTruncatedResponses(t *testing.T) {
	tests := []struct {
		name      string
		rcode     int
		truncated bool
	}{
		{name: "nxdomain", rcode: dns.RcodeNameError},
		{name: "refused", rcode: dns.RcodeRefused},
		{name: "upstream_servfail", rcode: dns.RcodeServerFailure},
		{name: "truncated", rcode: dns.RcodeSuccess, truncated: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := dnsResponseForTest(t, dnsProxyFailureQuery, test.rcode, test.truncated)
			t.Run("udp", func(t *testing.T) {
				upstream, done := startOneShotUDPUpstreamForTest(t, dnsProxyFailureQuery, response)
				defer upstream.Close()
				got, err := exchangeThroughProxyUDPForTest(t, dnsProxyFailureQuery, []string{upstream.LocalAddr().String()})
				if err != nil {
					t.Fatalf("exchange DNS over UDP: %v", err)
				}
				if !bytes.Equal(got, response) {
					t.Fatalf("UDP response changed: got %x want %x", got, response)
				}
				if err := <-done; err != nil {
					t.Fatalf("UDP upstream: %v", err)
				}
			})
			t.Run("tcp", func(t *testing.T) {
				upstream, done := startOneShotTCPUpstreamForTest(t, dnsProxyFailureQuery, response)
				defer upstream.Close()
				got, err := exchangeThroughProxyTCPForTest(t, dnsProxyFailureQuery, []string{upstream.Addr().String()})
				if err != nil {
					t.Fatalf("exchange DNS over TCP: %v", err)
				}
				if !bytes.Equal(got, response) {
					t.Fatalf("TCP response changed: got %x want %x", got, response)
				}
				if err := <-done; err != nil {
					t.Fatalf("TCP upstream: %v", err)
				}
			})
		})
	}
}

func dnsResponseForTest(t *testing.T, queryWire []byte, rcode int, truncated bool) []byte {
	t.Helper()
	var query dns.Msg
	if err := query.Unpack(queryWire); err != nil {
		t.Fatalf("unpack test query: %v", err)
	}
	response := new(dns.Msg)
	response.SetReply(&query)
	response.RecursionAvailable = true
	response.Rcode = rcode
	response.Truncated = truncated
	wire, err := response.Pack()
	if err != nil {
		t.Fatalf("pack test response: %v", err)
	}
	return wire
}

func startOneShotUDPUpstreamForTest(t *testing.T, query, response []byte) (*net.UDPConn, <-chan error) {
	t.Helper()
	upstream, err := net.ListenUDP("udp4", mustResolveUDPAddr(t, "127.0.0.1:0"))
	if err != nil {
		t.Fatalf("listen one-shot UDP upstream: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 512)
		_ = upstream.SetDeadline(time.Now().Add(2 * time.Second))
		n, client, err := upstream.ReadFromUDP(buf)
		if err != nil {
			done <- fmt.Errorf("read query: %w", err)
			return
		}
		if !bytes.Equal(buf[:n], query) {
			done <- fmt.Errorf("query = %x, want %x", buf[:n], query)
			return
		}
		if _, err := upstream.WriteToUDP(response, client); err != nil {
			done <- fmt.Errorf("write response: %w", err)
			return
		}
		done <- nil
	}()
	return upstream, done
}

func startOneShotTCPUpstreamForTest(t *testing.T, query, response []byte) (net.Listener, <-chan error) {
	t.Helper()
	upstream, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen one-shot TCP upstream: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		conn, err := upstream.Accept()
		if err != nil {
			done <- fmt.Errorf("accept connection: %w", err)
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		got, err := readDNSFrame(conn)
		if err != nil {
			done <- fmt.Errorf("read query: %w", err)
			return
		}
		if !bytes.Equal(got, query) {
			done <- fmt.Errorf("query = %x, want %x", got, query)
			return
		}
		if err := writeDNSFrame(conn, response); err != nil {
			done <- fmt.Errorf("write response: %w", err)
			return
		}
		done <- nil
	}()
	return upstream, done
}

func requireDNSSERVFAIL(t *testing.T, query, response []byte) {
	t.Helper()
	if len(query) < 12 {
		t.Fatalf("test query is shorter than a DNS header: %d", len(query))
	}
	if len(response) < 12 {
		t.Fatalf("DNS response is shorter than a header: %d", len(response))
	}
	if response[0] != query[0] || response[1] != query[1] {
		t.Fatalf("DNS response transaction ID = %x, want %x", response[:2], query[:2])
	}
	if response[2]&0x80 == 0 {
		t.Fatalf("DNS response does not set QR: flags=%x", response[2:4])
	}
	if got := response[3] & 0x0f; got != 2 {
		t.Fatalf("DNS response RCODE = %d, want SERVFAIL (2): flags=%x", got, response[2:4])
	}
	if !bytes.Equal(response[4:6], query[4:6]) {
		t.Fatalf("DNS response question count = %x, want %x", response[4:6], query[4:6])
	}
	if !bytes.Equal(response[6:12], []byte{0, 0, 0, 0, 0, 0}) {
		t.Fatalf("DNS SERVFAIL answer/authority/additional counts = %x, want all zero", response[6:12])
	}
	if !bytes.Equal(response[12:], query[12:]) {
		t.Fatalf("DNS SERVFAIL question = %x, want original question %x", response[12:], query[12:])
	}
}

func requireDNSRcode(t *testing.T, response []byte, rcode int) *dns.Msg {
	t.Helper()
	var message dns.Msg
	if err := message.Unpack(response); err != nil {
		t.Fatalf("unpack DNS response: %v", err)
	}
	if !message.Response {
		t.Fatalf("DNS response does not set QR")
	}
	if message.Rcode != rcode {
		t.Fatalf("DNS response RCODE = %d, want %d", message.Rcode, rcode)
	}
	return &message
}

func TestParseDNSQueryRejectsMalformedQueries(t *testing.T) {
	mutate := func(update func([]byte)) []byte {
		wire := append([]byte(nil), dnsProxyFailureQuery...)
		update(wire)
		return wire
	}
	tests := []struct {
		name string
		wire []byte
	}{
		{name: "short_header", wire: []byte{0x12, 0x34, 0x01}},
		{name: "response_bit", wire: mutate(func(wire []byte) { wire[2] |= 0x80 })},
		{name: "non_query_opcode", wire: mutate(func(wire []byte) { wire[2] |= byte(dns.OpcodeUpdate << 3) })},
		{name: "zero_questions", wire: mutate(func(wire []byte) { binary.BigEndian.PutUint16(wire[4:6], 0) })},
		{name: "truncated_question", wire: dnsProxyFailureQuery[:len(dnsProxyFailureQuery)-1]},
		{name: "response_only_flag", wire: mutate(func(wire []byte) { wire[3] |= 0x80 })},
		{name: "reserved_question_type", wire: mutate(func(wire []byte) { binary.BigEndian.PutUint16(wire[len(wire)-4:len(wire)-2], 0) })},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseDNSQuery(test.wire); err == nil {
				t.Fatalf("parseDNSQuery accepted malformed query %x", test.wire)
			}
		})
	}
}

func TestDNSProxyReturnsFORMERROverUDPAndTCPForMalformedQuery(t *testing.T) {
	malformed := append([]byte(nil), dnsProxyFailureQuery...)
	malformed[2] |= 0x80
	upstream := closedLocalUDPAddress(t)
	proxy := startProxyOnFreePort(t, []string{upstream})
	defer proxy.Close()

	t.Run("udp", func(t *testing.T) {
		client, err := net.Dial("udp4", proxy.udp.LocalAddr().String())
		if err != nil {
			t.Fatalf("dial proxy UDP: %v", err)
		}
		defer client.Close()
		_ = client.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := client.Write(malformed); err != nil {
			t.Fatalf("write malformed UDP query: %v", err)
		}
		response := make([]byte, 512)
		n, err := client.Read(response)
		if err != nil {
			t.Fatalf("read UDP FORMERR: %v", err)
		}
		message := requireDNSRcode(t, response[:n], dns.RcodeFormatError)
		if message.Id != binary.BigEndian.Uint16(malformed[:2]) {
			t.Fatalf("FORMERR ID = %d, want %d", message.Id, binary.BigEndian.Uint16(malformed[:2]))
		}
	})

	t.Run("tcp", func(t *testing.T) {
		client, err := net.Dial("tcp4", proxy.tcp.Addr().String())
		if err != nil {
			t.Fatalf("dial proxy TCP: %v", err)
		}
		defer client.Close()
		_ = client.SetDeadline(time.Now().Add(2 * time.Second))
		if err := writeDNSFrame(client, malformed); err != nil {
			t.Fatalf("write malformed TCP query: %v", err)
		}
		response, err := readDNSFrame(client)
		if err != nil {
			t.Fatalf("read TCP FORMERR: %v", err)
		}
		requireDNSRcode(t, response, dns.RcodeFormatError)
	})

	status := proxy.Status()
	if status.Queries != 2 || status.LocalFormatErrors != 2 {
		t.Fatalf("status query/FORMERR counts = %d/%d, want 2/2", status.Queries, status.LocalFormatErrors)
	}
	if status.LastErrorKind != DNSProxyErrorMalformedQuery {
		t.Fatalf("last error kind = %q, want %q", status.LastErrorKind, DNSProxyErrorMalformedQuery)
	}
	if strings.Contains(strings.ToLower(status.LastErrorKind), "example") {
		t.Fatalf("status leaked qname: %#v", status)
	}
}

func TestDNSProxyExchangeUsesOneTotalDeadlineAcrossUpstreams(t *testing.T) {
	first, firstReceived := startBlackholeUDPUpstreamForTest(t)
	defer first.Close()
	second, secondReceived := startBlackholeUDPUpstreamForTest(t)
	defer second.Close()
	query, err := parseDNSQuery(dnsProxyFailureQuery)
	if err != nil {
		t.Fatalf("parse query: %v", err)
	}

	const total = 240 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), total)
	defer cancel()
	started := time.Now()
	_, err = exchangeDNSContext(ctx, "udp4", dnsProxyFailureQuery, query, []string{
		first.LocalAddr().String(), second.LocalAddr().String(),
	}, nil)
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("exchange unexpectedly succeeded")
	}
	if elapsed > 600*time.Millisecond {
		t.Fatalf("exchange elapsed %s, exceeded one bounded total deadline", elapsed)
	}
	if dnsProxyQueryTimeout > 4*time.Second {
		t.Fatalf("default query timeout = %s, want <=4s", dnsProxyQueryTimeout)
	}
	waitForUDPReceipt(t, firstReceived, "first")
	waitForUDPReceipt(t, secondReceived, "second")
}

func TestDNSProxyCloseCancelsInflightUDPExchange(t *testing.T) {
	upstream, received := startBlackholeUDPUpstreamForTest(t)
	defer upstream.Close()
	proxy := startProxyOnFreePort(t, []string{upstream.LocalAddr().String()})
	client, err := net.Dial("udp4", proxy.udp.LocalAddr().String())
	if err != nil {
		t.Fatalf("dial proxy UDP: %v", err)
	}
	defer client.Close()
	if _, err := client.Write(dnsProxyFailureQuery); err != nil {
		t.Fatalf("write proxy UDP query: %v", err)
	}
	waitForUDPReceipt(t, received, "in-flight")

	started := time.Now()
	if err := proxy.Close(); err != nil {
		t.Fatalf("close proxy: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("proxy close waited %s for a cancelled exchange", elapsed)
	}
	status := proxy.Status()
	if status.Running || status.InFlight != 0 {
		t.Fatalf("closed proxy status = %#v, want stopped with no in-flight queries", status)
	}
	if err := proxy.Close(); err != nil {
		t.Fatalf("second proxy close: %v", err)
	}
}

func TestDNSProxyCloseCancelsInflightTCPExchange(t *testing.T) {
	upstream, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen TCP upstream: %v", err)
	}
	defer upstream.Close()
	received := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	go func() {
		conn, err := upstream.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		if _, err := readDNSFrame(conn); err != nil {
			return
		}
		close(received)
		<-release
	}()

	proxy := startProxyOnFreePort(t, []string{upstream.Addr().String()})
	client, err := net.Dial("tcp4", proxy.tcp.Addr().String())
	if err != nil {
		t.Fatalf("dial proxy TCP: %v", err)
	}
	defer client.Close()
	if err := writeDNSFrame(client, dnsProxyFailureQuery); err != nil {
		t.Fatalf("write proxy TCP query: %v", err)
	}
	select {
	case <-received:
	case <-time.After(time.Second):
		t.Fatal("TCP upstream did not receive in-flight query")
	}

	started := time.Now()
	if err := proxy.Close(); err != nil {
		t.Fatalf("close proxy: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("proxy close waited %s for a cancelled TCP exchange", elapsed)
	}
	status := proxy.Status()
	if status.Running || status.InFlight != 0 {
		t.Fatalf("closed proxy status = %#v, want stopped with no in-flight queries", status)
	}
}

func TestDNSProxyRejectsExcessUDPWorkWithImmediateSERVFAIL(t *testing.T) {
	upstream, received := startBlackholeUDPUpstreamForTest(t)
	defer upstream.Close()
	proxy := startProxyOnFreePort(t, []string{upstream.LocalAddr().String()})
	defer proxy.Close()
	for index := 0; index < cap(proxy.querySlots)-1; index++ {
		proxy.querySlots <- struct{}{}
	}

	first, err := net.Dial("udp4", proxy.udp.LocalAddr().String())
	if err != nil {
		t.Fatalf("dial first UDP client: %v", err)
	}
	defer first.Close()
	if _, err := first.Write(dnsProxyFailureQuery); err != nil {
		t.Fatalf("write first query: %v", err)
	}
	waitForUDPReceipt(t, received, "first saturated")

	second, err := net.Dial("udp4", proxy.udp.LocalAddr().String())
	if err != nil {
		t.Fatalf("dial second UDP client: %v", err)
	}
	defer second.Close()
	_ = second.SetDeadline(time.Now().Add(500 * time.Millisecond))
	if _, err := second.Write(dnsProxyFailureQuery); err != nil {
		t.Fatalf("write second query: %v", err)
	}
	response := make([]byte, 512)
	n, err := second.Read(response)
	if err != nil {
		t.Fatalf("read capacity SERVFAIL: %v", err)
	}
	requireDNSSERVFAIL(t, dnsProxyFailureQuery, response[:n])
	status := proxy.Status()
	if status.RejectedQueries != 1 || status.LocalServerFailures != 1 || status.LastErrorKind != DNSProxyErrorCapacity {
		t.Fatalf("capacity status = %#v", status)
	}
}

func TestDNSProxyManagerWideLimitRejectsExcessWorkAcrossAllocations(t *testing.T) {
	upstream, received := startBlackholeUDPUpstreamForTest(t)
	defer upstream.Close()
	globalSlots := make(chan struct{}, 1)
	first := startProxyWithGlobalOnFreePort(t, []string{upstream.LocalAddr().String()}, globalSlots)
	defer first.Close()
	second := startProxyWithGlobalOnFreePort(t, []string{upstream.LocalAddr().String()}, globalSlots)
	defer second.Close()

	firstClient, err := net.Dial("udp4", first.udp.LocalAddr().String())
	if err != nil {
		t.Fatalf("dial first proxy: %v", err)
	}
	defer firstClient.Close()
	if _, err := firstClient.Write(dnsProxyFailureQuery); err != nil {
		t.Fatalf("write first query: %v", err)
	}
	waitForUDPReceipt(t, received, "global-limit first")

	secondClient, err := net.Dial("udp4", second.udp.LocalAddr().String())
	if err != nil {
		t.Fatalf("dial second proxy: %v", err)
	}
	defer secondClient.Close()
	_ = secondClient.SetDeadline(time.Now().Add(500 * time.Millisecond))
	if _, err := secondClient.Write(dnsProxyFailureQuery); err != nil {
		t.Fatalf("write second query: %v", err)
	}
	response := make([]byte, 512)
	n, err := secondClient.Read(response)
	if err != nil {
		t.Fatalf("read global capacity SERVFAIL: %v", err)
	}
	requireDNSSERVFAIL(t, dnsProxyFailureQuery, response[:n])
	status := second.Status()
	if status.RejectedQueries != 1 || status.LastErrorKind != DNSProxyErrorCapacity {
		t.Fatalf("second proxy global-capacity status = %#v", status)
	}
}

func TestDNSProxySupportsMultipleTCPFramesPerConnection(t *testing.T) {
	upstream, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen TCP upstream: %v", err)
	}
	defer upstream.Close()
	upstreamDone := make(chan error, 1)
	go func() {
		for index := 0; index < 2; index++ {
			conn, err := upstream.Accept()
			if err != nil {
				upstreamDone <- err
				return
			}
			query, err := readDNSFrame(conn)
			if err == nil {
				err = writeDNSFrame(conn, validDNSResponseForTest(query))
			}
			_ = conn.Close()
			if err != nil {
				upstreamDone <- err
				return
			}
		}
		upstreamDone <- nil
	}()

	proxy := startProxyOnFreePort(t, []string{upstream.Addr().String()})
	defer proxy.Close()
	client, err := net.Dial("tcp4", proxy.tcp.Addr().String())
	if err != nil {
		t.Fatalf("dial proxy TCP: %v", err)
	}
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(2 * time.Second))
	for index := 0; index < 2; index++ {
		query := append([]byte(nil), dnsProxyFailureQuery...)
		query[1] += byte(index)
		if err := writeDNSFrame(client, query); err != nil {
			t.Fatalf("write query %d: %v", index, err)
		}
		response, err := readDNSFrame(client)
		if err != nil {
			t.Fatalf("read response %d: %v", index, err)
		}
		if !bytes.Equal(response, validDNSResponseForTest(query)) {
			t.Fatalf("response %d = %x", index, response)
		}
	}
	if err := <-upstreamDone; err != nil {
		t.Fatalf("TCP upstream: %v", err)
	}
	status := proxy.Status()
	if status.Queries != 2 || status.Successes != 2 || status.InFlight != 0 {
		t.Fatalf("multi-frame status = %#v", status)
	}
}

func TestDNSProxyStatusClassifiesInvalidUpstreamWithoutQueryData(t *testing.T) {
	invalidResponse := validDNSResponseForTest(dnsProxyFailureQuery)
	invalidResponse[0] ^= 0xff
	invalidUpstream, invalidDone := startOneShotUDPUpstreamForTest(t, dnsProxyFailureQuery, invalidResponse)
	defer invalidUpstream.Close()
	validResponse := dnsResponseForTest(t, dnsProxyFailureQuery, dns.RcodeServerFailure, false)
	validUpstream, validDone := startOneShotUDPUpstreamForTest(t, dnsProxyFailureQuery, validResponse)
	defer validUpstream.Close()
	proxy := startProxyOnFreePort(t, []string{invalidUpstream.LocalAddr().String(), validUpstream.LocalAddr().String()})
	defer proxy.Close()

	client, err := net.Dial("udp4", proxy.udp.LocalAddr().String())
	if err != nil {
		t.Fatalf("dial proxy UDP: %v", err)
	}
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := client.Write(dnsProxyFailureQuery); err != nil {
		t.Fatalf("write query: %v", err)
	}
	response := make([]byte, 512)
	n, err := client.Read(response)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if !bytes.Equal(response[:n], validResponse) {
		t.Fatalf("response = %x, want %x", response[:n], validResponse)
	}
	if err := <-invalidDone; err != nil {
		t.Fatalf("invalid upstream: %v", err)
	}
	if err := <-validDone; err != nil {
		t.Fatalf("valid upstream: %v", err)
	}
	status := proxy.Status()
	if status.Queries != 1 || status.Successes != 1 || status.InvalidUpstreamResponses != 1 {
		t.Fatalf("status = %#v", status)
	}
	if status.LocalServerFailures != 0 || status.LastResponseRcode != dns.RcodeServerFailure || status.LastResponseAt.IsZero() {
		t.Fatalf("upstream SERVFAIL classification = %#v", status)
	}
	if status.LastErrorKind != DNSProxyErrorInvalidUpstreamResponse {
		t.Fatalf("last error kind = %q, want %q", status.LastErrorKind, DNSProxyErrorInvalidUpstreamResponse)
	}
	serialized := fmt.Sprintf("%#v", status)
	if strings.Contains(strings.ToLower(serialized), "example") {
		t.Fatalf("status leaked qname: %s", serialized)
	}
}

func TestDNSProxyManagerStopIsValidatedAndIdempotent(t *testing.T) {
	disabled := NewDNSProxyManager(false, defaultBaseCIDR, nil)
	if err := disabled.Stop(""); err != nil {
		t.Fatalf("disabled manager Stop = %v, want nil", err)
	}
	enabled := NewDNSProxyManager(true, defaultBaseCIDR, []string{"127.0.0.1"})
	if err := enabled.Stop("127.0.0.2"); err != nil {
		t.Fatalf("absent valid gateway Stop = %v, want nil", err)
	}
	if err := enabled.Stop(""); err == nil {
		t.Fatal("enabled manager accepted empty gateway Stop")
	}
	if err := enabled.Close(); err != nil {
		t.Fatalf("close empty manager: %v", err)
	}
	if err := enabled.Close(); err != nil {
		t.Fatalf("second manager close: %v", err)
	}
	if err := enabled.Start("127.0.0.2"); err == nil || !strings.Contains(err.Error(), "manager is closed") {
		t.Fatalf("start after manager close = %v, want closed error", err)
	}
}

func TestDNSProxyManagerStartRejectsBridgeOverlappingUpstreamsBeforeMutation(t *testing.T) {
	tests := []struct {
		name      string
		gateway   string
		upstreams []string
	}{
		{
			name:      "allocated gateway would recurse into itself",
			gateway:   "172.16.0.1",
			upstreams: []string{"172.16.0.1"},
		},
		{
			name:      "another allocation-domain address could route into a VM",
			gateway:   "172.16.0.1",
			upstreams: []string{"172.16.0.53"},
		},
		{
			name:      "mixed safe and overlapping does not silently filter",
			gateway:   "172.16.0.1",
			upstreams: []string{"127.0.0.53", "172.16.0.53"},
		},
		{
			name:      "IPv4 mapped upstream compares canonically",
			gateway:   "172.16.0.1",
			upstreams: []string{"::ffff:172.16.0.53"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := NewDNSProxyManager(true, "172.16.0.0/16", test.upstreams)
			upstreamCount := len(manager.upstreams)
			globalInFlight := len(manager.global)

			err := manager.Start(test.gateway)
			if err == nil {
				t.Fatal("Start accepted a DNS proxy upstream inside KILN_VM_BRIDGE_CIDR")
			}
			for _, want := range []string{"overlaps KILN_VM_BRIDGE_CIDR", "choose a non-overlapping"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("Start error = %q, want containing %q", err, want)
				}
			}
			if len(manager.upstreams) != upstreamCount {
				t.Fatalf("Start filtered configured upstreams: got %d, want %d", len(manager.upstreams), upstreamCount)
			}
			if len(manager.servers) != 0 {
				t.Fatalf("Start created DNS proxy server records before rejecting overlap: %#v", manager.servers)
			}
			if len(manager.global) != globalInFlight {
				t.Fatalf("Start consumed global query capacity before rejecting overlap: got %d, want %d", len(manager.global), globalInFlight)
			}
			if manager.closed {
				t.Fatal("Start mutated manager closed state while rejecting overlap")
			}
			if status, observed := manager.Status(test.gateway); observed || status.Running {
				t.Fatalf("Status after rejected Start = %+v observed=%t, want no listener", status, observed)
			}
		})
	}
}

func TestValidateDNSProxyUpstreamsOutsideBridgeAllowsSafeAddressFamilies(t *testing.T) {
	upstreams := normalizeDNSProxyUpstreams([]string{"127.0.0.53", "192.0.2.53", "::1", "2001:db8::53"})
	if err := validateDNSProxyUpstreamsOutsideBridge("172.16.0.0/16", upstreams); err != nil {
		t.Fatalf("validate safe DNS proxy upstreams: %v", err)
	}
}

func TestDNSProxyManagerStartRejectsInvalidBridgeBeforeMutation(t *testing.T) {
	manager := NewDNSProxyManager(true, "not-a-cidr", []string{"127.0.0.53"})
	err := manager.Start("172.16.0.1")
	if err == nil || !strings.Contains(err.Error(), "KILN_VM_BRIDGE_CIDR") {
		t.Fatalf("Start error = %v, want invalid KILN_VM_BRIDGE_CIDR", err)
	}
	if len(manager.servers) != 0 || len(manager.global) != 0 {
		t.Fatalf("invalid bridge mutated proxy runtime: servers=%d global=%d", len(manager.servers), len(manager.global))
	}
}

func TestDNSProxyManagerStopRetainsFailedCloseForFailClosedRetry(t *testing.T) {
	manager := NewDNSProxyManager(true, defaultBaseCIDR, []string{"127.0.0.1"})
	const gatewayIP = "127.0.0.2"
	listenAddr := net.JoinHostPort(gatewayIP, dnsProxyPort)
	closeErr := errors.New("listener close incomplete")
	server := &dnsProxyServer{}
	server.closeOnce.Do(func() {
		server.closed.Store(true)
		server.closeErr = closeErr
	})
	manager.servers[listenAddr] = server

	err := manager.Stop(gatewayIP)
	if err == nil || !errors.Is(err, closeErr) {
		t.Fatalf("Stop() error = %v, want retained close failure", err)
	}
	if got := manager.servers[listenAddr]; got != server {
		t.Fatalf("failed Stop server record = %p, want %p", got, server)
	}
	status, observed := manager.Status(gatewayIP)
	if !observed || status.Running {
		t.Fatalf("Status() = %+v observed=%t, want retained stopped-but-unclean record", status, observed)
	}
	if err := manager.Stop(gatewayIP); err == nil || !errors.Is(err, closeErr) {
		t.Fatalf("retry Stop() error = %v, want same close failure", err)
	}
}

func TestDNSProxyManagerStopForgetsFailedCloseOnlyAfterQuiescence(t *testing.T) {
	manager := NewDNSProxyManager(true, defaultBaseCIDR, []string{"127.0.0.1"})
	const gatewayIP = "127.0.0.2"
	listenAddr := net.JoinHostPort(gatewayIP, dnsProxyPort)
	closeErr := errors.New("listener close reported a late cleanup error")
	server := &dnsProxyServer{}
	server.closeOnce.Do(func() {
		server.closed.Store(true)
		server.shutdownComplete.Store(true)
		server.closeErr = closeErr
	})
	manager.servers[listenAddr] = server

	err := manager.Stop(gatewayIP)
	if err == nil || !errors.Is(err, closeErr) {
		t.Fatalf("Stop() error = %v, want completed close warning", err)
	}
	if _, retained := manager.servers[listenAddr]; retained {
		t.Fatal("quiesced failed-close record was retained and would make cleanup permanently unretryable")
	}
	if err := manager.Stop(gatewayIP); err != nil {
		t.Fatalf("retry Stop() after confirmed quiescence = %v, want nil", err)
	}
}

func startBlackholeUDPUpstreamForTest(t *testing.T) (*net.UDPConn, <-chan struct{}) {
	t.Helper()
	upstream, err := net.ListenUDP("udp4", mustResolveUDPAddr(t, "127.0.0.1:0"))
	if err != nil {
		t.Fatalf("listen blackhole UDP upstream: %v", err)
	}
	received := make(chan struct{}, 1)
	go func() {
		buf := make([]byte, 512)
		if _, _, err := upstream.ReadFromUDP(buf); err == nil {
			received <- struct{}{}
		}
	}()
	return upstream, received
}

func waitForUDPReceipt(t *testing.T, received <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-received:
	case <-time.After(time.Second):
		t.Fatalf("%s UDP upstream did not receive query", label)
	}
}

func TestNormalizeDNSProxyUpstreams(t *testing.T) {
	got := normalizeDNSProxyUpstreams([]string{
		" 127.0.0.53 ",
		"127.0.0.53",
		"192.168.1.254:5353",
		"[2001:db8::53]:0053",
		"2001:db8::53",
		"127.0.0.1:domain",
		"127.0.0.1:0",
		"127.0.0.1:65536",
		"",
		"not-ip",
	})
	want := []string{"127.0.0.53:53", "192.168.1.254:5353", "[2001:db8::53]:53"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalizeDNSProxyUpstreams = %#v, want %#v", got, want)
	}
}

func TestDNSUpstreamNetworkMatchesResolverAddressFamily(t *testing.T) {
	tests := []struct {
		clientNetwork string
		upstream      string
		want          string
	}{
		{clientNetwork: "udp4", upstream: "127.0.0.1:53", want: "udp4"},
		{clientNetwork: "udp4", upstream: "[2001:db8::53]:53", want: "udp6"},
		{clientNetwork: "tcp4", upstream: "127.0.0.1:53", want: "tcp4"},
		{clientNetwork: "tcp4", upstream: "[2001:db8::53]:53", want: "tcp6"},
	}
	for _, test := range tests {
		got, err := dnsUpstreamNetwork(test.clientNetwork, test.upstream)
		if err != nil {
			t.Fatalf("dnsUpstreamNetwork(%q, %q): %v", test.clientNetwork, test.upstream, err)
		}
		if got != test.want {
			t.Fatalf("dnsUpstreamNetwork(%q, %q) = %q, want %q", test.clientNetwork, test.upstream, got, test.want)
		}
	}
	if _, err := dnsUpstreamNetwork("unix", "127.0.0.1:53"); err == nil {
		t.Fatal("dnsUpstreamNetwork accepted unsupported transport")
	}
	if _, err := dnsUpstreamNetwork("udp4", "not-an-upstream"); err == nil {
		t.Fatal("dnsUpstreamNetwork accepted invalid upstream")
	}
}

func TestDNSProxyExchangesWithIPv6UpstreamsFromIPv4Guests(t *testing.T) {
	query := append([]byte(nil), dnsProxyFailureQuery...)
	response := validDNSResponseForTest(query)
	parsed, err := parseDNSQuery(query)
	if err != nil {
		t.Fatalf("parse test query: %v", err)
	}

	t.Run("udp", func(t *testing.T) {
		upstream, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.ParseIP("::1")})
		if err != nil {
			t.Skipf("IPv6 loopback UDP is unavailable: %v", err)
		}
		defer upstream.Close()
		done := make(chan error, 1)
		go func() {
			buf := make([]byte, dns.MaxMsgSize)
			n, peer, readErr := upstream.ReadFromUDP(buf)
			if readErr != nil {
				done <- readErr
				return
			}
			if !bytes.Equal(buf[:n], query) {
				done <- fmt.Errorf("IPv6 UDP query changed: got %x want %x", buf[:n], query)
				return
			}
			_, writeErr := upstream.WriteToUDP(response, peer)
			done <- writeErr
		}()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		got, err := exchangeDNSContext(ctx, "udp4", query, parsed, []string{upstream.LocalAddr().String()}, nil)
		if err != nil {
			t.Fatalf("exchange through IPv6 UDP upstream: %v", err)
		}
		if !bytes.Equal(got, response) {
			t.Fatalf("IPv6 UDP response changed: got %x want %x", got, response)
		}
		if err := <-done; err != nil {
			t.Fatalf("IPv6 UDP upstream: %v", err)
		}
	})

	t.Run("tcp", func(t *testing.T) {
		upstream, err := net.Listen("tcp6", "[::1]:0")
		if err != nil {
			t.Skipf("IPv6 loopback TCP is unavailable: %v", err)
		}
		defer upstream.Close()
		done := make(chan error, 1)
		go func() {
			conn, acceptErr := upstream.Accept()
			if acceptErr != nil {
				done <- acceptErr
				return
			}
			defer conn.Close()
			got, readErr := readDNSFrame(conn)
			if readErr != nil {
				done <- readErr
				return
			}
			if !bytes.Equal(got, query) {
				done <- fmt.Errorf("IPv6 TCP query changed: got %x want %x", got, query)
				return
			}
			done <- writeDNSFrame(conn, response)
		}()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		got, err := exchangeDNSContext(ctx, "tcp4", query, parsed, []string{upstream.Addr().String()}, nil)
		if err != nil {
			t.Fatalf("exchange through IPv6 TCP upstream: %v", err)
		}
		if !bytes.Equal(got, response) {
			t.Fatalf("IPv6 TCP response changed: got %x want %x", got, response)
		}
		if err := <-done; err != nil {
			t.Fatalf("IPv6 TCP upstream: %v", err)
		}
	})
}

func TestGuestDNSServersForAllocationUsesGatewayWhenProxyEnabled(t *testing.T) {
	manager := &Manager{cfg: &config.Config{
		GuestDNSProxyEnabled: true,
		GuestDNSServers:      []string{"9.9.9.9"},
	}}
	got := manager.guestDNSServersForAllocation(&Allocation{GatewayIP: "172.16.0.25"})
	want := []string{"172.16.0.25"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("guestDNSServersForAllocation = %#v, want %#v", got, want)
	}
}

func TestGuestDNSServersForAllocationKeepsDirectOverride(t *testing.T) {
	manager := &Manager{cfg: &config.Config{
		GuestDNSProxyEnabled: false,
		GuestDNSServers:      []string{"9.9.9.9"},
	}}
	got := manager.guestDNSServersForAllocation(&Allocation{GatewayIP: "172.16.0.25"})
	want := []string{"9.9.9.9"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("guestDNSServersForAllocation = %#v, want %#v", got, want)
	}
}

func exchangeThroughProxyUDPForTest(t *testing.T, query []byte, upstreams []string) ([]byte, error) {
	t.Helper()
	proxy := startProxyOnFreePort(t, upstreams)
	defer proxy.Close()
	client, err := net.Dial("udp4", proxy.udp.LocalAddr().String())
	if err != nil {
		return nil, fmt.Errorf("dial UDP proxy: %w", err)
	}
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := client.Write(query); err != nil {
		return nil, fmt.Errorf("write UDP query: %w", err)
	}
	response := make([]byte, dns.MaxMsgSize)
	n, err := client.Read(response)
	if err != nil {
		return nil, fmt.Errorf("read UDP response: %w", err)
	}
	return response[:n], nil
}

func exchangeThroughProxyTCPForTest(t *testing.T, query []byte, upstreams []string) ([]byte, error) {
	t.Helper()
	proxy := startProxyOnFreePort(t, upstreams)
	defer proxy.Close()
	client, err := net.Dial("tcp4", proxy.tcp.Addr().String())
	if err != nil {
		return nil, fmt.Errorf("dial TCP proxy: %w", err)
	}
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(2 * time.Second))
	if err := writeDNSFrame(client, query); err != nil {
		return nil, err
	}
	return readDNSFrame(client)
}

func startProxyOnFreePort(t *testing.T, upstreams []string) *dnsProxyServer {
	t.Helper()
	return startProxyWithGlobalOnFreePort(t, upstreams, make(chan struct{}, dnsProxyMaxGlobalQueries))
}

func startProxyWithGlobalOnFreePort(t *testing.T, upstreams []string, globalSlots chan struct{}) *dnsProxyServer {
	t.Helper()
	var lastErr error
	for attempt := 0; attempt < 20; attempt++ {
		proxy, err := startDNSProxyWithGlobalLimit("127.0.0.1:0", upstreams, globalSlots)
		if err == nil {
			return proxy
		}
		lastErr = err
	}
	t.Fatalf("startDNSProxy after retries: %v", lastErr)
	return nil
}

func reserveLocalTCPPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve TCP port: %v", err)
	}
	defer ln.Close()
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split listener addr: %v", err)
	}
	return port
}

func closedLocalUDPAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.ListenUDP("udp4", mustResolveUDPAddr(t, "127.0.0.1:0"))
	if err != nil {
		t.Fatalf("reserve UDP port: %v", err)
	}
	address := listener.LocalAddr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("close reserved UDP port: %v", err)
	}
	return address
}

func mustResolveUDPAddr(t *testing.T, addr string) *net.UDPAddr {
	t.Helper()
	resolved, err := net.ResolveUDPAddr("udp4", addr)
	if err != nil {
		t.Fatalf("resolve UDP addr %q: %v", addr, err)
	}
	return resolved
}

func TestDNSFrameHelpers(t *testing.T) {
	payload := []byte{1, 2, 3}
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		var header [2]byte
		binary.BigEndian.PutUint16(header[:], uint16(len(payload)))
		_, _ = pw.Write(header[:])
		_, _ = pw.Write(payload)
	}()
	got, err := readDNSFrame(pr)
	if err != nil {
		t.Fatalf("readDNSFrame: %v", err)
	}
	if !reflect.DeepEqual(got, payload) {
		t.Fatalf("readDNSFrame = %#v, want %#v", got, payload)
	}
}

func TestWriteDNSFrameCompletesShortNilWrites(t *testing.T) {
	payload := []byte{1, 2, 3, 4, 5}
	writer := &boundedDNSWriter{limit: 1}
	if err := writeDNSFrame(writer, payload); err != nil {
		t.Fatalf("writeDNSFrame: %v", err)
	}
	want := append([]byte{0, byte(len(payload))}, payload...)
	if got := writer.Bytes(); !bytes.Equal(got, want) {
		t.Fatalf("framed payload = %x, want %x", got, want)
	}
}

func TestWriteDNSFrameRejectsZeroProgress(t *testing.T) {
	err := writeDNSFrame(zeroProgressDNSWriter{}, []byte{1})
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("writeDNSFrame error = %v, want io.ErrShortWrite", err)
	}
}

type boundedDNSWriter struct {
	bytes.Buffer
	limit int
}

func (w *boundedDNSWriter) Write(payload []byte) (int, error) {
	if len(payload) > w.limit {
		payload = payload[:w.limit]
	}
	n, err := w.Buffer.Write(payload)
	if err != nil {
		return n, fmt.Errorf("write bounded DNS buffer: %w", err)
	}
	return n, nil
}

type zeroProgressDNSWriter struct{}

func (zeroProgressDNSWriter) Write([]byte) (int, error) {
	return 0, nil
}
