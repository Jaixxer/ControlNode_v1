package utils

import (
	"bytes"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// PEMParser reads a PEM encoded private key from disk and returns it as an RSA
// key. GitHub App private keys are PKCS#1 ("BEGIN RSA PRIVATE KEY"), but PKCS#8
// is accepted too.
func PEMParser(path string) (*rsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading private key %s: %w", path, err)
	}

	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found in %s", path)
	}

	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}

	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parsing private key %s: %w", path, err)
	}
	rsaKey, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private key %s is not an RSA key", path)
	}
	return rsaKey, nil
}

// GenerateAppJWT signs a short lived JWT that authenticates as the GitHub App
// itself. It is the first step of the server-to-server flow.
func GenerateAppJWT(appID string, privateKeyPath string) (string, error) {
	if appID == "" {
		return "", fmt.Errorf("github app id is required")
	}

	privateKey, err := PEMParser(privateKeyPath)
	if err != nil {
		slog.Error("failed to parse PEM file", slog.String("error", err.Error()))
		return "", err
	}

	claims := jwt.MapClaims{
		"iat": time.Now().Add(-60 * time.Second).Unix(),
		"exp": time.Now().Add(10 * time.Minute).Unix(),
		"iss": appID, // Use App ID, not installation ID
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	jwtString, err := token.SignedString(privateKey)
	if err != nil {
		slog.Error("failed to sign JWT", slog.String("error", err.Error()))
		return "", err
	}
	return jwtString, nil
}

// GenerateGithubAppJWT signs an app JWT and exchanges it for an installation
// access token for the given installation.
func GenerateGithubAppJWT(appID string, privateKeyPath string, installationID string) (string, error) {
	jwtString, err := GenerateAppJWT(appID, privateKeyPath)
	if err != nil {
		return "", err
	}

	installationToken, err := getInstallationAccessToken(jwtString, installationID)
	if err != nil {
		slog.Error("failed to get installation access token", slog.String("error", err.Error()))
		return "", err
	}
	return installationToken, nil
}

func getInstallationAccessToken(appJWT, installationID string) (string, error) {
	url := fmt.Sprintf("https://api.github.com/app/installations/%s/access_tokens", installationID)

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewBuffer([]byte("{}")))
	if err != nil {
		return "", err
	}

	req.Header.Set("Authorization", "Bearer "+appJWT)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("failed to get installation access token: %s", resp.Status)
	}

	var result struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	return result.Token, nil
}
