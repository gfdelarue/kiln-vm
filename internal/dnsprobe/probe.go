package dnsprobe

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

const (
	SchemaVersion = 1
	DefaultName   = "example.com."
	DefaultPort   = "53"

	DefaultTotalTimeout     = 8 * time.Second
	DefaultTransportTimeout = 3 * time.Second

	ExitOK         = 0
	ExitUsage      = 2
	ExitPath       = 20
	ExitResolution = 21
)

const (
	dnsHeaderSize = 12
	maxDNSMessage = 65535
)

// Options controls one bounded DNS readiness probe. Server must be an IP
// address, optionally with a port, so the probe never needs DNS to locate DNS.
type Options struct {
	Server           string
	Name             string
	TotalTimeout     time.Duration
	TransportTimeout time.Duration
}

// Result is the private, versioned kiln-vm-agent DNS probe result contract.
// PathOK means that both transports received a syntactically valid DNS reply.
// ResolutionOK additionally requires a NOERROR reply containing an answer.
type Result struct {
	SchemaVersion int             `json:"schema_version"`
	Server        string          `json:"server"`
	Name          string          `json:"name"`
	QueryType     string          `json:"query_type"`
	PathOK        bool            `json:"path_ok"`
	ResolutionOK  bool            `json:"resolution_ok"`
	FailureKind   string          `json:"failure_kind,omitempty"`
	UDP           TransportResult `json:"udp"`
	TCP           TransportResult `json:"tcp"`
	Error         string          `json:"error,omitempty"`
}

// TransportResult separates connectivity to the DNS service from the
// external resolver's ability to answer the requested name.
type TransportResult struct {
	Transport    string `json:"transport"`
	PathOK       bool   `json:"path_ok"`
	ResolutionOK bool   `json:"resolution_ok"`
	FailureKind  string `json:"failure_kind,omitempty"`
	Phase        string `json:"phase,omitempty"`
	RCode        *int   `json:"rcode,omitempty"`
	RCodeName    string `json:"rcode_name,omitempty"`
	AnswerCount  int    `json:"answer_count,omitempty"`
	AddressCount int    `json:"address_count,omitempty"`
	DurationMS   int64  `json:"duration_ms"`
	Error        string `json:"error,omitempty"`
}

// InvalidResult returns a JSON-safe result for a rejected invocation.
func InvalidResult(server, name string, err error) Result {
	if strings.TrimSpace(name) == "" {
		name = DefaultName
	}
	result := Result{
		SchemaVersion: SchemaVersion,
		Server:        strings.TrimSpace(server),
		Name:          canonicalDisplayName(name),
		QueryType:     "A",
		FailureKind:   "usage",
		UDP:           TransportResult{Transport: "udp", FailureKind: "usage"},
		TCP:           TransportResult{Transport: "tcp", FailureKind: "usage"},
	}
	if err != nil {
		result.Error = err.Error()
	}
	return result
}

// Probe performs UDP and TCP probes under a hard total deadline. It returns a
// result rather than an operational error so callers can always emit the
// structured diagnostics, including partial success.
func Probe(ctx context.Context, options Options) Result {
	server, name, totalTimeout, transportTimeout, err := normalizeOptions(options)
	if err != nil {
		return InvalidResult(options.Server, options.Name, err)
	}

	probeCtx, cancel := context.WithTimeout(ctx, totalTimeout)
	defer cancel()

	result := Result{
		SchemaVersion: SchemaVersion,
		Server:        server,
		Name:          name,
		QueryType:     "A",
	}
	result.UDP = probeTransport(probeCtx, "udp", server, name, transportTimeout)
	result.TCP = probeTransport(probeCtx, "tcp", server, name, transportTimeout)
	result.PathOK = result.UDP.PathOK && result.TCP.PathOK
	result.ResolutionOK = result.UDP.ResolutionOK && result.TCP.ResolutionOK
	switch {
	case !result.PathOK:
		result.FailureKind = "path"
	case !result.ResolutionOK:
		result.FailureKind = "resolution"
	}
	return result
}

func (r Result) ExitCode() int {
	switch {
	case r.FailureKind == "usage":
		return ExitUsage
	case !r.PathOK:
		return ExitPath
	case !r.ResolutionOK:
		return ExitResolution
	default:
		return ExitOK
	}
}

