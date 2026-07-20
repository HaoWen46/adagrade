package email

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HaoWen46/adagrade/internal/domain"
)

// fakeSMTPServer is a minimal in-process listener that speaks enough SMTP to
// exercise net/smtp's client: EHLO, STARTTLS (for the plaintext-then-upgrade
// case), AUTH PLAIN, MAIL FROM/RCPT TO/DATA. It records the negotiated commands
// and the raw DATA payload for assertions, and needs no real network.
type fakeSMTPServer struct {
	ln     net.Listener
	tlsCfg *tls.Config

	mu       sync.Mutex
	commands []string
	dataMsg  string
	authSeen bool

	implicitTLS   bool // true ⇒ the listener itself is TLS (port-465 style)
	noSTARTTLS    bool // true ⇒ EHLO never advertises STARTTLS (downgrade-attack simulation)
	dropAfterData bool // true ⇒ close after the DATA terminator, before replying 250 (ambiguous acceptance)
	rcptReject    string
	dataReject    string
	stallRCPTFor  time.Duration
}

// newTLSConfig builds a self-signed cert for "127.0.0.1" (matches the fake
// server's bind address) so tests using p.testTLSConfig's InsecureSkipVerify
// (or, for the downgrade/mismatch tests, the real hostname-checked path) can
// exercise a real TLS handshake without touching real trust roots.
func newTLSConfig(t *testing.T) *tls.Config {
	t.Helper()
	return newTLSConfigForCN(t, "127.0.0.1")
}

// newTLSConfigForCN builds a self-signed cert for an arbitrary CN/SAN — used
// by the negative-cert-verification test to present a cert for a hostname
// that does NOT match what the client dials, so production tls.Config
// verification (ServerName set, no InsecureSkipVerify) must reject it.
func newTLSConfigForCN(t *testing.T, cn string) *tls.Config {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{cn},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	cert := tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  priv,
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}}
}

func startFakeSMTP(t *testing.T, implicitTLS bool) *fakeSMTPServer {
	t.Helper()
	return startFakeSMTPWithOpts(t, implicitTLS, false, newTLSConfig(t))
}

// startFakeSMTPWithOpts is the general constructor: noSTARTTLS suppresses the
// STARTTLS advertisement on EHLO (for the downgrade test), and tlsCfg lets
// callers supply a cert bound to a different CN (for the negative cert test).
func startFakeSMTPWithOpts(t *testing.T, implicitTLS, noSTARTTLS bool, tlsCfg *tls.Config) *fakeSMTPServer {
	t.Helper()

	var ln net.Listener
	var err error
	if implicitTLS {
		ln, err = tls.Listen("tcp", "127.0.0.1:0", tlsCfg)
	} else {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		t.Fatal(err)
	}

	s := &fakeSMTPServer{ln: ln, tlsCfg: tlsCfg, implicitTLS: implicitTLS, noSTARTTLS: noSTARTTLS}
	go s.serve()
	t.Cleanup(func() { ln.Close() })
	return s
}

func (s *fakeSMTPServer) addr() (host, port string) {
	host, port, _ = net.SplitHostPort(s.ln.Addr().String())
	return
}

func (s *fakeSMTPServer) record(cmd string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commands = append(s.commands, cmd)
}

func (s *fakeSMTPServer) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handleConn(conn)
	}
}

