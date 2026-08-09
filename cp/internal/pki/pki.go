package pki

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path"
	"path/filepath"
	"time"
)

type CertificateManager struct {
	caCert     *x509.Certificate
	caDer      []byte
	serverDer  []byte
	serverCert *x509.Certificate
	serverKey  *rsa.PrivateKey
	caKey      *rsa.PrivateKey
}

func (s *CertificateManager) InitCACert(pkiPath string, certPath string, keyPath string) error {
	//Checking if the key exists and if so loading it
	keyBytes, keyErr := os.ReadFile(path.Join(pkiPath, keyPath))
	certBytes, certErr := os.ReadFile(path.Join(pkiPath + certPath))
	if keyErr == nil && certErr == nil {
		keyPemBlock, _ := pem.Decode(keyBytes)
		if keyPemBlock == nil {
			return errors.New("no PEM block found")
		}
		keyBytes, err := x509.ParsePKCS8PrivateKey(keyPemBlock.Bytes)
		if err != nil {
			return errors.New("Error Parsing the PEM block")
		}
		s.caKey = keyBytes.(*rsa.PrivateKey)

		// Certificate Time
		certPemBlock, _ := pem.Decode(certBytes)
		caCert, err := x509.ParseCertificate(certPemBlock.Bytes)
		s.caDer = certPemBlock.Bytes
		s.caCert = caCert
		return nil
	}
	//If it doesnt exist, creating directory first. Hacky workaround so need to improve it later
	//TODO:Improve directory creation logic
	_ = os.MkdirAll(pkiPath, 0o700)
	privKey, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		return err
	}
	s.caKey = privKey
	//Saving CaKey
	err = s.saveKey(privKey, path.Join(pkiPath, keyPath))
	if err != nil {
		return err
	}
	template := x509.Certificate{
		NotBefore:             time.Now(),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCRLSign,
		SerialNumber:          big.NewInt(time.Now().Unix()),
		BasicConstraintsValid: true,
	}
	caDer, err := x509.CreateCertificate(rand.Reader, &template, &template, &s.caKey.PublicKey, s.caKey)
	if err != nil {
		return err
	}
	caCert, err := x509.ParseCertificate(caDer)
	if err != nil {
		return err
	}
	s.caCert = caCert
	err = s.saveCert(s.caDer, path.Join(pkiPath, certPath))
	return nil
}

// FIX:Not being saved apparently
func (s *CertificateManager) InitServerCert(pkiPath string, certPath string, keyPath string) error {
	certBytes, certErr := os.ReadFile(path.Join(pkiPath, certPath))
	keyBytes, keyErr := os.ReadFile(path.Join(pkiPath, keyPath))

	if keyErr == nil && certErr == nil {
		//Load the key and cert of server
		certPemBlock, _ := pem.Decode(certBytes)
		keyPemBlock, _ := pem.Decode(keyBytes)
		if certPemBlock == nil || keyPemBlock == nil {
			return errors.New("Error finding the PEM Block")
		}
		s.serverDer = certPemBlock.Bytes
		//Parsing Certificate
		serverCert, err := x509.ParseCertificate(certPemBlock.Bytes)
		if err != nil {
			return err
		}
		s.serverCert = serverCert

		//Loading the Key
		keyBytes, err := x509.ParsePKCS8PrivateKey(keyPemBlock.Bytes)
		if err != nil {
			return err
		}
		s.serverKey = keyBytes.(*rsa.PrivateKey)
		return nil
	}
	privKey, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		return err
	}
	s.serverKey = privKey
	//Saving key
	err = s.saveKey(privKey, path.Join(pkiPath, keyPath))
	if err != nil {
		return err
	}
	//Create server cert
	template := &x509.Certificate{
		NotBefore:    time.Now(),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:         false,
		SerialNumber: big.NewInt(time.Now().Unix()),
	}
	serverDer, err := x509.CreateCertificate(rand.Reader, template, s.caCert, &s.serverKey.PublicKey, s.caKey)
	if err != nil {
		return err
	}
	s.serverDer = serverDer
	serverCert, err := x509.ParseCertificate(s.serverDer)
	if err != nil {
		return err
	}
	s.serverCert = serverCert
	err = s.saveCert(s.serverDer, path.Join(pkiPath, certPath))
	if err != nil {
		return err
	}
	return nil
}

func (s *CertificateManager) saveCert(certificateDer []byte, path string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := pem.Encode(file, &pem.Block{Bytes: certificateDer, Type: "CERTIFICATE"}); err != nil {
		return err
	}
	return nil
}

func (s *CertificateManager) saveKey(key *rsa.PrivateKey, path string) error {
	rsaMarshallKey, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	pemBlock := &pem.Block{
		Bytes: rsaMarshallKey,
		Type:  "PRIVATE KEY",
	}

	//Opening and saving the file directly
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := pem.Encode(file, pemBlock); err != nil {
		return err
	}
	return nil
}
func (s *CertificateManager) CreateWorkerKeyCert(name string, path string) error {
	clientPriv, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		return err
	}

	certTemplate := x509.Certificate{
		SerialNumber: big.NewInt(time.Now().Unix()),
		NotBefore:    time.Now(),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageClientAuth,
		},
		Subject: pkix.Name{
			CommonName: name,
		},
		KeyUsage: x509.KeyUsageDigitalSignature,
	}

	derCert, err := x509.CreateCertificate(
		rand.Reader,
		&certTemplate,
		s.caCert,
		&clientPriv.PublicKey,
		s.caKey,
	)
	if err != nil {
		return err
	}

	// Ensure output directory exists.
	if err := os.MkdirAll(path, 0755); err != nil {
		return err
	}

	// Save certificate.
	certFile, err := os.Create(filepath.Join(path, name+".crt"))
	if err != nil {
		return err
	}
	defer certFile.Close()

	if err := pem.Encode(certFile, &pem.Block{
		Type:  "CERTIFICATE",
		Bytes: derCert,
	}); err != nil {
		return err
	}

	// Save private key.
	keyFile, err := os.OpenFile(
		filepath.Join(path, name+".key"),
		os.O_CREATE|os.O_WRONLY|os.O_TRUNC,
		0600,
	)
	if err != nil {
		return err
	}
	defer keyFile.Close()

	if err := pem.Encode(keyFile, &pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(clientPriv),
	}); err != nil {
		return err
	}

	return nil
}