func normalizeOptions(options Options) (server, name string, totalTimeout, transportTimeout time.Duration, err error) {
	server, err = normalizeServer(options.Server)
	if err != nil {
		return "", "", 0, 0, err
	}
	name, err = normalizeName(options.Name)
	if err != nil {
		return "", "", 0, 0, err
	}
	totalTimeout = options.TotalTimeout
	if totalTimeout == 0 {
		totalTimeout = DefaultTotalTimeout
	}
	if totalTimeout < 0 {
		return "", "", 0, 0, fmt.Errorf("total timeout must be positive")
	}
	transportTimeout = options.TransportTimeout
	if transportTimeout == 0 {
		transportTimeout = DefaultTransportTimeout
	}
	if transportTimeout < 0 {
		return "", "", 0, 0, fmt.Errorf("transport timeout must be positive")
	}
	if totalTimeout > time.Minute {
		return "", "", 0, 0, fmt.Errorf("total timeout must not exceed 1m")
	}
	if transportTimeout > 30*time.Second {
		return "", "", 0, 0, fmt.Errorf("transport timeout must not exceed 30s")
	}
	return server, name, totalTimeout, transportTimeout, nil
}

func normalizeServer(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("server is required")
	}
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		if strings.Contains(value, ":") {
			trimmed := strings.Trim(value, "[]")
			if ip := net.ParseIP(trimmed).To4(); ip != nil {
				return net.JoinHostPort(ip.String(), DefaultPort), nil
			}
			return "", fmt.Errorf("server must be an IP address with an optional port: %w", err)
		}
		host, port = value, DefaultPort
	}
	host = strings.Trim(host, "[]")
	ip := net.ParseIP(host).To4()
	if ip == nil {
		return "", fmt.Errorf("server host must be an IPv4 address")
	}
	if port == "" {
		port = DefaultPort
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return "", fmt.Errorf("server port must be an integer from 1 to 65535")
	}
	return net.JoinHostPort(ip.String(), strconv.Itoa(portNumber)), nil
}

func normalizeName(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = DefaultName
	}
	value = strings.TrimSuffix(value, ".")
	if value == "" {
		return "", fmt.Errorf("query name must not be the DNS root")
	}
	if len(value) > 253 {
		return "", fmt.Errorf("query name exceeds 253 bytes")
	}
	for _, label := range strings.Split(value, ".") {
		if len(label) == 0 {
			return "", fmt.Errorf("query name contains an empty label")
		}
		if len(label) > 63 {
			return "", fmt.Errorf("query name label exceeds 63 bytes")
		}
	}
	return strings.ToLower(value) + ".", nil
}

func canonicalDisplayName(value string) string {
	name, err := normalizeName(value)
	if err != nil {
		return strings.TrimSpace(value)
	}
	return name
}

func probeTransport(ctx context.Context, transport, server, name string, timeout time.Duration) (result TransportResult) {
	started := time.Now()
	result = TransportResult{Transport: transport}
	defer func() {
		result.DurationMS = time.Since(started).Milliseconds()
	}()

	query, transactionID, err := buildQuery(name)
	if err != nil {
		return transportFailure(result, "path", "build_query", err)
	}
	exchangeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	response, err := exchangeTransport(exchangeCtx, transport, server, query)
	if err != nil {
		return transportFailure(result, "path", exchangePhase(err), contextError(exchangeCtx, err))
	}

	parsed, err := validateResponse(response, transactionID, name)
	if err != nil {
		return transportFailure(result, "path", "validate_response", err)
	}
	if transport == "udp" && parsed.truncated {
		// TC is not a resolution failure: RFC-compliant resolvers retry the same
		// query over TCP. Keep that retry inside the UDP leg's existing bounded
		// deadline so a truncated datagram cannot extend readiness indefinitely.
		response, err = exchangeTransport(exchangeCtx, "tcp", server, query)
		if err != nil {
			return transportFailure(result, "path", exchangePhase(err), contextError(exchangeCtx, err))
		}
		parsed, err = validateResponse(response, transactionID, name)
		if err != nil {
			return transportFailure(result, "path", "validate_response", err)
		}
	}
	result.PathOK = true
	result.RCode = &parsed.rcode
	result.RCodeName = rcodeName(parsed.rcode)
	result.AnswerCount = parsed.answerCount
	result.AddressCount = parsed.addressCount
	if parsed.rcode != 0 {
		return transportFailure(result, "resolution", "resolve", fmt.Errorf("DNS response code %s", result.RCodeName))
	}
	if parsed.truncated {
		return transportFailure(result, "resolution", "resolve", fmt.Errorf("DNS response remained truncated over TCP"))
	}
	if parsed.addressCount == 0 {
		return transportFailure(result, "resolution", "resolve", fmt.Errorf("DNS response contained no A records"))
	}
	result.ResolutionOK = true
	return result
}