func (s *fakeSMTPServer) handleConn(conn net.Conn) {
	defer conn.Close()
	rw := bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))
	writeLine := func(line string) {
		rw.WriteString(line + "\r\n")
		rw.Flush()
	}

	writeLine("220 fake.smtp.test ESMTP")

	inData := false
	var dataBuf strings.Builder

	for {
		line, err := rw.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")

		if inData {
			if line == "." {
				inData = false
				s.mu.Lock()
				s.dataMsg = dataBuf.String()
				dropAfterData := s.dropAfterData
				dataReject := s.dataReject
				s.mu.Unlock()
				if dropAfterData {
					return
				}
				if dataReject != "" {
					writeLine("550 " + dataReject)
					continue
				}
				writeLine("250 OK")
				continue
			}
			dataBuf.WriteString(line + "\n")
			continue
		}

		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "EHLO"):
			s.record("EHLO")
			writeLine("250-fake.smtp.test greets you")
			if !s.implicitTLS && !s.noSTARTTLS {
				writeLine("250-STARTTLS")
			}
			writeLine("250-AUTH PLAIN LOGIN")
			writeLine("250 8BITMIME")
		case strings.HasPrefix(upper, "STARTTLS"):
			s.record("STARTTLS")
			writeLine("220 Ready to start TLS")
			tlsConn := tls.Server(conn, s.tlsCfg)
			if err := tlsConn.Handshake(); err != nil {
				return
			}
			conn = tlsConn
			rw = bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))
		case strings.HasPrefix(upper, "AUTH"):
			s.mu.Lock()
			s.authSeen = true
			s.mu.Unlock()
			s.record("AUTH")
			writeLine("235 Authentication successful")
		case strings.HasPrefix(upper, "MAIL FROM"):
			s.record("MAIL")
			writeLine("250 OK")
		case strings.HasPrefix(upper, "RCPT TO"):
			s.record("RCPT")
			s.mu.Lock()
			rcptReject := s.rcptReject
			stallFor := s.stallRCPTFor
			s.mu.Unlock()
			if stallFor > 0 {
				time.Sleep(stallFor)
			}
			if rcptReject != "" {
				writeLine("550 " + rcptReject)
				continue
			}
			writeLine("250 OK")
		case upper == "DATA":
			s.record("DATA")
			inData = true
			writeLine("354 Send message content")
		case upper == "QUIT":
			s.record("QUIT")
			writeLine("221 Bye")
			return
		default:
			writeLine("500 unrecognized command")
		}
	}
}

func TestSMTPProvider_DeliveryKeyProducesStableMessageID(t *testing.T) {
	srv := startFakeSMTP(t, false)
	host, port := srv.addr()
	p, err := NewSMTPProvider(Config{From: "grades@example.edu", SMTPHost: host, SMTPPort: port})
	if err != nil {
		t.Fatal(err)
	}
	p.testTLSConfig = &tls.Config{InsecureSkipVerify: true}
	msg := domain.OutboundEmail{
		DeliveryKey: "publish-item-101",
		To:          "s0000022@example.edu",
		Subject:     "Midterm results",
		TextBody:    "result",
	}

	id1, err := p.Send(context.Background(), msg)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := p.Send(context.Background(), msg)
	if err != nil {
		t.Fatal(err)
	}
	if id1 == "" || id1 != id2 {
		t.Fatalf("SMTP correlation ids = %q, %q; want stable non-empty id", id1, id2)
	}
	srv.mu.Lock()
	raw := srv.dataMsg
	srv.mu.Unlock()
	if !strings.Contains(raw, "Message-Id: <"+id1+"@adamarker.local>") {
		t.Fatalf("DATA lacks stable Message-Id for %q:\n%s", id1, raw)
	}
	if !strings.Contains(raw, "X-ADA-Marker-Delivery-Key: "+id1) {
		t.Fatalf("DATA lacks stable delivery correlation header:\n%s", raw)
	}
	if strings.Contains(raw, msg.DeliveryKey) {
		t.Fatalf("raw DeliveryKey must not be exposed on the wire:\n%s", raw)
	}
}

func TestSMTPProvider_ClassifiesPreAcceptanceFailureAsDefinitelyNotAccepted(t *testing.T) {
	tlsCfg := newTLSConfig(t)
	srv := startFakeSMTPWithOpts(t, false, true, tlsCfg)
	host, port := srv.addr()
	p, err := NewSMTPProvider(Config{From: "grades@example.edu", SMTPHost: host, SMTPPort: port})
	if err != nil {
		t.Fatal(err)
	}
	p.testTLSConfig = &tls.Config{InsecureSkipVerify: true}
	_, err = p.Send(context.Background(), domain.OutboundEmail{DeliveryKey: "smtp-definite", To: "s@example.edu", Subject: "x", TextBody: "y"})
	if !domain.IsEmailDefinitelyNotAccepted(err) {
		t.Fatalf("STARTTLS rejection outcome = %q, err=%v", domain.EmailDeliveryOutcomeOf(err), err)
	}
}

