package proxy

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SOCKS5ProxyServer is a local SOCKS5 proxy that forwards to an authenticated upstream proxy
type SOCKS5ProxyServer struct {
	listenAddr     string
	upstreamURL    string
	listener       net.Listener
	shutdown       chan struct{}
	wg             sync.WaitGroup
	running        bool
	mu             sync.Mutex
	activeConns    int
	connMu         sync.Mutex
	connectTimeout time.Duration
	relayTimeout   time.Duration
	
	// Circuit breaker for upstream failures
	upstreamFailures    int
	lastUpstreamFailure time.Time
	upstreamMu          sync.Mutex
}

// NewSOCKS5ProxyServer creates a new SOCKS5 proxy server
func NewSOCKS5ProxyServer(listenAddr, upstreamURL string) *SOCKS5ProxyServer {
	return &SOCKS5ProxyServer{
		listenAddr:     listenAddr,
		upstreamURL:    upstreamURL,
		shutdown:       make(chan struct{}),
		connectTimeout: 30 * time.Second, // Connection timeout
		relayTimeout:   5 * time.Minute,  // Data relay timeout
	}
}

// Start starts the SOCKS5 proxy server
func (s *SOCKS5ProxyServer) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running {
		return fmt.Errorf("proxy server already running")
	}

	listener, err := net.Listen("tcp", s.listenAddr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", s.listenAddr, err)
	}

	s.listener = listener
	s.running = true

	slog.Info("SOCKS5 proxy server started", 
		"listen_addr", s.listenAddr, 
		"upstream", s.upstreamURL,
		"connect_timeout", s.connectTimeout,
		"relay_timeout", s.relayTimeout)

	s.wg.Add(1)
	go s.acceptLoop()

	return nil
}

// Stop stops the SOCKS5 proxy server
func (s *SOCKS5ProxyServer) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.running {
		return nil
	}

	close(s.shutdown)
	s.running = false

	if s.listener != nil {
		s.listener.Close()
	}

	s.wg.Wait()
	slog.Info("SOCKS5 proxy server stopped")
	return nil
}

// acceptLoop accepts incoming connections
func (s *SOCKS5ProxyServer) acceptLoop() {
	defer s.wg.Done()

	for {
		select {
		case <-s.shutdown:
			return
		default:
		}

		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.shutdown:
				return
			default:
				slog.Error("Failed to accept connection", "error", err)
				continue
			}
		}

		s.wg.Add(1)
		go s.handleConnection(conn)
	}
}

// handleConnection handles a single client connection
func (s *SOCKS5ProxyServer) handleConnection(clientConn net.Conn) {
	defer s.wg.Done()
	defer clientConn.Close()
	
	// Track active connections for monitoring
	s.connMu.Lock()
	s.activeConns++
	s.connMu.Unlock()
	
	defer func() {
		s.connMu.Lock()
		s.activeConns--
		s.connMu.Unlock()
	}()
	
	// Set connection timeout
	clientConn.SetDeadline(time.Now().Add(s.connectTimeout))

	// Step 1: SOCKS5 handshake
	if err := s.handleHandshake(clientConn); err != nil {
		slog.Error("SOCKS5 handshake failed", "error", err)
		return
	}

	// Step 2: Parse CONNECT request
	targetAddr, err := s.handleConnectRequest(clientConn)
	if err != nil {
		slog.Error("SOCKS5 connect request failed", "error", err)
		return
	}

	// Step 3: Check if upstream is healthy before attempting connection
	if !s.isUpstreamHealthy() {
		slog.Warn("Upstream proxy is unhealthy, rejecting connection", "target", targetAddr)
		s.sendConnectResponse(clientConn, 0x01) // General failure
		return
	}

	// Step 4: Connect to upstream proxy
	upstreamConn, err := s.connectToUpstream(targetAddr)
	if err != nil {
		slog.Error("Failed to connect to upstream", "error", err, "target", targetAddr)
		s.recordUpstreamFailure()
		s.sendConnectResponse(clientConn, 0x01) // General failure
		return
	}
	defer upstreamConn.Close()
	
	// Reset failure count on successful connection
	s.resetUpstreamFailures()

	// Step 5: Send success response
	if err := s.sendConnectResponse(clientConn, 0x00); err != nil {
		slog.Error("Failed to send connect response", "error", err)
		return
	}

	// Step 6: Clear connection deadline for data relay phase
	clientConn.SetDeadline(time.Time{})
	upstreamConn.SetDeadline(time.Time{})
	
	// Step 7: Relay data between client and upstream
	s.relayConnections(clientConn, upstreamConn)
}

