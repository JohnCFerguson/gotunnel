package tunnel

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"sync"
	"time"

	"github.com/johncferguson/gotunnel/internal/cert"
	"github.com/johncferguson/gotunnel/internal/mdns"
)

type Tunnel struct {
	Port      int
	HTTPSPort int
	Domain    string
	HTTPS     bool
	server    *http.Server
	listener  net.Listener
	done      chan struct{}
	Cert      *tls.Certificate
}

type Manager struct {
	tunnels     map[string]*Tunnel
	mu          sync.RWMutex
	mdns        *mdns.MDNSServer
	certManager *cert.CertManager
}

func NewManager() *Manager {
	m := &Manager{
		tunnels:     make(map[string]*Tunnel),
		mdns:        mdns.New(),
		certManager: cert.New("./certs"),
	}

	// Load existing tunnels
	// Verify mDNS registration by discovering services
	m.mdns.DiscoverServices()
	// states, err := state.LoadTunnels()
	// if err != nil {
	// 	log.Printf("Error loading tunnel state: %v", err)
	// 	return m
	// }

	// Start existing tunnels
	// for _, t := range states {
	// 	if err := m.StartTunnel(t.Port, t.Domain, t.HTTPS); err != nil {
	// 		log.Printf("Error restoring tunnel %s: %v", t.Domain, err)
	// 	}
	// }

	return m
}

func (m *Manager) StartTunnel(port int, domain string, https bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Prevent duplicate tunnels for the same domain
	if _, exists := m.tunnels[domain]; exists {
		return fmt.Errorf("tunnel for domain %s already exists", domain)
	}

	// Create new tunnel instance
	tunnel := &Tunnel{
		Port:   port,
		Domain: domain,
		HTTPS:  https,
		done:   make(chan struct{}), // Channel for cleanup signaling
	}

	// Ensure the SSL/TLS certificate is available
	if https {
		cert, err := m.certManager.EnsureCert(domain + ".local")
		if err != nil {
			return fmt.Errorf("failed to ensure certificate: %w", err)
		}
		// Set the certificate in the tunnel (assuming Tunnel struct has a field for it)
		tunnel.Cert = cert
		tunnel.HTTPSPort = 443

		// In tunnel.go, inside the start function:
		listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("0.0.0.0"), Port: tunnel.HTTPSPort})
		if err != nil {
			return fmt.Errorf("failed to create listener: %w", err)
		}

		// Wrap the listener with TLS
		tlsListener := tls.NewListener(listener, &tls.Config{
			Certificates: []tls.Certificate{*tunnel.Cert},
		})

		// Start accepting connections
		for {
			conn, err := tlsListener.Accept()
			if err != nil {
				log.Println("Error accepting connection:", err)
				continue
			}
			go handleConnection(conn, tunnel)
		}

	}

	tunnel.server = &http.Server{
		// ... other config ...
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{*tunnel.Cert}, // Load certificate
		},
	}
	// Start the HTTP server and set up the proxy
	if err := m.startTunnel(tunnel); err != nil {
		return fmt.Errorf("failed to start tunnel: %w", err)
	}

	// Add to internal map for tracking
	m.tunnels[domain] = tunnel

	// Persist tunnel configuration to disk
	// if err := m.saveTunnelState(); err != nil {
	// 	log.Printf("Error saving tunnel state: %v", err)
	// }

	// Register the domain with mDNS
	if err := m.mdns.RegisterDomain(domain); err != nil {
		// If mDNS registration fails, clean up everything
		delete(m.tunnels, domain)
		tunnel.stop()
		return fmt.Errorf("failed to register mDNS: %w", err)
	}

	log.Printf("Started tunnel: %s.local -> localhost:%d (HTTPS: %v)",
		domain, port, https)
	return nil
}