func TestSMTPProvider_ClassifiesLostDATAReplyAsOutcomeUnknown(t *testing.T) {
	srv := startFakeSMTP(t, false)
	srv.mu.Lock()
	srv.dropAfterData = true
	srv.mu.Unlock()
	host, port := srv.addr()
	p, err := NewSMTPProvider(Config{From: "grades@example.edu", SMTPHost: host, SMTPPort: port})
	if err != nil {
		t.Fatal(err)
	}
	p.testTLSConfig = &tls.Config{InsecureSkipVerify: true}
	_, err = p.Send(context.Background(), domain.OutboundEmail{DeliveryKey: "smtp-unknown", To: "s@example.edu", Subject: "x", TextBody: "y"})
	if !domain.IsEmailOutcomeUnknown(err) {
		t.Fatalf("lost DATA reply outcome = %q, err=%v", domain.EmailDeliveryOutcomeOf(err), err)
	}
}

func TestSMTPProvider_ErrorsDoNotExposeServerEchoedPII(t *testing.T) {
	const echoedPII = "student-private@example.edu Secret Exam Subject"

	t.Run("recipient rejection", func(t *testing.T) {
		srv := startFakeSMTP(t, false)
		srv.mu.Lock()
		srv.rcptReject = echoedPII
		srv.mu.Unlock()
		host, port := srv.addr()
		p, err := NewSMTPProvider(Config{From: "grades@example.edu", SMTPHost: host, SMTPPort: port})
		if err != nil {
			t.Fatal(err)
		}
		p.testTLSConfig = &tls.Config{InsecureSkipVerify: true}
		_, err = p.Send(context.Background(), domain.OutboundEmail{To: "student-private@example.edu", Subject: "Secret Exam Subject", TextBody: "private"})
		assertSanitizedSMTPError(t, err, echoedPII, "550")
		if !domain.IsEmailDefinitelyNotAccepted(err) {
			t.Fatalf("RCPT rejection outcome = %q", domain.EmailDeliveryOutcomeOf(err))
		}
	})

	t.Run("data completion rejection", func(t *testing.T) {
		srv := startFakeSMTP(t, false)
		srv.mu.Lock()
		srv.dataReject = echoedPII
		srv.mu.Unlock()
		host, port := srv.addr()
		p, err := NewSMTPProvider(Config{From: "grades@example.edu", SMTPHost: host, SMTPPort: port})
		if err != nil {
			t.Fatal(err)
		}
		p.testTLSConfig = &tls.Config{InsecureSkipVerify: true}
		_, err = p.Send(context.Background(), domain.OutboundEmail{To: "student-private@example.edu", Subject: "Secret Exam Subject", TextBody: "private"})
		assertSanitizedSMTPError(t, err, echoedPII, "550")
	})
}

