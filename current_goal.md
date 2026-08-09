Here's the implementation order for Task 3, with topics to explore for each step.

1. cp/internal/pki/pki.go — certificate generation package
The core crypto logic. Write this as a standalone package with no dependencies on the daemon or CLI — test it with a throwaway main before wiring anything up.

Three functions to implement:

`GenerateCA` — generates an ECDSA P-256 keypair, creates a self-signed CA certificate (IsCA=true, BasicConstraintsValid=true, KeyUsage=CertSign|CRLSign, 10 year validity), writes PEM-encoded cert+key to the given paths, returns the parsed cert + private key for immediate use
`GenerateServerCert` — generates a server keypair, creates a certificate template with ExtKeyUsageServerAuth, signs it with the CA's private key, writes server cert+key to disk
`SignClientCert` — generates a client keypair, creates a certificate template with the worker name as CommonName, ExtKeyUsageClientAuth, 1 year validity, signs with CA, returns PEM bytes (doesn't write — the CLI handles that)
Topics to explore:

crypto/ecdsa and elliptic.P256() — ECDSA key generation
crypto/rand — secure randomness for keys and serial numbers
crypto/x509 — CreateCertificate, Certificate struct fields (IsCA, BasicConstraintsValid, KeyUsage, ExtKeyUsage, Subject, SerialNumber, NotBefore/NotAfter)
crypto/x509/pkix — Name struct for setting CommonName, Organization
math/big — SerialNumber is *big.Int, should be random
encoding/pem — wrapping DER bytes in PEM armor (BEGIN CERTIFICATE, BEGIN EC PRIVATE KEY)
crypto/x509.MarshalECPrivateKey and crypto/x509.ParseECPrivateKey
os.WriteFile with os.FileMode(0600) for private keys, 0644 for certs
time — certificate validity windows
2. Update config struct in cp/cmd/daemon/yml-parser.go
The current config has generic server.host/port and database.user/password. Replace that with the roadmap's config structure — a daemon section that includes TLS certificate paths and the paths where generated certs get written.

Topics to explore:

YAML struct tags in Go — yaml:"field_name" matching
Nested structs for daemon.tls.ca_cert etc.
Same gopkg.in/yaml.v3 package you already use
3. PKI bootstrap at daemon startup
In cp/cmd/daemon/main.go, before the HTTP listener starts, add logic that:

Checks if the CA cert exists at config.Daemon.TLSCACert
If missing — calls GenerateCA, then GenerateServerCert, logs "PKI bootstrapped"
If present — loads CA cert + key and server cert + key from disk
Stores them in package-level vars or a struct accessible to HTTP handlers
Topics to explore:

os.Stat + os.IsNotExist — checking if a file exists
os.ReadFile for reading PEM files
pem.Decode — parsing PEM blocks from file content
x509.ParseCertificate — converting DER bytes to *x509.Certificate
x509.ParseECPrivateKey — converting DER bytes to *ecdsa.PrivateKey
Package-level vars in Go for sharing state across handlers (fine for v1)
How slog.Info / slog.Error logging works
4. Daemon HTTP handler for /worker/init
Add a new route to the daemon's http.ServeMux. The handler:

Reads ?name= from URL query params
Validates it's non-empty
Calls pki.SignClientCert(caCert, caKey, name)
Returns JSON: {"cert_pem": "....", "key_pem": "...."}
Topics to explore:

http.Request.URL.Query().Get("name") — reading query params
http.Error — sending error responses with status codes
encoding/json — marshalling a map to JSON
w.Header().Set("Content-Type", "application/json")
5. ctl worker init --name <name> CLI subcommand
Add a new Cobra subcommand under ctl worker init. It:

Takes a --name flag (required)
Constructs a URL http://local/worker/init?name=<name>
Uses the same DaemonClient() Unix socket transport your existing run command uses
Reads the JSON response
Writes <name>.pem and <name>-key.pem to current directory with proper permissions
Prints success message with the filenames
Topics to explore:

Cobra subcommand nesting — workerCmd parent + workerInitCmd child, registered with rootCmd.AddCommand(workerCmd)
cmd.Flags().String("name", "", "...") and cmd.MarkFlagRequired("name")
Reusing the existing DaemonClient() and Unix socket dial helper from main.go
http.NewRequest with URL containing the query param
Reading response body, json.Unmarshal or json.Decoder
os.WriteFile with 0644 for cert PEM, 0600 for key PEM
fmt.Println for user-facing output
Order summary
1. cp/internal/pki/pki.go     ← pure crypto, testable standalone
2. Update yml-parser.go        ← config struct grows TLS paths
3. main.go PKI bootstrap       ← init on daemon start
4. Daemon HTTP handler         ← /worker/init route
5. CLI subcommand              ← ctl worker init --name worker1
Steps 1-2 are independent. Steps 4-5 both depend on 1 being done and 3 providing the in-memory CA key.

Start with step 1, write a main() in a temp file to test cert generation and verify with openssl, then move on to wiring it into the daemon.
