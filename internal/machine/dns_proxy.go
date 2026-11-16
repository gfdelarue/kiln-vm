package machine

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
)

const (
	dnsProxyPort                 = "53"
	dnsProxyQueryTimeout         = 4 * time.Second
	dnsProxyTCPIdleTimeout       = 10 * time.Second
	dnsProxyMaxConcurrentQueries = 128
	dnsProxyMaxGlobalQueries     = 512
	dnsProxyMaxTCPConnections    = 64
)

const (
	DNSProxyErrorMalformedQuery          = "malformed_query"
	DNSProxyErrorCapacity                = "capacity_exhausted"
	DNSProxyErrorNoUpstreams             = "no_upstreams"
	DNSProxyErrorUpstreamTransport       = "upstream_transport_failure"
	DNSProxyErrorInvalidUpstreamResponse = "invalid_upstream_response"
)

// DNSProxyStatus is a qname-free snapshot of one running proxy. It is safe to
// expose through authenticated provider diagnostics: it contains no query
// payloads, resolver addresses, or consumer network identity.
type DNSProxyStatus struct {
	Running                   bool      `json:"running"`
	Queries                   uint64    `json:"queries"`
	Successes                 uint64    `json:"successes"`
	LocalFormatErrors         uint64    `json:"local_format_errors"`
	LocalServerFailures       uint64    `json:"local_server_failures"`
	RejectedQueries           uint64    `json:"rejected_queries"`
	RejectedTCPConnections    uint64    `json:"rejected_tcp_connections"`
	InvalidUpstreamResponses  uint64    `json:"invalid_upstream_responses"`
	UpstreamTransportFailures uint64    `json:"upstream_transport_failures"`
	InFlight                  int64     `json:"in_flight"`
	LastSuccessAt             time.Time `json:"last_success_at,omitempty"`
	LastResponseAt            time.Time `json:"last_response_at,omitempty"`
	LastResponseRcode         int       `json:"last_response_rcode,omitempty"`
	LastResponseTruncated     bool      `json:"last_response_truncated,omitempty"`
	LastErrorAt               time.Time `json:"last_error_at,omitempty"`
	LastErrorKind             string    `json:"last_error_kind,omitempty"`
}

type DNSProxyManager struct {
	enabled    bool
	bridgeCIDR string
	upstreams  []string

	mu      sync.Mutex
	servers map[string]*dnsProxyServer
	global  chan struct{}
	closed  bool
}

func NewDNSProxyManager(enabled bool, bridgeCIDR string, upstreams []string) *DNSProxyManager {
	return &DNSProxyManager{
		enabled:    enabled,
		bridgeCIDR: strings.TrimSpace(bridgeCIDR),
		upstreams:  normalizeDNSProxyUpstreams(upstreams),
		servers:    map[string]*dnsProxyServer{},
		global:     make(chan struct{}, dnsProxyMaxGlobalQueries),
	}
}

func (m *DNSProxyManager) Start(gatewayIP string) error {
	if m == nil || !m.enabled {
		return nil
	}
	gatewayIP = strings.TrimSpace(gatewayIP)
	if gatewayIP == "" {
		return fmt.Errorf("start guest DNS proxy: gateway IP is empty")
	}
	upstreams := m.upstreams
	if len(upstreams) == 0 {
		return fmt.Errorf("start guest DNS proxy for %s: no upstream resolvers configured", gatewayIP)
	}
	if err := validateDNSProxyUpstreamsOutsideBridge(m.bridgeCIDR, upstreams); err != nil {
		return fmt.Errorf("start guest DNS proxy: %w", err)
	}
	listenAddr := net.JoinHostPort(gatewayIP, dnsProxyPort)

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return fmt.Errorf("start guest DNS proxy on %s: manager is closed", listenAddr)
	}
	if _, ok := m.servers[listenAddr]; ok {
		return nil
	}
	server, err := startDNSProxyWithGlobalLimit(listenAddr, upstreams, m.global)
	if err != nil {
		return fmt.Errorf("start guest DNS proxy on %s: %w", listenAddr, err)
	}
	m.servers[listenAddr] = server
	return nil
}