// handleHandshake performs SOCKS5 initial handshake
func (s *SOCKS5ProxyServer) handleHandshake(conn net.Conn) error {
	// Read client greeting header
	header := make([]byte, 2)
	if _, err := io.ReadFull(conn, header); err != nil {
		return fmt.Errorf("failed to read greeting header: %w", err)
	}

	version := header[0]
	nmethods := header[1]

	if version != 0x05 {
		return fmt.Errorf("unsupported SOCKS version: %d", version)
	}

	// Read authentication methods
	methods := make([]byte, nmethods)
	if _, err := io.ReadFull(conn, methods); err != nil {
		return fmt.Errorf("failed to read auth methods: %w", err)
	}

	// Check if "no authentication" (0x00) is supported
	noAuthSupported := false
	for _, method := range methods {
		if method == 0x00 {
			noAuthSupported = true
			break
		}
	}

	if !noAuthSupported {
		// Return "no acceptable methods"
		response := []byte{0x05, 0xFF}
		conn.Write(response)
		return fmt.Errorf("client doesn't support no-authentication method")
	}

	// Respond with "no authentication required"
	response := []byte{0x05, 0x00}
	if _, err := conn.Write(response); err != nil {
		return fmt.Errorf("failed to send handshake response: %w", err)
	}

	return nil
}

// handleConnectRequest parses the SOCKS5 CONNECT request
func (s *SOCKS5ProxyServer) handleConnectRequest(conn net.Conn) (string, error) {
	// Read request header
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return "", fmt.Errorf("failed to read request header: %w", err)
	}

	version := buf[0]
	cmd := buf[1]
	atyp := buf[3]

	if version != 0x05 {
		return "", fmt.Errorf("unsupported SOCKS version: %d", version)
	}

	if cmd != 0x01 { // CONNECT
		return "", fmt.Errorf("unsupported command: %d", cmd)
	}

	var host string
	var port uint16

	switch atyp {
	case 0x01: // IPv4
		addr := make([]byte, 4)
		if _, err := io.ReadFull(conn, addr); err != nil {
			return "", fmt.Errorf("failed to read IPv4 address: %w", err)
		}
		host = net.IP(addr).String()

	case 0x03: // Domain name
		lenBuf := make([]byte, 1)
		if _, err := io.ReadFull(conn, lenBuf); err != nil {
			return "", fmt.Errorf("failed to read domain length: %w", err)
		}
		
		domainLen := lenBuf[0]
		domain := make([]byte, domainLen)
		if _, err := io.ReadFull(conn, domain); err != nil {
			return "", fmt.Errorf("failed to read domain name: %w", err)
		}
		host = string(domain)

	case 0x04: // IPv6
		addr := make([]byte, 16)
		if _, err := io.ReadFull(conn, addr); err != nil {
			return "", fmt.Errorf("failed to read IPv6 address: %w", err)
		}
		host = net.IP(addr).String()

	default:
		return "", fmt.Errorf("unsupported address type: %d", atyp)
	}

	// Read port
	portBuf := make([]byte, 2)
	if _, err := io.ReadFull(conn, portBuf); err != nil {
		return "", fmt.Errorf("failed to read port: %w", err)
	}
	port = uint16(portBuf[0])<<8 | uint16(portBuf[1])

	return net.JoinHostPort(host, strconv.Itoa(int(port))), nil
}

