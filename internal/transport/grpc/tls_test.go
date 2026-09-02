package grpc

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"log/slog"
	"math/big"
	"net"
	"testing"
	"time"

	"go.uber.org/mock/gomock"
	grpclib "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"

	lorev1 "lore/api/proto/lore/v1"
	"lore/internal/entities"
	mock_services "lore/internal/mocks/services"
	"lore/internal/services"
	"lore/internal/transport"
)

type certAuthority struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newCertAuthority(t *testing.T) certAuthority {
	t.Helper()

	key := newECDSAKey(t)
	template := certTemplate("lore-test-ca")
	template.KeyUsage = x509.KeyUsageCertSign
	template.BasicConstraintsValid = true
	template.IsCA = true

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create the CA certificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse the CA certificate: %v", err)
	}

	return certAuthority{cert: cert, key: key}
}

func (ca certAuthority) issue(t *testing.T, commonName string, usage x509.ExtKeyUsage) tls.Certificate {
	t.Helper()

	key := newECDSAKey(t)
	template := certTemplate(commonName)
	template.KeyUsage = x509.KeyUsageDigitalSignature
	template.ExtKeyUsage = []x509.ExtKeyUsage{usage}
	template.IPAddresses = []net.IP{net.IPv4(127, 0, 0, 1)}

	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("issue a certificate for %s: %v", commonName, err)
	}

	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func (ca certAuthority) pool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)

	return pool
}

func certTemplate(commonName string) *x509.Certificate {
	return &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
}

func newECDSAKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}

	return key
}

type mutualTLSFixture struct {
	addr      string
	ca        certAuthority
	query     *mock_services.MockQueryService
	synthesis *mock_services.MockSynthesisService
}

func newMutualTLSFixture(t *testing.T) mutualTLSFixture {
	t.Helper()

	ca := newCertAuthority(t)
	ctrl := gomock.NewController(t)
	query := mock_services.NewMockQueryService(ctrl)
	synthesis := mock_services.NewMockSynthesisService(ctrl)
	svc := transport.Services{
		Query:  query,
		Trace:  mock_services.NewMockTraceService(ctrl),
		Sync:   mock_services.NewMockSyncOrchestrator(ctrl),
		Status: mock_services.NewMockStatusService(ctrl),
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on loopback: %v", err)
	}

	serverTLS := &tls.Config{
		Certificates: []tls.Certificate{ca.issue(t, "lore-test-server", x509.ExtKeyUsageServerAuth)},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    ca.pool(),
		MinVersion:   tls.VersionTLS13,
	}

	ctx, stop := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() {
		served <- Serve(ctx, Config{
			Listener:  listener,
			Services:  svc,
			Synthesis: synthesis,
			Log:       slog.New(slog.DiscardHandler),
			TLS:       serverTLS,
		})
	}()
	t.Cleanup(func() {
		stop()
		if err := <-served; err != nil {
			t.Errorf("Serve() = %v, want nil", err)
		}
	})

	return mutualTLSFixture{addr: listener.Addr().String(), ca: ca, query: query, synthesis: synthesis}
}

func (f mutualTLSFixture) trustedClientTLS(t *testing.T) *tls.Config {
	t.Helper()

	return &tls.Config{
		Certificates: []tls.Certificate{f.ca.issue(t, "lore-test-client", x509.ExtKeyUsageClientAuth)},
		RootCAs:      f.ca.pool(),
		MinVersion:   tls.VersionTLS13,
	}
}

func (f mutualTLSFixture) findDecision(
	t *testing.T,
	clientTLS *tls.Config,
) (*lorev1.FindDecisionResponse, error) {
	t.Helper()

	conn, err := grpclib.NewClient(f.addr, grpclib.WithTransportCredentials(credentials.NewTLS(clientTLS)))
	if err != nil {
		t.Fatalf("client for %s: %v", f.addr, err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close the client: %v", err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()

	return lorev1.NewQueryServiceClient(conn).FindDecision(ctx, &lorev1.FindDecisionRequest{Question: rpcQuestion})
}

func (f mutualTLSFixture) assertServesATrustedClient(t *testing.T) {
	t.Helper()

	f.query.EXPECT().
		FindDecision(gomock.Any(), services.FindDecisionRequest{Question: rpcQuestion}).
		Return(&entities.EvidenceBundle{Question: rpcQuestion}, nil)
	f.synthesis.EXPECT().
		Synthesize(gomock.Any(), rpcQuestion, gomock.Any()).
		Return(rpcProse, nil)

	res, err := f.findDecision(t, f.trustedClientTLS(t))
	if err != nil {
		t.Fatalf("FindDecision() over a trusted client = %v, want a bundle", err)
	}

	assertSameProto(t, res.GetBundle(), &lorev1.EvidenceBundle{Question: rpcQuestion, Anchor: &lorev1.Anchor{}})
}

func TestMutualTLSServesAClientSignedByTheTrustedCA(t *testing.T) {
	newMutualTLSFixture(t).assertServesATrustedClient(t)
}

func TestMutualTLSRefuses(t *testing.T) {
	tests := []struct {
		name   string
		client func(*testing.T, mutualTLSFixture) *tls.Config
	}{
		{
			name: "a client offering no certificate",
			client: func(_ *testing.T, f mutualTLSFixture) *tls.Config {
				return &tls.Config{RootCAs: f.ca.pool(), MinVersion: tls.VersionTLS13}
			},
		},
		{
			name: "a client certificate signed by another CA",
			client: func(t *testing.T, f mutualTLSFixture) *tls.Config {
				intruder := newCertAuthority(t)
				return &tls.Config{
					Certificates: []tls.Certificate{intruder.issue(t, "intruder", x509.ExtKeyUsageClientAuth)},
					RootCAs:      f.ca.pool(),
					MinVersion:   tls.VersionTLS13,
				}
			},
		},
		{
			name: "a client that trusts another server CA",
			client: func(t *testing.T, f mutualTLSFixture) *tls.Config {
				client := f.trustedClientTLS(t)
				client.RootCAs = newCertAuthority(t).pool()
				return client
			},
		},
		{
			name: "a client that speaks only TLS 1.2",
			client: func(t *testing.T, f mutualTLSFixture) *tls.Config {
				client := f.trustedClientTLS(t)
				client.MinVersion, client.MaxVersion = tls.VersionTLS12, tls.VersionTLS12
				return client
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newMutualTLSFixture(t)
			f.assertServesATrustedClient(t)

			_, err := f.findDecision(t, test.client(t, f))

			if code := rpcStatus(t, err).Code(); code != codes.Unavailable {
				t.Errorf("FindDecision() code = %s, want %s (%v)", code, codes.Unavailable, err)
			}
		})
	}
}
