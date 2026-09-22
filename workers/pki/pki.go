package worker_pki

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
)

type BootstrapToken struct {
	Name      string `json:"name"`
	CaDer     []byte `json:"caDer"`
	WorkerDer []byte `json:"workerDer"`
	WorkerKey []byte `json:"workerKey"`
}

type CertificateManager struct {
	name string

	caCert *x509.Certificate
	caDer  []byte

	workerCert *x509.Certificate
	workerDer  []byte
	workerKey  *rsa.PrivateKey
}

func (s *CertificateManager) LoadBootstrapToken(tokenString string, configPath string) error {
	tokenBytes, err := base64.StdEncoding.DecodeString(tokenString)
	if err != nil {
		return err
	}

	var token BootstrapToken

	if err := json.Unmarshal(tokenBytes, &token); err != nil {
		return err
	}

	if token.Name == "" ||
		len(token.CaDer) == 0 ||
		len(token.WorkerDer) == 0 ||
		len(token.WorkerKey) == 0 {
		return errors.New("invalid bootstrap token")
	}

	caCert, err := x509.ParseCertificate(token.CaDer)
	if err != nil {
		return err
	}

	workerCert, err := x509.ParseCertificate(token.WorkerDer)
	if err != nil {
		return err
	}

	workerKey, err := x509.ParsePKCS1PrivateKey(token.WorkerKey)
	if err != nil {
		return err
	}

	s.name = token.Name

	s.caDer = token.CaDer
	s.caCert = caCert

	s.workerDer = token.WorkerDer
	s.workerCert = workerCert
	s.workerKey = workerKey

	if err := os.MkdirAll(configPath, 0700); err != nil {
		return err
	}

	if err := savePEM(
		filepath.Join(configPath, "ca.crt"),
		"CERTIFICATE",
		s.caDer,
		0644,
	); err != nil {
		return err
	}

	if err := savePEM(
		filepath.Join(configPath, "worker.crt"),
		"CERTIFICATE",
		s.workerDer,
		0644,
	); err != nil {
		return err
	}

	if err := savePEM(
		filepath.Join(configPath, "worker.key"),
		"RSA PRIVATE KEY",
		s.workerKeyBytes(),
		0600,
	); err != nil {
		return err
	}

	return nil
}

func (s *CertificateManager) workerKeyBytes() []byte {
	return x509.MarshalPKCS1PrivateKey(s.workerKey)
}

func savePEM(filePath string, pemType string, derBytes []byte, permissions os.FileMode) error {
	file, err := os.OpenFile(
		filePath,
		os.O_WRONLY|os.O_CREATE|os.O_TRUNC,
		permissions,
	)
	if err != nil {
		return err
	}
	defer file.Close()

	return pem.Encode(file, &pem.Block{
		Type:  pemType,
		Bytes: derBytes,
	})
}