func (m *Manager) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var errs []error
	// Stop all tunnels
	for domain, tunnel := range m.tunnels {
		if err := tunnel.stop(); err != nil {
			errs = append(errs, fmt.Errorf("failed to stop tunnel %s: %w", domain, err))
		}
		// Unregister from mDNS
		if err := m.mdns.UnregisterDomain(domain); err != nil {
			errs = append(errs, fmt.Errorf("failed to unregister mDNS for %s: %w", domain, err))
		}
	}

	// Clear the tunnels map
	m.tunnels = make(map[string]*Tunnel)

	// Save empty state
	// if err := m.saveTunnelState(); err != nil {
	// 	log.Printf("Error saving tunnel state: %v", err)
	// }

	// If there were any errors, return them combined
	if len(errs) > 0 {
		var errMsg string
		for _, err := range errs {
			errMsg += err.Error() + "; "
		}
		return fmt.Errorf("errors while stopping tunnels: %s", errMsg)
	}

	return nil
}

func (m *Manager) startTunnel(t *Tunnel) error {
	// Create reverse proxy
	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = "https"
			req.URL.Host = fmt.Sprintf("localhost:%d", t.Port)
		},
	}

	// Create server
	t.server = &http.Server{
		Handler: proxy,
	}

	// Create listener
	var err error
	if t.HTTPS {
		// Generate or load certificate
		cert, err := m.certManager.EnsureCert(t.Domain + ".local")
		if err != nil {
			return fmt.Errorf("failed to ensure certificate: %w", err)
		}

		// Create TLS config
		tlsConfig := &tls.Config{
			Certificates: []tls.Certificate{*cert},
		}

		// Create TLS listener
		t.listener, err = tls.Listen("tcp", ":443", tlsConfig)
		if err != nil {
			return fmt.Errorf("failed to create TLS listener: %w", err)
		}
	} else {
		// Create regular HTTP listener
		t.listener, err = net.Listen("tcp", ":80")
		if err != nil {
			return fmt.Errorf("failed to create listener: %w", err)
		}
	}

	// Start server in goroutine
	go func() {
		if err := t.server.Serve(t.listener); err != http.ErrServerClosed {
			log.Printf("Server error: %v", err)
		}
	}()

	return nil
}

func (m *Manager) StopTunnel(domain string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	tunnel, exists := m.tunnels[domain]
	if !exists {
		return fmt.Errorf("tunnel for domain %s does not exist", domain)
	}

	// Stop the tunnel
	if err := tunnel.stop(); err != nil {
		return fmt.Errorf("failed to stop tunnel: %w", err)
	}

	// Unregister from mDNS
	if err := m.mdns.UnregisterDomain(domain); err != nil {
		return fmt.Errorf("failed to unregister mDNS: %w", err)
	}

	// Remove from tunnels map
	delete(m.tunnels, domain)
	log.Printf("Stopped tunnel: %s", domain)
	// if err := m.saveTunnelState(); err != nil {
	// 	log.Printf("Error saving tunnel state: %v", err)
	// }
	return nil
}

func (t *Tunnel) stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if t.server != nil {
		if err := t.server.Shutdown(ctx); err != nil {
			return fmt.Errorf("failed to shutdown server: %w", err)
		}
	}

	if t.listener != nil {
		if err := t.listener.Close(); err != nil {
			return fmt.Errorf("failed to close listener: %w", err)
		}
	}

	close(t.done)
	return nil
}

func (m *Manager) ListTunnels() []map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()

	tunnelList := make([]map[string]interface{}, 0, len(m.tunnels))
	for domain, tunnel := range m.tunnels {
		tunnelInfo := map[string]interface{}{
			"domain": domain,
			"port":   tunnel.Port,
			"https":  tunnel.HTTPS,
		}
		tunnelList = append(tunnelList, tunnelInfo)
	}

	return tunnelList
}

func handleConnection(clientConn net.Conn, tunnel *Tunnel) {
	defer clientConn.Close()

	// Connect to the local application
	localConn, err := net.Dial("tcp", fmt.Sprintf("localhost:%d", tunnel.Port))
	if err != nil {
		log.Println("Error connecting to local application:", err)
		return
	}
	defer localConn.Close()

	// Forward traffic between client and local application
	go io.Copy(localConn, clientConn)
	io.Copy(clientConn, localConn)
}