// connectToUpstream connects to the upstream SOCKS5 proxy with authentication
func (s *SOCKS5ProxyServer) connectToUpstream(targetAddr string) (net.Conn, error) {
	// Parse upstream URL, handling socks5h:// protocol variant
	upstreamURL := s.upstreamURL
	if strings.HasPrefix(upstreamURL, "socks5h://") {
		// Convert socks5h:// to socks5:// for parsing
		// The 'h' variant means DNS resolution should be done by proxy (which we always do anyway)
		upstreamURL = strings.Replace(upstreamURL, "socks5h://", "socks5://", 1)
	}
	
	u, err := url.Parse(upstreamURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse upstream URL: %w", err)
	}

	// Add default port if missing
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		// No port specified, add default SOCKS5 port
		host = u.Host
		port = "1080"
	}
	
	// Properly format IPv6 addresses
	upstreamAddr := net.JoinHostPort(host, port)

	// Connect to upstream proxy with timeout
	dialer := net.Dialer{
		Timeout: s.connectTimeout,
	}
	upstreamConn, err := dialer.Dial("tcp", upstreamAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to upstream: %w", err)
	}

	// Perform SOCKS5 handshake with upstream (with auth)
	if err := s.authenticateUpstream(upstreamConn, u.User); err != nil {
		upstreamConn.Close()
		return nil, fmt.Errorf("upstream authentication failed: %w", err)
	}

	// Send CONNECT request to upstream
	if err := s.sendUpstreamConnect(upstreamConn, targetAddr); err != nil {
		upstreamConn.Close()
		return nil, fmt.Errorf("upstream connect failed: %w", err)
	}

	return upstreamConn, nil
}

// authenticateUpstream performs SOCKS5 authentication with the upstream proxy
func (s *SOCKS5ProxyServer) authenticateUpstream(conn net.Conn, userInfo *url.Userinfo) error {
	// Send greeting with both no-auth and username/password methods
	greeting := []byte{0x05, 0x02, 0x00, 0x02} // Version 5, 2 methods: no-auth (0x00) and username/password (0x02)
	if _, err := conn.Write(greeting); err != nil {
		return fmt.Errorf("failed to send greeting: %w", err)
	}

	// Read response
	response := make([]byte, 2)
	if _, err := io.ReadFull(conn, response); err != nil {
		return fmt.Errorf("failed to read greeting response: %w", err)
	}

	if response[0] != 0x05 {
		return fmt.Errorf("upstream proxy responded with wrong SOCKS version: %d", response[0])
	}

	selectedMethod := response[1]
	switch selectedMethod {
	case 0x00:
		// No authentication required
		return nil
		
	case 0x02:
		// Username/password authentication required
		if userInfo == nil {
			return fmt.Errorf("upstream proxy requires authentication but no credentials provided")
		}

		username := userInfo.Username()
		password, _ := userInfo.Password()

		authReq := make([]byte, 0, 3+len(username)+len(password))
		authReq = append(authReq, 0x01) // Version 1
		authReq = append(authReq, byte(len(username)))
		authReq = append(authReq, username...)
		authReq = append(authReq, byte(len(password)))
		authReq = append(authReq, password...)

		if _, err := conn.Write(authReq); err != nil {
			return fmt.Errorf("failed to send auth request: %w", err)
		}

		// Read auth response
		authResp := make([]byte, 2)
		if _, err := io.ReadFull(conn, authResp); err != nil {
			return fmt.Errorf("failed to read auth response: %w", err)
		}

		if authResp[1] != 0x00 {
			return fmt.Errorf("authentication failed")
		}
		return nil
		
	case 0xFF:
		return fmt.Errorf("upstream proxy rejected all authentication methods")
		
	default:
		return fmt.Errorf("upstream proxy selected unsupported auth method: %d", selectedMethod)
	}
}