// Stop closes the proxy associated with gatewayIP. Stopping a valid gateway
// with no active proxy is idempotent. An empty gateway is rejected so a caller
// cannot silently miss a live proxy because it reconstructed an incomplete
// allocation.
func (m *DNSProxyManager) Stop(gatewayIP string) error {
	if m == nil || !m.enabled {
		return nil
	}
	gatewayIP = strings.TrimSpace(gatewayIP)
	if gatewayIP == "" {
		return fmt.Errorf("stop guest DNS proxy: gateway IP is empty")
	}
	listenAddr := net.JoinHostPort(gatewayIP, dnsProxyPort)

	m.mu.Lock()
	defer m.mu.Unlock()
	server := m.servers[listenAddr]
	if server == nil {
		return nil
	}
	if err := server.Close(); err != nil {
		// Retain the record unless Close reached its explicit quiescence edge.
		// A completed close may still report a listener/client cleanup error, but
		// all accept loops and workers have then joined, so a later reconciliation
		// can safely observe the absent listener and release the firewall guard.
		// An unconfirmed close remains fail-closed and must never be forgotten.
		if server.shutdownComplete.Load() {
			delete(m.servers, listenAddr)
		}
		return fmt.Errorf("stop guest DNS proxy on %s: %w", listenAddr, err)
	}
	delete(m.servers, listenAddr)
	return nil
}

