package controller

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/url"
	"time"
)

// ingressLeafValidity is how long a generated endpoint certificate is
// valid; ingressLeafRenewalMargin is how much remaining validity forces a
// re-issue on reconcile.
const (
	ingressLeafValidity      = 30 * 24 * time.Hour
	ingressLeafRenewalMargin = 24 * time.Hour
)

// ingressCA holds a parsed CA for signing endpoint certificates.
type ingressCA struct {
	certificate *x509.Certificate
	privateKey  interface{}
}

// parseIngressCA reads a CA certificate and key from PEM blocks.
func parseIngressCA(certificatePEM, keyPEM []byte) (*ingressCA, error) {
	certificateBlock, _ := pem.Decode(certificatePEM)
	if certificateBlock == nil {
		return nil, fmt.Errorf("ca.crt is not valid PEM")
	}
	certificate, err := x509.ParseCertificate(certificateBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parsing ca.crt: %w", err)
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, fmt.Errorf("ca.key is not valid PEM")
	}
	privateKey, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		if privateKey, err = x509.ParsePKCS1PrivateKey(keyBlock.Bytes); err != nil {
			if privateKey, err = x509.ParseECPrivateKey(keyBlock.Bytes); err != nil {
				return nil, fmt.Errorf("parsing ca.key: unsupported key type")
			}
		}
	}
	return &ingressCA{certificate: certificate, privateKey: privateKey}, nil
}

// signIngressLeaf issues a certificate for the endpoint host, signed by
// the CA. Returns the leaf certificate and its key, both PEM-encoded.
func (ca *ingressCA) signIngressLeaf(host string) (certificatePEM, keyPEM []byte, err error) {
	leafKey, err := ecdsaGenerateKey()
	if err != nil {
		return nil, nil, err
	}

	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}

	uri, err := url.Parse("spiffe://aigrid.llm.redhat.com/ingress/" + host)
	if err != nil {
		return nil, nil, err
	}

	template := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		URIs:         []*url.URL{uri},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(ingressLeafValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, ca.certificate, leafKey.Public(), ca.privateKey)
	if err != nil {
		return nil, nil, fmt.Errorf("signing ingress leaf: %w", err)
	}
	certificatePEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	keyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		return nil, nil, err
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certificatePEM, keyPEM, nil
}

// ingressLeafValid reports whether the PEM certificate on the route still
// covers the host with renewal margin to spare.
func ingressLeafValid(certificatePEM []byte, host string) bool {
	block, _ := pem.Decode(certificatePEM)
	if block == nil {
		return false
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}
	if certificate.NotAfter.Before(time.Now().Add(ingressLeafRenewalMargin)) {
		return false
	}
	for _, name := range certificate.DNSNames {
		if name == host {
			return true
		}
	}
	return false
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	return rand.Int(rand.Reader, limit)
}

func ecdsaGenerateKey() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}