func exchangeTransport(ctx context.Context, transport, server string, query []byte) ([]byte, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, transport+"4", server)
	if err != nil {
		return nil, &phaseError{phase: "dial", err: err}
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return nil, &phaseError{phase: "set_deadline", err: err}
		}
	}
	if transport == "tcp" {
		return exchangeTCP(conn, query)
	}
	return exchangeUDP(conn, query)
}

type phaseError struct {
	phase string
	err   error
}

func (e *phaseError) Error() string { return e.err.Error() }
func (e *phaseError) Unwrap() error { return e.err }

func exchangePhase(err error) string {
	var phased *phaseError
	if errors.As(err, &phased) {
		return phased.phase
	}
	return "exchange"
}

func exchangeUDP(conn net.Conn, query []byte) ([]byte, error) {
	if n, err := conn.Write(query); err != nil {
		return nil, &phaseError{phase: "write", err: fmt.Errorf("write UDP DNS query: %w", err)}
	} else if n != len(query) {
		return nil, &phaseError{phase: "write", err: fmt.Errorf("write UDP DNS query: %w", io.ErrShortWrite)}
	}
	response := make([]byte, maxDNSMessage)
	n, err := conn.Read(response)
	if err != nil {
		return nil, &phaseError{phase: "read", err: fmt.Errorf("read UDP DNS response: %w", err)}
	}
	return response[:n], nil
}

func exchangeTCP(conn net.Conn, query []byte) ([]byte, error) {
	frame := make([]byte, 2+len(query))
	binary.BigEndian.PutUint16(frame[:2], uint16(len(query)))
	copy(frame[2:], query)
	if err := writeAll(conn, frame); err != nil {
		return nil, &phaseError{phase: "write", err: fmt.Errorf("write TCP DNS query: %w", err)}
	}
	var header [2]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return nil, &phaseError{phase: "read_frame", err: fmt.Errorf("read TCP DNS response length: %w", err)}
	}
	size := int(binary.BigEndian.Uint16(header[:]))
	if size < dnsHeaderSize {
		return nil, &phaseError{phase: "read_frame", err: fmt.Errorf("TCP DNS response length %d is shorter than a DNS header", size)}
	}
	response := make([]byte, size)
	if _, err := io.ReadFull(conn, response); err != nil {
		return nil, &phaseError{phase: "read", err: fmt.Errorf("read TCP DNS response: %w", err)}
	}
	return response, nil
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := writer.Write(data)
		if err != nil {
			return fmt.Errorf("write DNS probe payload: %w", err)
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func transportFailure(result TransportResult, kind, phase string, err error) TransportResult {
	result.FailureKind = kind
	result.Phase = phase
	if err != nil {
		result.Error = err.Error()
	}
	return result
}

func contextError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		// Preserve the stable probe result text while still retaining the context
		// error in the chain for errors.Is and satisfying the wrapping contract.
		return fmt.Errorf("%w", ctx.Err())
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return context.DeadlineExceeded
	}
	return err
}

func buildQuery(name string) ([]byte, uint16, error) {
	name, err := normalizeName(name)
	if err != nil {
		return nil, 0, err
	}
	var idBytes [2]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		return nil, 0, fmt.Errorf("generate DNS transaction ID: %w", err)
	}
	id := binary.BigEndian.Uint16(idBytes[:])
	dnsName, err := dnsmessage.NewName(name)
	if err != nil {
		return nil, 0, fmt.Errorf("encode DNS query name: %w", err)
	}
	builder := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: id, RecursionDesired: true})
	if err := builder.StartQuestions(); err != nil {
		return nil, 0, fmt.Errorf("start DNS question: %w", err)
	}
	if err := builder.Question(dnsmessage.Question{Name: dnsName, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}); err != nil {
		return nil, 0, fmt.Errorf("encode DNS question: %w", err)
	}
	query, err := builder.Finish()
	if err != nil {
		return nil, 0, fmt.Errorf("finish DNS query: %w", err)
	}
	return query, id, nil
}

type parsedResponse struct {
	rcode        int
	answerCount  int
	addressCount int
	truncated    bool
}