// Close shuts down every proxy owned by the manager and returns all cleanup
// failures. It is intended for daemon shutdown and recovery cleanup.
func (m *DNSProxyManager) Close() error {
	if m == nil || !m.enabled {
		return nil
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	servers := m.servers
	m.servers = map[string]*dnsProxyServer{}
	m.mu.Unlock()

	var result error
	for listenAddr, server := range servers {
		if err := server.Close(); err != nil {
			result = errors.Join(result, fmt.Errorf("close guest DNS proxy on %s: %w", listenAddr, err))
		}
	}
	return result
}

// Status returns a sanitized status snapshot for gatewayIP.
func (m *DNSProxyManager) Status(gatewayIP string) (DNSProxyStatus, bool) {
	if m == nil || !m.enabled {
		return DNSProxyStatus{}, false
	}
	listenAddr := net.JoinHostPort(strings.TrimSpace(gatewayIP), dnsProxyPort)
	m.mu.Lock()
	server := m.servers[listenAddr]
	m.mu.Unlock()
	if server == nil {
		return DNSProxyStatus{}, false
	}
	return server.Status(), true
}

func normalizeDNSProxyUpstreams(upstreams []string) []string {
	seen := map[string]bool{}
	normalized := []string{}
	for _, upstream := range upstreams {
		upstream = strings.TrimSpace(upstream)
		if upstream == "" {
			continue
		}
		if host, port, err := net.SplitHostPort(upstream); err == nil {
			ip := net.ParseIP(strings.TrimSpace(strings.Trim(host, "[]")))
			portNumber, portErr := strconv.Atoi(port)
			if ip == nil || portErr != nil || portNumber < 1 || portNumber > 65535 {
				continue
			}
			upstream = net.JoinHostPort(ip.String(), strconv.Itoa(portNumber))
		} else {
			ip := net.ParseIP(upstream)
			if ip == nil {
				continue
			}
			upstream = net.JoinHostPort(ip.String(), dnsProxyPort)
		}
		if seen[upstream] {
			continue
		}
		seen[upstream] = true
		normalized = append(normalized, upstream)
	}
	return normalized
}

func validateDNSProxyUpstreamsOutsideBridge(bridgeCIDR string, upstreams []string) error {
	_, bridgeNetwork, err := parseBaseCIDR(bridgeCIDR)
	if err != nil {
		return fmt.Errorf("validate KILN_VM_BRIDGE_CIDR: %w", err)
	}
	_, addressBits := bridgeNetwork.Mask.Size()
	if addressBits != net.IPv4len*8 {
		return fmt.Errorf("validate KILN_VM_BRIDGE_CIDR: bridge allocation CIDR must be IPv4")
	}
	for _, upstream := range upstreams {
		host, _, err := net.SplitHostPort(upstream)
		if err != nil {
			return fmt.Errorf("validate DNS proxy upstream: invalid normalized resolver address")
		}
		address := net.ParseIP(strings.TrimSpace(strings.Trim(host, "[]")))
		if address == nil {
			return fmt.Errorf("validate DNS proxy upstream: invalid normalized resolver address")
		}
		address = address.To4()
		if address == nil || !bridgeNetwork.Contains(address) {
			continue
		}
		return fmt.Errorf("DNS proxy upstream %s overlaps KILN_VM_BRIDGE_CIDR %s; choose a non-overlapping KILN_VM_BRIDGE_CIDR or resolver address", address.String(), bridgeNetwork.String())
	}
	return nil
}

type dnsProxyServer struct {
	udp       *net.UDPConn
	tcp       net.Listener
	upstreams []string

	ctx    context.Context
	cancel context.CancelFunc

	querySlots  chan struct{}
	globalSlots chan struct{}
	tcpSlots    chan struct{}

	loopsWG   sync.WaitGroup
	workersWG sync.WaitGroup

	clientsMu sync.Mutex
	clients   map[net.Conn]struct{}

	stats dnsProxyStats

	closed atomic.Bool
	// shutdownComplete is set only after both listener loops and every query
	// worker have joined. It is stronger than !Status.Running, which is exposed
	// as soon as shutdown begins.
	shutdownComplete atomic.Bool
	closeOnce        sync.Once
	closeErr         error
}

func startDNSProxyWithGlobalLimit(listenAddr string, upstreams []string, globalSlots chan struct{}) (*dnsProxyServer, error) {
	upstreams = normalizeDNSProxyUpstreams(upstreams)
	if len(upstreams) == 0 {
		return nil, fmt.Errorf("no upstream resolvers configured")
	}
	if globalSlots == nil {
		return nil, fmt.Errorf("global DNS query limit is not configured")
	}
	udpAddr, err := net.ResolveUDPAddr("udp4", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("resolve UDP listen address: %w", err)
	}
	udp, err := net.ListenUDP("udp4", udpAddr)
	if err != nil {
		return nil, fmt.Errorf("listen UDP: %w", err)
	}
	tcpListenAddr := listenAddr
	if host, port, splitErr := net.SplitHostPort(listenAddr); splitErr == nil && port == "0" {
		_, assignedPort, assignedErr := net.SplitHostPort(udp.LocalAddr().String())
		if assignedErr != nil {
			_ = udp.Close()
			return nil, fmt.Errorf("read assigned UDP listen port: %w", assignedErr)
		}
		tcpListenAddr = net.JoinHostPort(host, assignedPort)
	}
	tcp, err := net.Listen("tcp4", tcpListenAddr)
	if err != nil {
		_ = udp.Close()
		return nil, fmt.Errorf("listen TCP: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	server := &dnsProxyServer{
		udp:         udp,
		tcp:         tcp,
		upstreams:   append([]string(nil), upstreams...),
		ctx:         ctx,
		cancel:      cancel,
		querySlots:  make(chan struct{}, dnsProxyMaxConcurrentQueries),
		globalSlots: globalSlots,
		tcpSlots:    make(chan struct{}, dnsProxyMaxTCPConnections),
		clients:     map[net.Conn]struct{}{},
	}
	server.loopsWG.Add(2)
	go server.serveUDP()
	go server.serveTCP()
	return server, nil
}

func (s *dnsProxyServer) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		s.cancel()
		var result error
		if err := s.udp.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			result = errors.Join(result, fmt.Errorf("close UDP listener: %w", err))
		}
		if err := s.tcp.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			result = errors.Join(result, fmt.Errorf("close TCP listener: %w", err))
		}

		// Accept/read loops are the only goroutines allowed to add workers.
		// Waiting for them first prevents WaitGroup.Add racing with Wait.
		s.loopsWG.Wait()
		result = errors.Join(result, s.closeClients())
		s.workersWG.Wait()
		s.shutdownComplete.Store(true)
		s.closeErr = result
	})
	return s.closeErr
}

func (s *dnsProxyServer) Status() DNSProxyStatus {
	if s == nil {
		return DNSProxyStatus{}
	}
	return s.stats.snapshot(!s.closed.Load())
}

