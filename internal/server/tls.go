package server

import (
	"crypto/tls"
	"sync"
)

// CertReloader serves the certificate from disk and swaps it on Reload, so a
// renewed ACME certificate is picked up with SIGHUP instead of a restart.
type CertReloader struct {
	certFile, keyFile string
	mu                sync.RWMutex
	cert              *tls.Certificate
}

// NewCertReloader loads the pair once and fails if it can't.
func NewCertReloader(certFile, keyFile string) (*CertReloader, error) {
	c := &CertReloader{certFile: certFile, keyFile: keyFile}
	return c, c.Reload()
}

// Reload re-reads the files; on error the old certificate stays.
func (c *CertReloader) Reload() error {
	cert, err := tls.LoadX509KeyPair(c.certFile, c.keyFile)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.cert = &cert
	c.mu.Unlock()
	return nil
}

// TLSConfig is the server config using the current certificate.
func (c *CertReloader) TLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"h2", "http/1.1"},
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			c.mu.RLock()
			defer c.mu.RUnlock()
			return c.cert, nil
		},
	}
}