func validateResponse(response []byte, transactionID uint16, name string) (parsedResponse, error) {
	if len(response) < dnsHeaderSize {
		return parsedResponse{}, fmt.Errorf("DNS response is shorter than %d-byte header", dnsHeaderSize)
	}
	var parser dnsmessage.Parser
	header, err := parser.Start(response)
	if err != nil {
		return parsedResponse{}, fmt.Errorf("parse DNS response header: %w", err)
	}
	if header.ID != transactionID {
		return parsedResponse{}, fmt.Errorf("DNS transaction ID %04x does not match query %04x", header.ID, transactionID)
	}
	if !header.Response {
		return parsedResponse{}, fmt.Errorf("DNS response bit is not set")
	}
	questions, err := parser.AllQuestions()
	if err != nil {
		return parsedResponse{}, fmt.Errorf("parse DNS response questions: %w", err)
	}
	if len(questions) != 1 {
		return parsedResponse{}, fmt.Errorf("DNS response question count is %d, want 1", len(questions))
	}
	if questions[0].Type != dnsmessage.TypeA || questions[0].Class != dnsmessage.ClassINET {
		return parsedResponse{}, fmt.Errorf("DNS response question is %s/%s, want A/INET", questions[0].Type, questions[0].Class)
	}
	if !strings.EqualFold(questions[0].Name.String(), name) {
		return parsedResponse{}, fmt.Errorf("DNS response question name is %q, want %q", questions[0].Name.String(), name)
	}
	if header.Truncated {
		// A truncated UDP message may end part-way through an RR. The validated
		// header and question are sufficient to authorize a TCP retry; its answer
		// section is the one that must satisfy the strict resolution semantics.
		return parsedResponse{rcode: int(header.RCode), truncated: true}, nil
	}
	answers, err := parser.AllAnswers()
	if err != nil {
		return parsedResponse{}, fmt.Errorf("parse DNS response answers: %w", err)
	}
	addressCount, err := relevantAddressCount(answers, name)
	if err != nil {
		return parsedResponse{}, err
	}
	return parsedResponse{
		rcode:        int(header.RCode),
		answerCount:  len(answers),
		addressCount: addressCount,
	}, nil
}

func relevantAddressCount(answers []dnsmessage.Resource, queryName string) (int, error) {
	cnameTargets := make(map[string][]string)
	for _, answer := range answers {
		if answer.Header.Class != dnsmessage.ClassINET {
			continue
		}
		switch answer.Header.Type {
		case dnsmessage.TypeA:
			if _, ok := answer.Body.(*dnsmessage.AResource); !ok {
				return 0, fmt.Errorf("DNS A answer has an invalid body")
			}
		case dnsmessage.TypeCNAME:
			body, ok := answer.Body.(*dnsmessage.CNAMEResource)
			if !ok {
				return 0, fmt.Errorf("DNS CNAME answer has an invalid body")
			}
			owner := canonicalDNSResponseName(answer.Header.Name.String())
			target := canonicalDNSResponseName(body.CNAME.String())
			cnameTargets[owner] = append(cnameTargets[owner], target)
		}
	}

	terminal := canonicalDNSResponseName(queryName)
	seen := make(map[string]bool, len(cnameTargets)+1)
	for {
		if seen[terminal] {
			return 0, fmt.Errorf("DNS CNAME chain contains a loop")
		}
		seen[terminal] = true
		targets := cnameTargets[terminal]
		if len(targets) == 0 {
			break
		}
		for _, target := range targets[1:] {
			if target != targets[0] {
				return 0, fmt.Errorf("DNS CNAME owner %q has conflicting targets", terminal)
			}
		}
		terminal = targets[0]
	}

	addressCount := 0
	for _, answer := range answers {
		if answer.Header.Type != dnsmessage.TypeA || answer.Header.Class != dnsmessage.ClassINET || canonicalDNSResponseName(answer.Header.Name.String()) != terminal {
			continue
		}
		if _, ok := answer.Body.(*dnsmessage.AResource); !ok {
			return 0, fmt.Errorf("DNS A answer has an invalid body")
		}
		addressCount++
	}
	return addressCount, nil
}

func canonicalDNSResponseName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func rcodeName(rcode int) string {
	switch rcode {
	case 0:
		return "NOERROR"
	case 1:
		return "FORMERR"
	case 2:
		return "SERVFAIL"
	case 3:
		return "NXDOMAIN"
	case 4:
		return "NOTIMP"
	case 5:
		return "REFUSED"
	default:
		return fmt.Sprintf("RCODE%d", rcode)
	}
}
