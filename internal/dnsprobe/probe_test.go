package dnsprobe

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func TestProbeReportsHealthyUDPAndTCPResolution(t *testing.T) {
	server := startTestDNSServer(t, testDNSHealthy, testDNSHealthy)

	result := Probe(context.Background(), Options{
		Server:           server,
		TotalTimeout:     time.Second,
		TransportTimeout: 300 * time.Millisecond,
	})

	if result.SchemaVersion != SchemaVersion || result.Server != server || result.Name != DefaultName || result.QueryType != "A" {
		t.Fatalf("probe identity = %+v", result)
	}
	if !result.PathOK || !result.ResolutionOK || result.FailureKind != "" || result.ExitCode() != ExitOK {
		t.Fatalf("probe result = %+v, want healthy", result)
	}
	for _, transport := range []TransportResult{result.UDP, result.TCP} {
		if !transport.PathOK || !transport.ResolutionOK || transport.RCode == nil || *transport.RCode != 0 || transport.RCodeName != "NOERROR" || transport.AddressCount != 1 {
			t.Fatalf("%s result = %+v, want one resolved A record", transport.Transport, transport)
		}
	}
}

func TestProbeAcceptsOnlyRelevantINAddressAnswers(t *testing.T) {
	tests := []struct {
		name             string
		mode             testDNSMode
		wantPath         bool
		wantResolution   bool
		wantAddressCount int
	}{
		{name: "validated CNAME chain", mode: testDNSCNAME, wantPath: true, wantResolution: true, wantAddressCount: 1},
		{name: "unrelated owner", mode: testDNSUnrelatedA, wantPath: true},
		{name: "wrong class", mode: testDNSWrongClassA, wantPath: true},
		{name: "unrelated CNAME target", mode: testDNSCNAMEUnrelatedA, wantPath: true},
		{name: "CNAME loop", mode: testDNSCNAMELoop},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := startTestDNSServer(t, test.mode, test.mode)
			result := Probe(context.Background(), Options{
				Server:           server,
				TotalTimeout:     time.Second,
				TransportTimeout: 300 * time.Millisecond,
			})

			if result.PathOK != test.wantPath || result.ResolutionOK != test.wantResolution {
				t.Fatalf("probe result = %+v, want path=%t resolution=%t", result, test.wantPath, test.wantResolution)
			}
			for _, transport := range []TransportResult{result.UDP, result.TCP} {
				if transport.AddressCount != test.wantAddressCount {
					t.Fatalf("%s address count = %d, want %d (result=%+v)", transport.Transport, transport.AddressCount, test.wantAddressCount, transport)
				}
			}
		})
	}
}

func TestProbeRetriesTruncatedUDPResponseOverTCP(t *testing.T) {
	server := startTestDNSServer(t, testDNSTruncated, testDNSHealthy)

	result := Probe(context.Background(), Options{
		Server:           server,
		TotalTimeout:     time.Second,
		TransportTimeout: 300 * time.Millisecond,
	})

	if !result.PathOK || !result.ResolutionOK || result.ExitCode() != ExitOK {
		t.Fatalf("probe result = %+v, want healthy TCP fallback", result)
	}
	if !result.UDP.PathOK || !result.UDP.ResolutionOK || result.UDP.RCode == nil || *result.UDP.RCode != 0 || result.UDP.AnswerCount != 1 || result.UDP.AddressCount != 1 {
		t.Fatalf("UDP result = %+v, want validated TCP fallback answer", result.UDP)
	}
}

func TestProbeDistinguishesResolverFailureFromGatewayPath(t *testing.T) {
	server := startTestDNSServer(t, testDNSSERVFAIL, testDNSSERVFAIL)

	result := Probe(context.Background(), Options{
		Server:           server,
		TotalTimeout:     time.Second,
		TransportTimeout: 300 * time.Millisecond,
	})

	if !result.PathOK || result.ResolutionOK || result.FailureKind != "resolution" || result.ExitCode() != ExitResolution {
		t.Fatalf("probe result = %+v, want resolver failure with working path", result)
	}
	for _, transport := range []TransportResult{result.UDP, result.TCP} {
		if !transport.PathOK || transport.ResolutionOK || transport.FailureKind != "resolution" || transport.Phase != "resolve" || transport.RCode == nil || *transport.RCode != 2 || transport.RCodeName != "SERVFAIL" {
			t.Fatalf("%s result = %+v, want SERVFAIL resolution classification", transport.Transport, transport)
		}
	}
}