func (s *dnsProxyServer) serveUDP() {
	defer s.loopsWG.Done()
	buf := make([]byte, dns.MaxMsgSize)
	for {
		n, client, err := s.udp.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) || s.ctx.Err() != nil {
				return
			}
			continue
		}
		queryWire := append([]byte(nil), buf[:n]...)
		s.stats.queries.Add(1)
		query, err := parseDNSQuery(queryWire)
		if err != nil {
			s.stats.recordFormatError()
			if response, ok := localDNSFormatError(queryWire); ok {
				_, _ = s.udp.WriteToUDP(response, client)
			}
			continue
		}
		if !s.acquireQuery() {
			s.stats.rejectedQueries.Add(1)
			s.stats.recordServerFailure(DNSProxyErrorCapacity)
			if response, responseErr := localDNSServerFailure(query); responseErr == nil {
				_, _ = s.udp.WriteToUDP(response, client)
			}
			continue
		}

		s.workersWG.Add(1)
		go func() {
			defer s.workersWG.Done()
			defer s.releaseQuery()
			ctx, cancel := context.WithTimeout(s.ctx, dnsProxyQueryTimeout)
			defer cancel()
			response, exchangeErr := exchangeDNSContext(ctx, "udp4", queryWire, query, s.upstreams, &s.stats)
			if exchangeErr != nil {
				s.stats.recordServerFailure(exchangeErrorKind(exchangeErr))
				response, _ = localDNSServerFailure(query)
			}
			if len(response) > 0 {
				_, _ = s.udp.WriteToUDP(response, client)
			}
		}()
	}
}

func (s *dnsProxyServer) serveTCP() {
	defer s.loopsWG.Done()
	for {
		conn, err := s.tcp.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || s.ctx.Err() != nil {
				return
			}
			continue
		}
		if !tryAcquire(s.tcpSlots) {
			s.stats.rejectedTCPConnections.Add(1)
			_ = conn.Close()
			continue
		}
		s.trackClient(conn)
		s.workersWG.Add(1)
		go func() {
			defer s.workersWG.Done()
			defer func() {
				s.untrackClient(conn)
				<-s.tcpSlots
				_ = conn.Close()
			}()
			s.serveTCPConnection(conn)
		}()
	}
}

func (s *dnsProxyServer) serveTCPConnection(conn net.Conn) {
	for {
		if s.ctx.Err() != nil {
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(dnsProxyTCPIdleTimeout))
		queryWire, err := readDNSFrame(conn)
		if err != nil {
			return
		}
		s.stats.queries.Add(1)
		query, err := parseDNSQuery(queryWire)
		if err != nil {
			s.stats.recordFormatError()
			response, ok := localDNSFormatError(queryWire)
			if !ok || writeDNSFrame(conn, response) != nil {
				return
			}
			continue
		}
		if !s.acquireQuery() {
			s.stats.rejectedQueries.Add(1)
			s.stats.recordServerFailure(DNSProxyErrorCapacity)
			response, responseErr := localDNSServerFailure(query)
			if responseErr != nil || writeDNSFrame(conn, response) != nil {
				return
			}
			continue
		}

		ctx, cancel := context.WithTimeout(s.ctx, dnsProxyQueryTimeout)
		response, exchangeErr := exchangeDNSContext(ctx, "tcp4", queryWire, query, s.upstreams, &s.stats)
		cancel()
		s.releaseQuery()
		if exchangeErr != nil {
			s.stats.recordServerFailure(exchangeErrorKind(exchangeErr))
			response, _ = localDNSServerFailure(query)
		}
		_ = conn.SetWriteDeadline(time.Now().Add(dnsProxyQueryTimeout))
		if len(response) == 0 || writeDNSFrame(conn, response) != nil {
			return
		}
	}
}

func (s *dnsProxyServer) acquireQuery() bool {
	if !tryAcquire(s.querySlots) {
		return false
	}
	if !tryAcquire(s.globalSlots) {
		<-s.querySlots
		return false
	}
	s.stats.inFlight.Add(1)
	return true
}

func (s *dnsProxyServer) releaseQuery() {
	<-s.querySlots
	<-s.globalSlots
	s.stats.inFlight.Add(-1)
}