// sendUpstreamConnect sends a CONNECT request to the upstream proxy
func (s *SOCKS5ProxyServer) sendUpstreamConnect(conn net.Conn, targetAddr string) error {
	host, portStr, err := net.SplitHostPort(targetAddr)
	if err != nil {
		return fmt.Errorf("invalid target address: %w", err)
	}

	port, err := strconv.Atoi(portStr)
	if err != nil {
		return fmt.Errorf("invalid port: %w", err)
	}

	// Build CONNECT request
	req := []byte{0x05, 0x01, 0x00} // Version, CONNECT, Reserved

	// Add address type and address
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			// IPv4
			req = append(req, 0x01)
			req = append(req, ip4...)
		} else {
			// IPv6
			req = append(req, 0x04)
			req = append(req, ip.To16()...)
		}
	} else {
		// Domain name
		req = append(req, 0x03)
		req = append(req, byte(len(host)))
		req = append(req, host...)
	}

	// Add port
	req = append(req, byte(port>>8), byte(port&0xff))

	// Send request
	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("failed to send connect request: %w", err)
	}

	// Read response
	resp := make([]byte, 4)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return fmt.Errorf("failed to read connect response: %w", err)
	}

	if resp[1] != 0x00 {
		return fmt.Errorf("connect request failed with code: %d", resp[1])
	}

	// Read bound address (we don't need it, but must consume it)
	// Only read if the response was successful (REP = 0x00)
	atyp := resp[3]
	switch atyp {
	case 0x01: // IPv4
		_, err = io.ReadFull(conn, make([]byte, 4+2)) // 4 bytes IP + 2 bytes port
	case 0x03: // Domain
		lenBuf := make([]byte, 1)
		if _, err = io.ReadFull(conn, lenBuf); err == nil {
			_, err = io.ReadFull(conn, make([]byte, int(lenBuf[0])+2)) // domain + port
		}
	case 0x04: // IPv6
		_, err = io.ReadFull(conn, make([]byte, 16+2)) // 16 bytes IP + 2 bytes port
	}

	return err
}

// sendConnectResponse sends a SOCKS5 CONNECT response to the client
func (s *SOCKS5ProxyServer) sendConnectResponse(conn net.Conn, status byte) error {
	// Simple response with IPv4 0.0.0.0:0
	response := []byte{
		0x05,       // Version
		status,     // Status
		0x00,       // Reserved
		0x01,       // IPv4
		0x00, 0x00, 0x00, 0x00, // IP 0.0.0.0
		0x00, 0x00, // Port 0
	}

	_, err := conn.Write(response)
	return err
}

// relayConnections relays data between two connections with timeout handling
func (s *SOCKS5ProxyServer) relayConnections(conn1, conn2 net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)

	relay := func(dst, src net.Conn, direction string) {
		defer wg.Done()
		defer dst.Close()
		defer src.Close()
		
		// Set read/write timeouts for the relay phase
		ticker := time.NewTicker(s.relayTimeout)
		defer ticker.Stop()
		
		// Channel to signal completion
		done := make(chan struct{})
		
		go func() {
			defer close(done)
			_, err := io.Copy(dst, src)
			if err != nil && !isConnectionClosed(err) {
				slog.Debug("Relay connection error", "direction", direction, "error", err)
			}
		}()
		
		// Wait for completion or timeout
		select {
		case <-done:
			// Normal completion
		case <-ticker.C:
			slog.Debug("Relay connection timeout", "direction", direction, "timeout", s.relayTimeout)
		}
	}

	go relay(conn1, conn2, "client->upstream")
	go relay(conn2, conn1, "upstream->client")

	wg.Wait()
}

// isConnectionClosed checks if an error indicates a closed connection
func isConnectionClosed(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "use of closed network connection") ||
		strings.Contains(err.Error(), "connection reset by peer") ||
		strings.Contains(err.Error(), "broken pipe")
}

// isUpstreamHealthy checks if the upstream proxy is considered healthy
func (s *SOCKS5ProxyServer) isUpstreamHealthy() bool {
	s.upstreamMu.Lock()
	defer s.upstreamMu.Unlock()
	
	// If we have recent failures, implement exponential backoff
	if s.upstreamFailures > 0 {
		backoffDuration := time.Duration(s.upstreamFailures*s.upstreamFailures) * time.Second
		if backoffDuration > 60*time.Second {
			backoffDuration = 60 * time.Second // Max 60 second backoff
		}
		
		if time.Since(s.lastUpstreamFailure) < backoffDuration {
			return false
		}
	}
	
	return true
}