func TestSMTPProvider_ContextCancellationInterruptsNetworkIO(t *testing.T) {
	const (
		contextWindow = 50 * time.Millisecond
		serverStall   = 800 * time.Millisecond
		maxReturn     = 300 * time.Millisecond
	)

	t.Run("established SMTP command", func(t *testing.T) {
		srv := startFakeSMTP(t, false)
		srv.mu.Lock()
		srv.stallRCPTFor = serverStall
		srv.mu.Unlock()
		host, port := srv.addr()
		p, err := NewSMTPProvider(Config{From: "grades@example.edu", SMTPHost: host, SMTPPort: port})
		if err != nil {
			t.Fatal(err)
		}
		p.testTLSConfig = &tls.Config{InsecureSkipVerify: true}
		ctx, cancel := context.WithTimeout(context.Background(), contextWindow)
		defer cancel()
		started := time.Now()
		_, err = p.Send(ctx, domain.OutboundEmail{To: "s@example.edu", Subject: "x", TextBody: "y"})
		if elapsed := time.Since(started); elapsed > maxReturn {
			t.Fatalf("Send returned after %v, want cancellation before %v server stall", elapsed, serverStall)
		}
		<-ctx.Done() // conn deadline and context timer can become observable a tick apart.
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) || !domain.IsEmailDefinitelyNotAccepted(err) {
			t.Fatalf("cancelled RCPT = ctx %v outcome %q err=%v", ctx.Err(), domain.EmailDeliveryOutcomeOf(err), err)
		}
	})

	t.Run("implicit TLS handshake", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		go func() {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
			time.Sleep(serverStall) // accept TCP but never complete TLS.
		}()
		host, _, _ := net.SplitHostPort(ln.Addr().String())
		p, err := NewSMTPProvider(Config{From: "grades@example.edu", SMTPHost: host, SMTPPort: implicitTLSPort})
		if err != nil {
			t.Fatal(err)
		}
		p.testDialAddr = ln.Addr().String()
		p.testTLSConfig = &tls.Config{InsecureSkipVerify: true}
		ctx, cancel := context.WithTimeout(context.Background(), contextWindow)
		defer cancel()
		started := time.Now()
		_, err = p.Send(ctx, domain.OutboundEmail{To: "s@example.edu", Subject: "x", TextBody: "y"})
		if elapsed := time.Since(started); elapsed > maxReturn {
			t.Fatalf("implicit TLS returned after %v, want cancellation before %v server stall", elapsed, serverStall)
		}
		<-ctx.Done()
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) || !domain.IsEmailDefinitelyNotAccepted(err) {
			t.Fatalf("cancelled TLS = ctx %v outcome %q err=%v", ctx.Err(), domain.EmailDeliveryOutcomeOf(err), err)
		}
	})
}

func assertSanitizedSMTPError(t *testing.T, err error, pii, code string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected SMTP error")
	}
	if strings.Contains(err.Error(), pii) || strings.Contains(err.Error(), "student-private") || strings.Contains(err.Error(), "Secret Exam") {
		t.Fatalf("SMTP error exposes server-controlled PII: %q", err)
	}
	if !strings.Contains(err.Error(), code) {
		t.Fatalf("sanitized SMTP error %q lacks reply code %s", err, code)
	}
}