func tryAcquire(slots chan struct{}) bool {
	select {
	case slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *dnsProxyServer) trackClient(conn net.Conn) {
	s.clientsMu.Lock()
	s.clients[conn] = struct{}{}
	s.clientsMu.Unlock()
}

func (s *dnsProxyServer) untrackClient(conn net.Conn) {
	s.clientsMu.Lock()
	delete(s.clients, conn)
	s.clientsMu.Unlock()
}

func (s *dnsProxyServer) closeClients() error {
	s.clientsMu.Lock()
	clients := make([]net.Conn, 0, len(s.clients))
	for conn := range s.clients {
		clients = append(clients, conn)
	}
	s.clientsMu.Unlock()
	var result error
	for _, conn := range clients {
		if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			result = errors.Join(result, fmt.Errorf("close TCP client: %w", err))
		}
	}
	return result
}

func exchangeDNSContext(ctx context.Context, network string, queryWire []byte, query *dns.Msg, upstreams []string, stats *dnsProxyStats) ([]byte, error) {
	upstreams = normalizeDNSProxyUpstreams(upstreams)
	if len(upstreams) == 0 {
		return nil, &dnsProxyExchangeError{kind: DNSProxyErrorNoUpstreams, err: fmt.Errorf("no upstream resolvers configured")}
	}
	var lastErr error
	for index, upstream := range upstreams {
		if ctx.Err() != nil {
			break
		}
		attemptCtx, cancel := dnsAttemptContext(ctx, len(upstreams)-index)
		upstreamNetwork, err := dnsUpstreamNetwork(network, upstream)
		if err != nil {
			cancel()
			lastErr = &dnsProxyExchangeError{kind: DNSProxyErrorUpstreamTransport, err: err}
			continue
		}
		responseWire, err := exchangeDNSUpstream(attemptCtx, upstreamNetwork, upstream, queryWire)
		cancel()
		if err != nil {
			if stats != nil {
				stats.upstreamTransportFailures.Add(1)
				stats.recordLastError(DNSProxyErrorUpstreamTransport)
			}
			lastErr = &dnsProxyExchangeError{kind: DNSProxyErrorUpstreamTransport, err: err}
			continue
		}
		response, err := validateDNSResponse(query, responseWire)
		if err != nil {
			if stats != nil {
				stats.invalidUpstreamResponses.Add(1)
				stats.recordLastError(DNSProxyErrorInvalidUpstreamResponse)
			}
			lastErr = &dnsProxyExchangeError{kind: DNSProxyErrorInvalidUpstreamResponse, err: err}
			continue
		}
		if stats != nil {
			stats.recordSuccess(response)
		}
		return responseWire, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	if ctx.Err() != nil {
		return nil, &dnsProxyExchangeError{kind: DNSProxyErrorUpstreamTransport, err: ctx.Err()}
	}
	return nil, &dnsProxyExchangeError{kind: DNSProxyErrorNoUpstreams, err: fmt.Errorf("no upstream resolvers configured")}
}

func dnsUpstreamNetwork(clientNetwork, upstream string) (string, error) {
	transport := strings.TrimSuffix(strings.TrimSuffix(clientNetwork, "4"), "6")
	if transport != "udp" && transport != "tcp" {
		return "", fmt.Errorf("unsupported DNS transport %q", clientNetwork)
	}
	host, _, err := net.SplitHostPort(upstream)
	if err != nil {
		return "", fmt.Errorf("invalid DNS upstream %q: %w", upstream, err)
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		return "", fmt.Errorf("invalid DNS upstream address %q", host)
	}
	if ip.To4() != nil {
		return transport + "4", nil
	}
	return transport + "6", nil
}

func dnsAttemptContext(parent context.Context, attemptsRemaining int) (context.Context, context.CancelFunc) {
	deadline, ok := parent.Deadline()
	if !ok || attemptsRemaining <= 1 {
		return context.WithCancel(parent)
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, remaining/time.Duration(attemptsRemaining))
}

func exchangeDNSUpstream(ctx context.Context, network, upstream string, queryWire []byte) ([]byte, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, network, upstream)
	if err != nil {
		return nil, fmt.Errorf("dial DNS upstream: %w", err)
	}
	defer conn.Close()
	done := closeConnOnContext(ctx, conn)
	defer done()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	switch network {
	case "udp4", "udp6":
		if n, err := conn.Write(queryWire); err != nil {
			return nil, fmt.Errorf("write UDP DNS query: %w", err)
		} else if n != len(queryWire) {
			return nil, fmt.Errorf("write UDP DNS query: %w", io.ErrShortWrite)
		}
		response := make([]byte, dns.MaxMsgSize)
		n, err := conn.Read(response)
		if err != nil {
			return nil, fmt.Errorf("read UDP DNS response: %w", err)
		}
		return append([]byte(nil), response[:n]...), nil
	case "tcp4", "tcp6":
		if err := writeDNSFrame(conn, queryWire); err != nil {
			return nil, err
		}
		return readDNSFrame(conn)
	default:
		return nil, fmt.Errorf("unsupported DNS transport %q", network)
	}
}