func TestProbeClassifiesInvalidRepliesAsPathFailures(t *testing.T) {
	server := startTestDNSServer(t, testDNSWrongTransaction, testDNSMalformed)

	result := Probe(context.Background(), Options{
		Server:           server,
		TotalTimeout:     time.Second,
		TransportTimeout: 300 * time.Millisecond,
	})

	if result.PathOK || result.ResolutionOK || result.FailureKind != "path" || result.ExitCode() != ExitPath {
		t.Fatalf("probe result = %+v, want path failure", result)
	}
	if result.UDP.Phase != "validate_response" || !strings.Contains(result.UDP.Error, "transaction ID") {
		t.Fatalf("UDP result = %+v, want transaction validation failure", result.UDP)
	}
	if result.TCP.Phase != "read_frame" || !strings.Contains(result.TCP.Error, "shorter than a DNS header") {
		t.Fatalf("TCP result = %+v, want frame validation failure", result.TCP)
	}
}

func TestProbePreservesPerTransportPartialSuccess(t *testing.T) {
	server := startTestDNSServer(t, testDNSSilent, testDNSHealthy)

	result := Probe(context.Background(), Options{
		Server:           server,
		TotalTimeout:     500 * time.Millisecond,
		TransportTimeout: 80 * time.Millisecond,
	})

	if result.PathOK || result.ResolutionOK || result.FailureKind != "path" {
		t.Fatalf("probe result = %+v, want aggregate path failure", result)
	}
	if result.UDP.PathOK || result.UDP.FailureKind != "path" || result.UDP.Phase != "read" || result.UDP.Error != context.DeadlineExceeded.Error() {
		t.Fatalf("UDP result = %+v, want bounded read timeout", result.UDP)
	}
	if !result.TCP.PathOK || !result.TCP.ResolutionOK {
		t.Fatalf("TCP result = %+v, want independent success", result.TCP)
	}
}

func TestProbeTotalDeadlineBoundsBothTransports(t *testing.T) {
	server := startTestDNSServer(t, testDNSSilent, testDNSSilent)
	started := time.Now()

	result := Probe(context.Background(), Options{
		Server:           server,
		TotalTimeout:     70 * time.Millisecond,
		TransportTimeout: time.Second,
	})
	elapsed := time.Since(started)

	if elapsed > 500*time.Millisecond {
		t.Fatalf("probe exceeded hard total deadline: %s", elapsed)
	}
	if result.PathOK || result.ExitCode() != ExitPath {
		t.Fatalf("probe result = %+v, want bounded path failure", result)
	}
}

func TestProbeRejectsInputsWithoutNetworkAccess(t *testing.T) {
	tests := []struct {
		name    string
		options Options
		want    string
	}{
		{name: "missing server", options: Options{}, want: "server is required"},
		{name: "server hostname", options: Options{Server: "resolver.example:53"}, want: "IPv4 address"},
		{name: "IPv6 server", options: Options{Server: "[::1]:53"}, want: "IPv4 address"},
		{name: "invalid port", options: Options{Server: "127.0.0.1:70000"}, want: "1 to 65535"},
		{name: "empty label", options: Options{Server: "127.0.0.1", Name: "bad..name"}, want: "empty label"},
		{name: "negative timeout", options: Options{Server: "127.0.0.1", TotalTimeout: -time.Second}, want: "must be positive"},
		{name: "unbounded total", options: Options{Server: "127.0.0.1", TotalTimeout: 2 * time.Minute}, want: "must not exceed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := Probe(context.Background(), test.options)
			if result.FailureKind != "usage" || result.ExitCode() != ExitUsage || !strings.Contains(result.Error, test.want) {
				t.Fatalf("Probe() = %+v, want usage error containing %q", result, test.want)
			}
		})
	}
}

type testDNSMode int

const (
	testDNSHealthy testDNSMode = iota
	testDNSSERVFAIL
	testDNSWrongTransaction
	testDNSMalformed
	testDNSSilent
	testDNSCNAME
	testDNSUnrelatedA
	testDNSWrongClassA
	testDNSCNAMEUnrelatedA
	testDNSCNAMELoop
	testDNSTruncated
)

type testDNSServer struct {
	tcp  net.Listener
	udp  *net.UDPConn
	done chan struct{}
	wg   sync.WaitGroup
}

func startTestDNSServer(t *testing.T, udpMode, tcpMode testDNSMode) string {
	t.Helper()
	tcp, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen TCP: %v", err)
	}
	port := tcp.Addr().(*net.TCPAddr).Port
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	if err != nil {
		_ = tcp.Close()
		t.Fatalf("listen UDP: %v", err)
	}
	server := &testDNSServer{tcp: tcp, udp: udp, done: make(chan struct{})}
	server.wg.Add(2)
	go server.serveUDP(udpMode)
	go server.serveTCP(tcpMode)
	t.Cleanup(func() {
		close(server.done)
		_ = server.udp.Close()
		_ = server.tcp.Close()
		server.wg.Wait()
	})
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
}