func TestSMTPProvider_STARTTLS_Negotiation(t *testing.T) {
	srv := startFakeSMTP(t, false)
	host, port := srv.addr()

	p, err := NewSMTPProvider(Config{
		From:     "grades@example.edu",
		SMTPHost: host,
		SMTPPort: port,
		SMTPUser: "grader",
		SMTPPass: "hunter2",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Tests talk to a self-signed cert; the provider must allow injecting a
	// permissive tls.Config for exactly this reason (test-only knob).
	p.testTLSConfig = &tls.Config{InsecureSkipVerify: true}

	msg := domain.OutboundEmail{
		To:       "s0000008@example.edu",
		Subject:  "Midterm 2 — results",
		TextBody: "Total: 25/30\n",
	}
	id, err := p.Send(context.Background(), msg)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if id == "" {
		t.Error("providerID must be non-empty on success")
	}

	srv.mu.Lock()
	cmds := append([]string(nil), srv.commands...)
	dataMsg := srv.dataMsg
	authSeen := srv.authSeen
	srv.mu.Unlock()

	if !containsAll(cmds, []string{"EHLO", "STARTTLS", "MAIL", "RCPT", "DATA"}) {
		t.Errorf("SMTP negotiation missing expected commands: %v", cmds)
	}
	if !authSeen {
		t.Error("expected AUTH to be negotiated when SMTPUser/Pass are set")
	}
	if !strings.Contains(dataMsg, "Total: 25/30") {
		t.Errorf("captured DATA payload missing body text:\n%s", dataMsg)
	}
	if !strings.Contains(dataMsg, "Subject: Midterm 2") {
		t.Errorf("captured DATA payload missing subject:\n%s", dataMsg)
	}
}

func TestSMTPProvider_ImplicitTLS(t *testing.T) {
	srv := startFakeSMTP(t, true)
	host, port := srv.addr()

	// SMTPPort is the real well-known implicit-TLS port (465) so
	// implicitTLS()'s port-based decision is exercised for real; testDialAddr
	// redirects the actual dial to the fake server's ephemeral OS port (a
	// literal ":465" listener isn't bindable in a test process).
	p, err := NewSMTPProvider(Config{
		From:     "grades@example.edu",
		SMTPHost: host,
		SMTPPort: implicitTLSPort,
	})
	if err != nil {
		t.Fatal(err)
	}
	p.testTLSConfig = &tls.Config{InsecureSkipVerify: true}
	p.testDialAddr = net.JoinHostPort(host, port)

	msg := domain.OutboundEmail{To: "s0000009@example.edu", Subject: "x", TextBody: "y"}
	if _, err := p.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send over implicit TLS (port 465 style): %v", err)
	}

	srv.mu.Lock()
	cmds := append([]string(nil), srv.commands...)
	srv.mu.Unlock()
	if !containsAll(cmds, []string{"EHLO", "MAIL", "RCPT", "DATA"}) {
		t.Errorf("implicit-TLS negotiation missing expected commands: %v", cmds)
	}
	// No STARTTLS command is exchanged in implicit-TLS mode — the whole
	// connection is already encrypted at the transport level.
	for _, c := range cmds {
		if c == "STARTTLS" {
			t.Error("implicit TLS mode must not issue STARTTLS — it is already inside TLS")
		}
	}
}

func TestSMTPProvider_ParseInboundNotSupported(t *testing.T) {
	p, err := NewSMTPProvider(Config{From: "grades@example.edu", SMTPHost: "smtp.example.edu"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.ParseInbound([]byte(`{}`)); err == nil {
		t.Fatal("smtp provider has no inbound webhook — ParseInbound must error")
	}
}

func TestSMTPProvider_PortDeterminesTLSMode(t *testing.T) {
	p, err := NewSMTPProvider(Config{From: "a@example.edu", SMTPHost: "h", SMTPPort: "465"})
	if err != nil {
		t.Fatal(err)
	}
	if !p.implicitTLS() {
		t.Error("port 465 must select implicit TLS")
	}

	p2, err := NewSMTPProvider(Config{From: "a@example.edu", SMTPHost: "h", SMTPPort: "587"})
	if err != nil {
		t.Fatal(err)
	}
	if p2.implicitTLS() {
		t.Error("port 587 must select STARTTLS, not implicit TLS")
	}

	p3, err := NewSMTPProvider(Config{From: "a@example.edu", SMTPHost: "h"})
	if err != nil {
		t.Fatal(err)
	}
	if p3.implicitTLS() {
		t.Error("default port must select STARTTLS (587), not implicit TLS")
	}
}

// TestSMTPProvider_STARTTLSAbsent_RefusesPlaintext is Finding 1 (CRITICAL):
// on the non-implicit-TLS (587) path, a server that never advertises STARTTLS
// in its EHLO response must cause Send to fail immediately — never falling
// back to a plaintext AUTH/DATA exchange. The transcript must stop at EHLO.
func TestSMTPProvider_STARTTLSAbsent_RefusesPlaintext(t *testing.T) {
	tlsCfg := newTLSConfig(t)
	srv := startFakeSMTPWithOpts(t, false, true, tlsCfg)
	host, port := srv.addr()

	p, err := NewSMTPProvider(Config{
		From:     "grades@example.edu",
		SMTPHost: host,
		SMTPPort: port,
		SMTPUser: "grader",
		SMTPPass: "hunter2",
	})
	if err != nil {
		t.Fatal(err)
	}
	p.testTLSConfig = &tls.Config{InsecureSkipVerify: true}

	msg := domain.OutboundEmail{
		To:       "s0000010@example.edu",
		Subject:  "Midterm 2 — results",
		TextBody: "Total: 25/30\n",
	}
	if _, err := p.Send(context.Background(), msg); err == nil {
		t.Fatal("Send must fail when the server does not advertise STARTTLS — plaintext SMTP must never be used")
	}

	srv.mu.Lock()
	cmds := append([]string(nil), srv.commands...)
	authSeen := srv.authSeen
	dataMsg := srv.dataMsg
	srv.mu.Unlock()

	if len(cmds) != 1 || cmds[0] != "EHLO" {
		t.Errorf("transcript must stop at EHLO when STARTTLS is unavailable, got: %v", cmds)
	}
	if authSeen {
		t.Error("AUTH must never be sent when STARTTLS was not negotiated")
	}
	if dataMsg != "" {
		t.Error("DATA must never be sent when STARTTLS was not negotiated")
	}
}

// TestSMTPProvider_ProductionTLS_RejectsWrongHostnameCert is Finding 2
// (IMPORTANT): the existing STARTTLS/implicit-TLS tests both inject
// InsecureSkipVerify into the test dial config, so production certificate
// verification (tlsConfig()'s real path: ServerName set, no skip-verify) is
// never exercised. This test leaves testTLSConfig unset so Send uses the real
// &tls.Config{ServerName: p.host} construction, and points the client at a
// server presenting a certificate for an unrelated hostname — the handshake
// must fail.
func TestSMTPProvider_ProductionTLS_RejectsWrongHostnameCert(t *testing.T) {
	wrongCertCfg := newTLSConfigForCN(t, "totally-different-host.invalid")
	srv := startFakeSMTPWithOpts(t, false, false, wrongCertCfg)
	host, port := srv.addr()

	p, err := NewSMTPProvider(Config{
		From:     "grades@example.edu",
		SMTPHost: host,
		SMTPPort: port,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately NOT setting p.testTLSConfig: Send must build its real
	// production tls.Config{ServerName: p.host}, which the fake server's
	// mismatched-hostname cert cannot satisfy.

	msg := domain.OutboundEmail{To: "s0000011@example.edu", Subject: "x", TextBody: "y"}
	if _, err := p.Send(context.Background(), msg); err == nil {
		t.Fatal("Send must fail: server cert is for a different hostname and production verification must reject it")
	}

	srv.mu.Lock()
	authSeen := srv.authSeen
	dataMsg := srv.dataMsg
	srv.mu.Unlock()
	if authSeen || dataMsg != "" {
		t.Error("no AUTH/DATA should occur when the TLS handshake itself failed")
	}
}

// TestBuildRFC5322_RejectsCRLFInjection is Finding 3 (IMPORTANT): a \r or \n
// embedded in any header-bound field (Subject here — sourced from
// attacker-influenced data like AssessmentName) must not reach the raw
// message unsanitized, since that would let the attacker inject arbitrary
// extra headers or split the header block from the body. buildRFC5322 must
// reject (error), not silently strip, any such field.
func TestBuildRFC5322_RejectsCRLFInjection(t *testing.T) {
	cases := []struct {
		name string
		msg  domain.OutboundEmail
	}{
		{
			name: "subject with embedded CRLF",
			msg: domain.OutboundEmail{
				To:       "s0000012@example.edu",
				Subject:  "Midterm 2\r\nX-Injected: evil",
				TextBody: "Total: 25/30\n",
			},
		},
		{
			name: "to with embedded CRLF",
			msg: domain.OutboundEmail{
				To:       "s0000012@example.edu\r\nBcc: attacker@evil.example",
				Subject:  "Midterm 2",
				TextBody: "Total: 25/30\n",
			},
		},
		{
			name: "reply-to with embedded CRLF",
			msg: domain.OutboundEmail{
				To:       "s0000012@example.edu",
				ReplyTo:  "regrade+abc@example.edu\r\nBcc: attacker@evil.example",
				Subject:  "Midterm 2",
				TextBody: "Total: 25/30\n",
			},
		},
		{
			name: "subject with bare LF",
			msg: domain.OutboundEmail{
				To:       "s0000012@example.edu",
				Subject:  "Midterm 2\nX-Injected: evil",
				TextBody: "Total: 25/30\n",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := buildRFC5322("grades@example.edu", tc.msg, "deadbeef"); err == nil {
				t.Fatal("buildRFC5322 must reject a header-bound field containing CR or LF, not silently emit it")
			}
		})
	}
}

// TestSMTPProvider_Send_RejectsCRLFInjection exercises the same attack through
// the full Send path against the in-process fake server: a poisoned Subject
// must cause Send to fail and no message may reach the wire.
func TestSMTPProvider_Send_RejectsCRLFInjection(t *testing.T) {
	srv := startFakeSMTP(t, false)
	host, port := srv.addr()

	p, err := NewSMTPProvider(Config{
		From:     "grades@example.edu",
		SMTPHost: host,
		SMTPPort: port,
	})
	if err != nil {
		t.Fatal(err)
	}
	p.testTLSConfig = &tls.Config{InsecureSkipVerify: true}

	msg := domain.OutboundEmail{
		To:       "s0000013@example.edu",
		Subject:  "Midterm 2\r\nBcc: attacker@evil.example",
		TextBody: "Total: 25/30\n",
	}
	if _, err := p.Send(context.Background(), msg); err == nil {
		t.Fatal("Send must fail for a CRLF-poisoned Subject — no message may be emitted")
	}

	srv.mu.Lock()
	dataMsg := srv.dataMsg
	srv.mu.Unlock()
	if dataMsg != "" {
		t.Error("no DATA payload should have been transmitted for a rejected message")
	}
}

// ---- Attachments (report-attachments spec §3): multipart/mixed wraps the
// existing multipart/alternative when Attachments is non-empty. ----

// TestBuildRFC5322_NoAttachments_StaysMultipartAlternative pins the
// no-attachment path unchanged: when msg.Attachments is empty, the top-level
// Content-Type must still be multipart/alternative, not multipart/mixed —
// existing recipients/deliverability behavior for the common case (D44
// "none" quality) must not change shape.
func TestBuildRFC5322_NoAttachments_StaysMultipartAlternative(t *testing.T) {
	raw, err := buildRFC5322("grades@example.edu", domain.OutboundEmail{
		To:       "s0000020@example.edu",
		Subject:  "Midterm 2",
		TextBody: "Total: 25/30",
	}, "deadbeef")
	if err != nil {
		t.Fatal(err)
	}
	msg := string(raw)
	if !strings.Contains(msg, "Content-Type: multipart/alternative") {
		t.Errorf("no-attachment message should stay multipart/alternative at top level:\n%s", msg)
	}
	if strings.Contains(msg, "multipart/mixed") {
		t.Errorf("no-attachment message must not mention multipart/mixed:\n%s", msg)
	}
}

// TestBuildRFC5322_WithAttachment_WrapsInMultipartMixed asserts a PDF
// attachment produces multipart/mixed at the top level, with the
// multipart/alternative (text+html) body nested as the first part and the
// attachment as a second part carrying Content-Disposition: attachment,
// base64-encoded content, and the given filename/MIME type.
func TestBuildRFC5322_WithAttachment_WrapsInMultipartMixed(t *testing.T) {
	pdfBytes := []byte("%PDF-1.4 fake pdf bytes for a test\n")
	raw, err := buildRFC5322("grades@example.edu", domain.OutboundEmail{
		To:       "s0000021@example.edu",
		Subject:  "Midterm 2",
		TextBody: "Total: 25/30",
		HTMLBody: "<p>Total: 25/30</p>",
		Attachments: []domain.Attachment{
			{Filename: "results.pdf", MIME: "application/pdf", Content: pdfBytes},
		},
	}, "deadbeef")
	if err != nil {
		t.Fatal(err)
	}
	msg := string(raw)

	if !strings.Contains(msg, "Content-Type: multipart/mixed") {
		t.Fatalf("attachment message must be multipart/mixed at top level:\n%s", msg)
	}
	if !strings.Contains(msg, "Content-Type: multipart/alternative") {
		t.Errorf("attachment message must still nest multipart/alternative for the text/html body:\n%s", msg)
	}
	if !strings.Contains(msg, "Total: 25/30") {
		t.Errorf("nested text body missing:\n%s", msg)
	}
	if !strings.Contains(msg, "application/pdf") {
		t.Errorf("attachment MIME type missing:\n%s", msg)
	}
	if !strings.Contains(msg, `filename="results.pdf"`) {
		t.Errorf("attachment filename missing:\n%s", msg)
	}
	if !strings.Contains(msg, "Content-Disposition: attachment") {
		t.Errorf("attachment must carry Content-Disposition: attachment:\n%s", msg)
	}
	if !strings.Contains(msg, "Content-Transfer-Encoding: base64") {
		t.Errorf("attachment body must be base64-encoded:\n%s", msg)
	}
	wantB64 := base64.StdEncoding.EncodeToString(pdfBytes)
	// base64.Encoding wraps long lines (76 chars) per RFC 2045 in the encoder
	// this package should use; strip all whitespace from both sides before
	// comparing so line-wrapping differences don't fail the assertion.
	gotStripped := stripWhitespace(msg)
	wantStripped := stripWhitespace(wantB64)
	if !strings.Contains(gotStripped, wantStripped) {
		t.Errorf("attachment body does not contain the expected base64 payload")
	}
}

// TestBuildRFC5322_MultipleAttachments asserts every attachment in the slice
// gets its own MIME part.
func TestBuildRFC5322_MultipleAttachments(t *testing.T) {
	raw, err := buildRFC5322("grades@example.edu", domain.OutboundEmail{
		To:       "s0000022@example.edu",
		Subject:  "Midterm 2",
		TextBody: "Total: 25/30",
		Attachments: []domain.Attachment{
			{Filename: "results.pdf", MIME: "application/pdf", Content: []byte("pdf-bytes")},
			{Filename: "results.zip", MIME: "application/zip", Content: []byte("zip-bytes")},
		},
	}, "deadbeef")
	if err != nil {
		t.Fatal(err)
	}
	msg := string(raw)
	if !strings.Contains(msg, `filename="results.pdf"`) || !strings.Contains(msg, `filename="results.zip"`) {
		t.Errorf("both attachment filenames should appear:\n%s", msg)
	}
}

// TestBuildRFC5322_RejectsCRLFInAttachmentFilename extends the header
// injection guard (already covering Subject/To/Reply-To) to attachment
// filenames, which land inside a Content-Disposition header value — the
// same injection surface, since a filename can in principle come from data
// that traces back to course-controlled strings (assessment name choices in
// filename templates).
func TestBuildRFC5322_RejectsCRLFInAttachmentFilename(t *testing.T) {
	_, err := buildRFC5322("grades@example.edu", domain.OutboundEmail{
		To:       "s0000023@example.edu",
		Subject:  "Midterm 2",
		TextBody: "Total: 25/30",
		Attachments: []domain.Attachment{
			{Filename: "results.pdf\r\nX-Injected: evil", MIME: "application/pdf", Content: []byte("x")},
		},
	}, "deadbeef")
	if err == nil {
		t.Fatal("buildRFC5322 must reject a CRLF-poisoned attachment filename")
	}
}

// TestBuildRFC5322_RejectsCRLFInAttachmentMIME is A11: MIME is interpolated
// into a raw "Content-Type: %s; name=%q" header line the same as Filename, so
// it must be guarded the same way.
func TestBuildRFC5322_RejectsCRLFInAttachmentMIME(t *testing.T) {
	_, err := buildRFC5322("grades@example.edu", domain.OutboundEmail{
		To:       "s0000023@example.edu",
		Subject:  "Midterm 2",
		TextBody: "Total: 25/30",
		Attachments: []domain.Attachment{
			{Filename: "results.pdf", MIME: "application/pdf\r\nX-Injected: evil", Content: []byte("x")},
		},
	}, "deadbeef")
	if err == nil {
		t.Fatal("buildRFC5322 must reject a CRLF-poisoned attachment MIME type")
	}
}

func stripWhitespace(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\r' || r == '\n' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func containsAll(haystack []string, want []string) bool {
	set := map[string]bool{}
	for _, h := range haystack {
		set[h] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}