func closeConnOnContext(ctx context.Context, conn net.Conn) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	return func() { close(done) }
}

func parseDNSQuery(wire []byte) (*dns.Msg, error) {
	if len(wire) < 12 {
		return nil, fmt.Errorf("DNS query is shorter than a header")
	}
	if wire[2]&0x80 != 0 {
		return nil, fmt.Errorf("DNS query has response bit set")
	}
	if opcode := int((wire[2] >> 3) & 0x0f); opcode != dns.OpcodeQuery {
		return nil, fmt.Errorf("unsupported DNS query opcode %d", opcode)
	}
	if questions := binary.BigEndian.Uint16(wire[4:6]); questions != 1 {
		return nil, fmt.Errorf("DNS query question count is %d, want 1", questions)
	}
	if err := validateDNSWireLength(wire); err != nil {
		return nil, fmt.Errorf("validate DNS query wire format: %w", err)
	}
	var query dns.Msg
	if err := query.Unpack(wire); err != nil {
		return nil, fmt.Errorf("unpack DNS query: %w", err)
	}
	if !dnsSectionCountsMatch(wire, &query) {
		return nil, fmt.Errorf("DNS query section counts do not match payload")
	}
	if query.Response || query.Opcode != dns.OpcodeQuery || len(query.Question) != 1 {
		return nil, fmt.Errorf("invalid DNS query header")
	}
	if query.Authoritative || query.Truncated || query.RecursionAvailable || query.Zero || query.Rcode != dns.RcodeSuccess {
		return nil, fmt.Errorf("invalid DNS query flags")
	}
	question := query.Question[0]
	if question.Name == "" || question.Qtype == 0 || question.Qclass == 0 {
		return nil, fmt.Errorf("invalid DNS question")
	}
	return &query, nil
}

func validateDNSResponse(query *dns.Msg, wire []byte) (*dns.Msg, error) {
	if query == nil {
		return nil, fmt.Errorf("DNS query is nil")
	}
	if len(wire) < 12 {
		return nil, fmt.Errorf("DNS response is shorter than a header")
	}
	if wire[2]&0x80 == 0 {
		return nil, fmt.Errorf("DNS response bit is not set")
	}
	if binary.BigEndian.Uint16(wire[:2]) != query.Id {
		return nil, fmt.Errorf("DNS response transaction ID does not match query")
	}
	if opcode := int((wire[2] >> 3) & 0x0f); opcode != query.Opcode {
		return nil, fmt.Errorf("DNS response opcode does not match query")
	}
	if questions := binary.BigEndian.Uint16(wire[4:6]); int(questions) != len(query.Question) {
		return nil, fmt.Errorf("DNS response question count does not match query")
	}
	if err := validateDNSWireLength(wire); err != nil {
		return nil, fmt.Errorf("validate DNS response wire format: %w", err)
	}
	var response dns.Msg
	if err := response.Unpack(wire); err != nil {
		return nil, fmt.Errorf("unpack DNS response: %w", err)
	}
	if !dnsSectionCountsMatch(wire, &response) {
		return nil, fmt.Errorf("DNS response section counts do not match payload")
	}
	if !response.Response || response.Id != query.Id || response.Opcode != query.Opcode {
		return nil, fmt.Errorf("DNS response header does not match query")
	}
	if response.Zero {
		return nil, fmt.Errorf("DNS response reserved flag is set")
	}
	if len(response.Question) != len(query.Question) {
		return nil, fmt.Errorf("DNS response question count does not match query")
	}
	for index := range query.Question {
		if !sameDNSQuestion(query.Question[index], response.Question[index]) {
			return nil, fmt.Errorf("DNS response question does not match query")
		}
	}
	return &response, nil
}