// recordUpstreamFailure records a failure to connect to upstream
func (s *SOCKS5ProxyServer) recordUpstreamFailure() {
	s.upstreamMu.Lock()
	defer s.upstreamMu.Unlock()
	
	s.upstreamFailures++
	s.lastUpstreamFailure = time.Now()
	
	slog.Warn("Upstream proxy failure recorded", 
		"failure_count", s.upstreamFailures, 
		"last_failure", s.lastUpstreamFailure)
}

// resetUpstreamFailures resets the failure counter on successful connection
func (s *SOCKS5ProxyServer) resetUpstreamFailures() {
	s.upstreamMu.Lock()
	defer s.upstreamMu.Unlock()
	
	if s.upstreamFailures > 0 {
		slog.Info("Upstream proxy connection successful, resetting failure count", 
			"previous_failures", s.upstreamFailures)
		s.upstreamFailures = 0
	}
}

// StartProxyServer starts a SOCKS5 proxy server if upstream proxy is configured
func StartProxyServer() (*SOCKS5ProxyServer, error) {
	upstreamProxy := os.Getenv("SOCKS5_PROXY")
	if upstreamProxy == "" {
		return nil, nil // No proxy configured
	}

	localAddr := "127.0.0.1:7777"
	server := NewSOCKS5ProxyServer(localAddr, upstreamProxy)
	
	if err := server.Start(); err != nil {
		return nil, fmt.Errorf("failed to start proxy server: %w", err)
	}

	return server, nil
}

// GetProxyURL returns the appropriate proxy URL to use throughout the application.
// If SOCKS5_PROXY is configured, returns the local proxy server URL.
// Otherwise returns empty string.
func GetProxyURL() string {
	upstreamProxy := os.Getenv("SOCKS5_PROXY")
	if upstreamProxy == "" {
		return "" // No proxy configured
	}
	
	// Return local proxy server address
	return "socks5://127.0.0.1:7777"
}

// IsProxyEnabled returns true if proxy is configured
func IsProxyEnabled() bool {
	return os.Getenv("SOCKS5_PROXY") != ""
}

// ConnectionStats returns current connection statistics
type ConnectionStats struct {
	ActiveConnections   int    `json:"active_connections"`
	IsRunning           bool   `json:"is_running"`
	ConnectTimeout      string `json:"connect_timeout"`
	RelayTimeout        string `json:"relay_timeout"`
	UpstreamFailures    int    `json:"upstream_failures"`
	UpstreamHealthy     bool   `json:"upstream_healthy"`
	LastUpstreamFailure string `json:"last_upstream_failure,omitempty"`
}

// GetConnectionStats returns current connection statistics
func (s *SOCKS5ProxyServer) GetConnectionStats() ConnectionStats {
	s.mu.Lock()
	s.connMu.Lock()
	s.upstreamMu.Lock()
	defer s.upstreamMu.Unlock()
	defer s.connMu.Unlock()
	defer s.mu.Unlock()
	
	stats := ConnectionStats{
		ActiveConnections: s.activeConns,
		IsRunning:         s.running,
		ConnectTimeout:    s.connectTimeout.String(),
		RelayTimeout:      s.relayTimeout.String(),
		UpstreamFailures:  s.upstreamFailures,
		UpstreamHealthy:   s.isUpstreamHealthyUnsafe(),
	}
	
	if !s.lastUpstreamFailure.IsZero() {
		stats.LastUpstreamFailure = s.lastUpstreamFailure.Format(time.RFC3339)
	}
	
	return stats
}

// isUpstreamHealthyUnsafe checks upstream health without locking (caller must hold upstreamMu)
func (s *SOCKS5ProxyServer) isUpstreamHealthyUnsafe() bool {
	if s.upstreamFailures > 0 {
		backoffDuration := time.Duration(s.upstreamFailures*s.upstreamFailures) * time.Second
		if backoffDuration > 60*time.Second {
			backoffDuration = 60 * time.Second
		}
		return time.Since(s.lastUpstreamFailure) >= backoffDuration
	}
	return true
}