func (s *testDNSServer) serveUDP(mode testDNSMode) {
	defer s.wg.Done()
	buf := make([]byte, maxDNSMessage)
	for {
		n, peer, err := s.udp.ReadFromUDP(buf)
		if err != nil {
			return
		}
		response := testDNSResponse(buf[:n], mode)
		if response != nil {
			_, _ = s.udp.WriteToUDP(response, peer)
		}
	}
}

func (s *testDNSServer) serveTCP(mode testDNSMode) {
	defer s.wg.Done()
	for {
		conn, err := s.tcp.Accept()
		if err != nil {
			return
		}
		func() {
			defer conn.Close()
			var header [2]byte
			if _, err := io.ReadFull(conn, header[:]); err != nil {
				return
			}
			query := make([]byte, int(binary.BigEndian.Uint16(header[:])))
			if _, err := io.ReadFull(conn, query); err != nil {
				return
			}
			response := testDNSResponse(query, mode)
			if response == nil {
				<-s.done
				return
			}
			binary.BigEndian.PutUint16(header[:], uint16(len(response)))
			_, _ = conn.Write(append(header[:], response...))
		}()
	}
}

func testDNSResponse(query []byte, mode testDNSMode) []byte {
	if mode == testDNSSilent {
		return nil
	}
	if mode == testDNSMalformed {
		return []byte{0, 1, 2, 3}
	}
	var parser dnsmessage.Parser
	header, err := parser.Start(query)
	if err != nil {
		return []byte{0}
	}
	questions, err := parser.AllQuestions()
	if err != nil || len(questions) != 1 {
		return []byte{0}
	}
	if mode == testDNSWrongTransaction {
		header.ID++
	}
	header.Response = true
	header.RecursionAvailable = true
	header.RCode = dnsmessage.RCodeSuccess
	if mode == testDNSSERVFAIL {
		header.RCode = dnsmessage.RCodeServerFailure
	}
	if mode == testDNSTruncated {
		header.Truncated = true
	}
	builder := dnsmessage.NewBuilder(nil, header)
	builder.EnableCompression()
	if err := builder.StartQuestions(); err != nil {
		return []byte{0}
	}
	if err := builder.Question(questions[0]); err != nil {
		return []byte{0}
	}
	if mode == testDNSHealthy || mode == testDNSCNAME || mode == testDNSUnrelatedA || mode == testDNSWrongClassA || mode == testDNSCNAMEUnrelatedA || mode == testDNSCNAMELoop {
		if err := builder.StartAnswers(); err != nil {
			return []byte{0}
		}
		alias, err := dnsmessage.NewName("alias.example.")
		if err != nil {
			return []byte{0}
		}
		unrelated, err := dnsmessage.NewName("unrelated.example.")
		if err != nil {
			return []byte{0}
		}
		if mode == testDNSCNAME || mode == testDNSCNAMEUnrelatedA || mode == testDNSCNAMELoop {
			if err := builder.CNAMEResource(dnsmessage.ResourceHeader{
				Name: questions[0].Name, Type: dnsmessage.TypeCNAME, Class: dnsmessage.ClassINET, TTL: 60,
			}, dnsmessage.CNAMEResource{CNAME: alias}); err != nil {
				return []byte{0}
			}
		}
		if mode == testDNSCNAMELoop {
			if err := builder.CNAMEResource(dnsmessage.ResourceHeader{
				Name: alias, Type: dnsmessage.TypeCNAME, Class: dnsmessage.ClassINET, TTL: 60,
			}, dnsmessage.CNAMEResource{CNAME: questions[0].Name}); err != nil {
				return []byte{0}
			}
		}
		if mode == testDNSCNAMELoop {
			response, err := builder.Finish()
			if err != nil {
				return []byte{0}
			}
			return response
		}
		owner := questions[0].Name
		class := dnsmessage.ClassINET
		switch mode {
		case testDNSCNAME:
			owner = alias
		case testDNSUnrelatedA, testDNSCNAMEUnrelatedA:
			owner = unrelated
		case testDNSWrongClassA:
			class = dnsmessage.ClassCHAOS
		}
		if err := builder.AResource(dnsmessage.ResourceHeader{
			Name:  owner,
			Type:  dnsmessage.TypeA,
			Class: class,
			TTL:   60,
		}, dnsmessage.AResource{A: [4]byte{93, 184, 216, 34}}); err != nil {
			return []byte{0}
		}
	}
	response, err := builder.Finish()
	if err != nil {
		return []byte{0}
	}
	return response
}

func TestContextErrorPrefersDeadlineClassification(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := contextError(ctx, errors.New("socket text")); !errors.Is(err, context.Canceled) {
		t.Fatalf("contextError = %v, want context cancellation", err)
	}
}

func TestWriteAllRejectsAZeroLengthWrite(t *testing.T) {
	if err := writeAll(zeroWriter{}, []byte("dns")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("writeAll error = %v, want short write", err)
	}
}

type zeroWriter struct{}

func (zeroWriter) Write([]byte) (int, error) { return 0, nil }