func validateDNSWireLength(wire []byte) error {
	if len(wire) < 12 {
		return fmt.Errorf("message is shorter than a header")
	}
	offset := 12
	questionCount := int(binary.BigEndian.Uint16(wire[4:6]))
	for index := 0; index < questionCount; index++ {
		_, next, err := dns.UnpackDomainName(wire, offset)
		if err != nil {
			return fmt.Errorf("unpack DNS question name: %w", err)
		}
		offset = next
		if offset+4 > len(wire) {
			return io.ErrUnexpectedEOF
		}
		offset += 4
	}
	recordCount := int(binary.BigEndian.Uint16(wire[6:8])) +
		int(binary.BigEndian.Uint16(wire[8:10])) +
		int(binary.BigEndian.Uint16(wire[10:12]))
	for index := 0; index < recordCount; index++ {
		_, next, err := dns.UnpackRR(wire, offset)
		if err != nil {
			return fmt.Errorf("unpack DNS resource record: %w", err)
		}
		offset = next
	}
	if offset != len(wire) {
		return fmt.Errorf("message has %d trailing bytes", len(wire)-offset)
	}
	return nil
}

func dnsSectionCountsMatch(wire []byte, message *dns.Msg) bool {
	if len(wire) < 12 || message == nil {
		return false
	}
	return int(binary.BigEndian.Uint16(wire[4:6])) == len(message.Question) &&
		int(binary.BigEndian.Uint16(wire[6:8])) == len(message.Answer) &&
		int(binary.BigEndian.Uint16(wire[8:10])) == len(message.Ns) &&
		int(binary.BigEndian.Uint16(wire[10:12])) == len(message.Extra)
}

func sameDNSQuestion(left, right dns.Question) bool {
	return strings.EqualFold(left.Name, right.Name) && left.Qtype == right.Qtype && left.Qclass == right.Qclass
}

func localDNSFormatError(queryWire []byte) ([]byte, bool) {
	if len(queryWire) < 2 {
		return nil, false
	}
	response := &dns.Msg{MsgHdr: dns.MsgHdr{
		Id:       binary.BigEndian.Uint16(queryWire[:2]),
		Response: true,
		Rcode:    dns.RcodeFormatError,
	}}
	if len(queryWire) >= 4 {
		response.Opcode = int((queryWire[2] >> 3) & 0x0f)
		response.RecursionDesired = queryWire[2]&0x01 != 0
		response.CheckingDisabled = queryWire[3]&0x10 != 0
	}
	wire, err := response.Pack()
	return wire, err == nil
}

func localDNSServerFailure(query *dns.Msg) ([]byte, error) {
	if query == nil || len(query.Question) != 1 {
		return nil, fmt.Errorf("cannot build DNS SERVFAIL for invalid query")
	}
	response := &dns.Msg{MsgHdr: dns.MsgHdr{
		Id:                 query.Id,
		Response:           true,
		Opcode:             query.Opcode,
		RecursionDesired:   query.RecursionDesired,
		RecursionAvailable: true,
		CheckingDisabled:   query.CheckingDisabled,
		Rcode:              dns.RcodeServerFailure,
	}}
	response.Question = append([]dns.Question(nil), query.Question...)
	wire, err := response.Pack()
	if err != nil {
		return nil, fmt.Errorf("pack DNS SERVFAIL: %w", err)
	}
	return wire, nil
}

type dnsProxyExchangeError struct {
	kind string
	err  error
}

func (e *dnsProxyExchangeError) Error() string {
	if e == nil || e.err == nil {
		return "DNS exchange failed"
	}
	return e.err.Error()
}

