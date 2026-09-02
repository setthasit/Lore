package cli

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.uber.org/fx"

	"lore/internal/config"
	"lore/internal/entities"
)

type serveResult struct {
	exitCode int
	stderr   string
	modules  []fx.Option
}

func runResolving(t *testing.T, ctx context.Context, cfg *config.Config, args ...string) serveResult {
	t.Helper()

	var errOut bytes.Buffer
	res := serveResult{}

	resolve := func(_ context.Context, _ string, modules ...fx.Option) (*Runtime, func() error, error) {
		res.modules = modules
		return &Runtime{Config: cfg}, func() error { return nil }, nil
	}

	root := newRootCommand(resolve)
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&errOut)
	root.SetArgs(args)

	if err := root.ExecuteContext(ctx); err != nil {
		res.exitCode = report(&errOut, err)
	}
	res.stderr = errOut.String()
	return res
}

func TestServeRefusesANonLoopbackAddressWithoutTLS(t *testing.T) {
	res := runResolving(t, context.Background(), &config.Config{}, "serve", "--http", "0.0.0.0:8080")

	if res.exitCode != exitBadRequest {
		t.Fatalf("exit = %d, want %d (stderr %q)", res.exitCode, exitBadRequest, res.stderr)
	}
	for _, want := range []string{"0.0.0.0:8080", "not a loopback address", "server.mtls.cert", "server.mtls.key"} {
		if !strings.Contains(res.stderr, want) {
			t.Errorf("stderr = %q, want it to contain %q", res.stderr, want)
		}
	}
}

func TestServeRefusesAConfiguredNonLoopbackAddressWithoutTLS(t *testing.T) {
	cfg := &config.Config{Server: config.Server{HTTPAddr: ":8080"}}

	res := runResolving(t, context.Background(), cfg, "serve")

	if res.exitCode != exitBadRequest {
		t.Fatalf("exit = %d, want %d (stderr %q)", res.exitCode, exitBadRequest, res.stderr)
	}
	if !strings.Contains(res.stderr, "not a loopback address") {
		t.Errorf("stderr = %q, want it to name the problem", res.stderr)
	}
}

func TestServeWithoutAnyAddressSaysSo(t *testing.T) {
	res := runResolving(t, context.Background(), &config.Config{}, "serve")

	if res.exitCode != exitBadRequest {
		t.Fatalf("exit = %d, want %d (stderr %q)", res.exitCode, exitBadRequest, res.stderr)
	}
	for _, want := range []string{"server.http_addr", "--http"} {
		if !strings.Contains(res.stderr, want) {
			t.Errorf("stderr = %q, want it to contain %q", res.stderr, want)
		}
	}
}

func TestServeShutsDownWithItsContextAndReportsWhereItListens(t *testing.T) {
	tests := []struct {
		name   string
		mtls   func(*testing.T) *config.MTLS
		banner *regexp.Regexp
	}{
		{
			name:   "in the clear",
			banner: regexp.MustCompile(`^lore: serving MCP on http://127\.0\.0\.1:\d+/mcp\n$`),
		},
		{
			name:   "over TLS",
			mtls:   selfSignedTLS,
			banner: regexp.MustCompile(`^lore: serving MCP on https://127\.0\.0\.1:\d+/mcp\n$`),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := new(config.Config)
			if test.mtls != nil {
				cfg.Server.MTLS = test.mtls(t)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			res := runResolving(t, ctx, cfg, "serve", "--http", "127.0.0.1:0")

			if res.exitCode != exitOK {
				t.Fatalf("exit = %d, want %d (stderr %q)", res.exitCode, exitOK, res.stderr)
			}
			if !test.banner.MatchString(res.stderr) {
				t.Errorf("stderr = %q, want it to match %s", res.stderr, test.banner)
			}
		})
	}
}

func TestServeRefusesAHalfConfiguredTLSBlockOnLoopback(t *testing.T) {
	material := selfSignedTLS(t)

	tests := []struct {
		name string
		mtls *config.MTLS
	}{
		{name: "a certificate with no key", mtls: &config.MTLS{Cert: material.Cert}},
		{name: "a key with no certificate", mtls: &config.MTLS{Key: material.Key}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := &config.Config{Server: config.Server{MTLS: test.mtls}}

			res := runResolving(t, context.Background(), cfg, "serve", "--http", "127.0.0.1:0")

			if res.exitCode != exitBadRequest {
				t.Fatalf("exit = %d, want %d (stderr %q)", res.exitCode, exitBadRequest, res.stderr)
			}
			for _, want := range []string{"server.mtls", "both cert and key"} {
				if !strings.Contains(res.stderr, want) {
					t.Errorf("stderr = %q, want it to contain %q", res.stderr, want)
				}
			}
			if strings.Contains(res.stderr, "serving MCP") {
				t.Errorf("stderr = %q, want no listener at all", res.stderr)
			}
		})
	}
}

func TestServeTreatsAClientCAAloneAsNoHTTPTLS(t *testing.T) {
	cfg := &config.Config{Server: config.Server{MTLS: &config.MTLS{ClientCA: selfSignedTLS(t).Cert}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res := runResolving(t, ctx, cfg, "serve", "--http", "127.0.0.1:0")

	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, want %d (stderr %q)", res.exitCode, exitOK, res.stderr)
	}
	if !regexp.MustCompile(`^lore: serving MCP on http://127\.0\.0\.1:\d+/mcp\n$`).MatchString(res.stderr) {
		t.Errorf("stderr = %q, want a plaintext banner", res.stderr)
	}
}

func selfSignedTLS(t *testing.T) *config.MTLS {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "lore-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}

	dir := t.TempDir()
	material := &config.MTLS{Cert: filepath.Join(dir, "server.pem"), Key: filepath.Join(dir, "server-key.pem")}
	for path, block := range map[string]*pem.Block{
		material.Cert: {Type: "CERTIFICATE", Bytes: certDER},
		material.Key:  {Type: "PRIVATE KEY", Bytes: keyDER},
	} {
		if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	return material
}

func TestOnlyServeRunsTheScheduler(t *testing.T) {
	loopback := &config.Config{Server: config.Server{HTTPAddr: "127.0.0.1:0"}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res := runResolving(t, ctx, loopback, "serve")
	if len(res.modules) != 1 || !strings.Contains(fmt.Sprint(res.modules[0]), `fx.Module("scheduler"`) {
		t.Errorf("serve modules = %v, want just the scheduler", res.modules)
	}

	if res := run(t, mockStatus(t, entities.IndexStats{}, nil), "status"); len(res.modules) != 0 {
		t.Errorf("status modules = %v, want none", res.modules)
	}
}