func (e *dnsProxyExchangeError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func exchangeErrorKind(err error) string {
	var exchangeErr *dnsProxyExchangeError
	if errors.As(err, &exchangeErr) && exchangeErr.kind != "" {
		return exchangeErr.kind
	}
	return DNSProxyErrorUpstreamTransport
}

type dnsProxyStats struct {
	queries                   atomic.Uint64
	successes                 atomic.Uint64
	localFormatErrors         atomic.Uint64
	localServerFailures       atomic.Uint64
	rejectedQueries           atomic.Uint64
	rejectedTCPConnections    atomic.Uint64
	invalidUpstreamResponses  atomic.Uint64
	upstreamTransportFailures atomic.Uint64
	inFlight                  atomic.Int64

	lastMu                sync.RWMutex
	lastSuccessAt         time.Time
	lastResponseAt        time.Time
	lastResponseRcode     int
	lastResponseTruncated bool
	lastErrorAt           time.Time
	lastErrorKind         string
}

func (s *dnsProxyStats) recordSuccess(response *dns.Msg) {
	s.successes.Add(1)
	now := time.Now().UTC()
	s.lastMu.Lock()
	s.lastSuccessAt = now
	s.lastResponseAt = now
	if response != nil {
		s.lastResponseRcode = response.Rcode
		s.lastResponseTruncated = response.Truncated
	}
	s.lastMu.Unlock()
}

func (s *dnsProxyStats) recordFormatError() {
	s.localFormatErrors.Add(1)
	s.recordLastError(DNSProxyErrorMalformedQuery)
}

func (s *dnsProxyStats) recordServerFailure(kind string) {
	s.localServerFailures.Add(1)
	s.recordLastError(kind)
}

func (s *dnsProxyStats) recordLastError(kind string) {
	s.lastMu.Lock()
	s.lastErrorAt = time.Now().UTC()
	s.lastErrorKind = kind
	s.lastMu.Unlock()
}

func (s *dnsProxyStats) snapshot(running bool) DNSProxyStatus {
	s.lastMu.RLock()
	lastSuccessAt := s.lastSuccessAt
	lastResponseAt := s.lastResponseAt
	lastResponseRcode := s.lastResponseRcode
	lastResponseTruncated := s.lastResponseTruncated
	lastErrorAt := s.lastErrorAt
	lastErrorKind := s.lastErrorKind
	s.lastMu.RUnlock()
	return DNSProxyStatus{
		Running:                   running,
		Queries:                   s.queries.Load(),
		Successes:                 s.successes.Load(),
		LocalFormatErrors:         s.localFormatErrors.Load(),
		LocalServerFailures:       s.localServerFailures.Load(),
		RejectedQueries:           s.rejectedQueries.Load(),
		RejectedTCPConnections:    s.rejectedTCPConnections.Load(),
		InvalidUpstreamResponses:  s.invalidUpstreamResponses.Load(),
		UpstreamTransportFailures: s.upstreamTransportFailures.Load(),
		InFlight:                  s.inFlight.Load(),
		LastSuccessAt:             lastSuccessAt,
		LastResponseAt:            lastResponseAt,
		LastResponseRcode:         lastResponseRcode,
		LastResponseTruncated:     lastResponseTruncated,
		LastErrorAt:               lastErrorAt,
		LastErrorKind:             lastErrorKind,
	}
}

func readDNSFrame(r io.Reader) ([]byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, fmt.Errorf("read DNS TCP frame header: %w", err)
	}
	size := binary.BigEndian.Uint16(header[:])
	if size == 0 {
		return nil, fmt.Errorf("empty DNS TCP frame")
	}
	payload := make([]byte, int(size))
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, fmt.Errorf("read DNS TCP frame payload: %w", err)
	}
	return payload, nil
}

func writeDNSFrame(w io.Writer, payload []byte) error {
	if len(payload) > dns.MaxMsgSize {
		return fmt.Errorf("DNS TCP frame too large: %d bytes", len(payload))
	}
	var header [2]byte
	binary.BigEndian.PutUint16(header[:], uint16(len(payload)))
	if err := writeDNSAll(w, header[:]); err != nil {
		return fmt.Errorf("write DNS TCP frame header: %w", err)
	}
	if err := writeDNSAll(w, payload); err != nil {
		return fmt.Errorf("write DNS TCP frame payload: %w", err)
	}
	return nil
}

func writeDNSAll(w io.Writer, payload []byte) error {
	for len(payload) > 0 {
		n, err := w.Write(payload)
		if n < 0 || n > len(payload) {
			return fmt.Errorf("invalid write count %d for %d bytes", n, len(payload))
		}
		payload = payload[n:]
		if err != nil {
			return fmt.Errorf("write DNS frame bytes: %w", err)
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
