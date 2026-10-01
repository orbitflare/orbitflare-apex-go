package apex

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"io"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/quic-go/quic-go"
)

type packet struct {
	wire       []byte
	mevProtect bool
	maxRetries *uint16
}

type fakeApex struct {
	t         *testing.T
	port      int
	tlsConf   *tls.Config
	respond   func(packet) []byte
	mu        sync.Mutex
	udp       *net.UDPConn
	tr        *quic.Transport
	ln        *quic.EarlyListener
	conns     []*quic.Conn
	packets   []packet
	peerKeys  []solana.PublicKey
	accepted  int
	refuseAll bool
}

func newFakeApex(t *testing.T) *fakeApex {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"localhost"}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeApex{
		t: t,
		tlsConf: &tls.Config{
			Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: priv}},
			ClientAuth:   tls.RequireAnyClientCert,
			NextProtos:   []string{"solana-tpu"},
			MinVersion:   tls.VersionTLS13,
		},
		respond: func(p packet) []byte {
			return append([]byte{0}, bytes.Repeat([]byte{9}, 64)...)
		},
	}
	f.start(true)
	t.Cleanup(f.stop)
	return f
}

func (f *fakeApex) addr() string {
	return net.JoinHostPort("127.0.0.1", itoa(f.port))
}

func itoa(n int) string {
	return big.NewInt(int64(n)).String()
}

func (f *fakeApex) start(allow0RTT bool) {
	f.t.Helper()
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: f.port})
	if err != nil {
		f.t.Fatal(err)
	}
	f.port = udp.LocalAddr().(*net.UDPAddr).Port
	tr := &quic.Transport{Conn: udp}
	ln, err := tr.ListenEarly(f.tlsConf, &quic.Config{Allow0RTT: allow0RTT, MaxIdleTimeout: 10 * time.Second, MaxIncomingStreams: 1000, MaxIncomingUniStreams: 1000})
	if err != nil {
		f.t.Fatal(err)
	}
	f.mu.Lock()
	f.udp, f.tr, f.ln = udp, tr, ln
	f.mu.Unlock()
	go f.acceptLoop(ln)
}

func (f *fakeApex) stop() {
	f.mu.Lock()
	conns := f.conns
	f.conns = nil
	ln, tr, udp := f.ln, f.tr, f.udp
	f.mu.Unlock()
	for _, c := range conns {
		_ = c.CloseWithError(0, "server restart")
	}
	if ln != nil {
		_ = ln.Close()
		_ = tr.Close()
		_ = udp.Close()
	}
}

func (f *fakeApex) restart(allow0RTT bool) {
	f.stop()
	f.start(allow0RTT)
}

func (f *fakeApex) closeConnections(code quic.ApplicationErrorCode, reason string) {
	f.mu.Lock()
	conns := f.conns
	f.conns = nil
	f.mu.Unlock()
	for _, c := range conns {
		_ = c.CloseWithError(code, reason)
	}
}

func (f *fakeApex) acceptLoop(ln *quic.EarlyListener) {
	for {
		conn, err := ln.Accept(context.Background())
		if err != nil {
			return
		}
		f.mu.Lock()
		f.accepted++
		refuse := f.refuseAll
		if !refuse {
			f.conns = append(f.conns, conn)
		}
		f.mu.Unlock()
		if refuse {
			go func() {
				<-conn.HandshakeComplete()
				_ = conn.CloseWithError(2, "unauthorized")
			}()
			continue
		}
		go f.serve(conn)
	}
}

func (f *fakeApex) serve(conn *quic.Conn) {
	go func() {
		select {
		case <-conn.HandshakeComplete():
		case <-conn.Context().Done():
			return
		}
		certs := conn.ConnectionState().TLS.PeerCertificates
		if len(certs) == 1 {
			if pub, ok := certs[0].PublicKey.(ed25519.PublicKey); ok {
				f.mu.Lock()
				f.peerKeys = append(f.peerKeys, solana.PublicKeyFromBytes(pub))
				f.mu.Unlock()
			}
		}
	}()
	go func() {
		for {
			s, err := conn.AcceptUniStream(context.Background())
			if err != nil {
				return
			}
			go func() {
				data, err := io.ReadAll(s)
				if err != nil {
					return
				}
				if p, ok := parsePacket(data); ok {
					f.mu.Lock()
					f.packets = append(f.packets, p)
					f.mu.Unlock()
				}
			}()
		}
	}()
	for {
		s, err := conn.AcceptStream(context.Background())
		if err != nil {
			return
		}
		go func() {
			data, err := io.ReadAll(s)
			if err != nil {
				return
			}
			p, ok := parsePacket(data)
			if !ok {
				_, _ = s.Write([]byte{7, 0, 0})
				_ = s.Close()
				return
			}
			f.mu.Lock()
			f.packets = append(f.packets, p)
			respond := f.respond
			f.mu.Unlock()
			_, _ = s.Write(respond(p))
			_ = s.Close()
		}()
	}
}

func parsePacket(data []byte) (packet, bool) {
	if len(data) < 8 {
		return packet{}, false
	}
	n := int(binary.LittleEndian.Uint64(data[:8]))
	if len(data) < 8+n+2 {
		return packet{}, false
	}
	p := packet{wire: append([]byte(nil), data[8:8+n]...), mevProtect: data[8+n] == 1}
	if data[8+n+1] == 1 {
		if len(data) < 8+n+4 {
			return packet{}, false
		}
		r := binary.LittleEndian.Uint16(data[8+n+2:])
		p.maxRetries = &r
	}
	return p, true
}

func (f *fakeApex) snapshot() ([]packet, []solana.PublicKey, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]packet(nil), f.packets...), append([]solana.PublicKey(nil), f.peerKeys...), f.accepted
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func connectTo(t *testing.T, f *fakeApex, opts Options) *Client {
	t.Helper()
	opts.Endpoint = f.addr()
	c, err := ConnectWithOptions(context.Background(), opts, "test-api-key")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func signedTransfer(t *testing.T) *solana.Transaction {
	t.Helper()
	payer := solana.NewWallet().PrivateKey
	tip := solana.MustPublicKeyFromBase58("APeX2oLtjYehgTMUCA971L8htM7tGNqsXHDz5NrivhhX")
	tx, err := solana.NewTransaction([]solana.Instruction{TipInstruction(payer.PublicKey(), tip, MinTipLamports)}, solana.Hash{7}, solana.TransactionPayer(payer.PublicKey()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Sign(func(solana.PublicKey) *solana.PrivateKey { return &payer }); err != nil {
		t.Fatal(err)
	}
	return tx
}

func TestUniSendDeliversTheFramedTransactionAndAuthenticatesWithTheDerivedKey(t *testing.T) {
	f := newFakeApex(t)
	c := connectTo(t, f, Options{MEVProtect: true, MaxRetries: RetryBudget(3)})
	if c.Health() != Healthy || c.RemoteAddr().String() != f.addr() {
		t.Fatalf("health %v remote %v", c.Health(), c.RemoteAddr())
	}
	tx := signedTransfer(t)
	sig, err := c.SendTransaction(context.Background(), tx)
	if err != nil {
		t.Fatal(err)
	}
	if sig != tx.Signatures[0] {
		t.Fatal("returned the wrong signature")
	}
	wire, _ := SerializeTransaction(tx)
	eventually(t, "the packet", func() bool { p, _, _ := f.snapshot(); return len(p) == 1 })
	packets, keys, _ := f.snapshot()
	if !bytes.Equal(packets[0].wire, wire) || !packets[0].mevProtect || packets[0].maxRetries == nil || *packets[0].maxRetries != 3 {
		t.Fatalf("server got %+v", packets[0])
	}
	if len(keys) != 1 || keys[0] != ClientPubkey("test-api-key") {
		t.Fatalf("server saw client keys %v", keys)
	}
}

func TestBidiSendReturnsTheAdmission(t *testing.T) {
	f := newFakeApex(t)
	c := connectTo(t, f, Options{})
	tx := signedTransfer(t)
	f.mu.Lock()
	f.respond = func(p packet) []byte { return append([]byte{0}, tx.Signatures[0][:]...) }
	f.mu.Unlock()
	sig, err := c.SendTransactionWithResponse(context.Background(), tx)
	if err != nil || sig != tx.Signatures[0] {
		t.Fatalf("sig %v err %v", sig, err)
	}

	f.mu.Lock()
	f.respond = func(p packet) []byte { return []byte{4, 6, 0, 'n', 'o', ' ', 't', 'i', 'p'} }
	f.mu.Unlock()
	_, err = c.SendTransactionWithResponse(context.Background(), tx)
	var rejected *RejectedError
	if !errors.As(err, &rejected) || rejected.Code != AdmissionNoTip || rejected.Message != "no tip" {
		t.Fatalf("expected a NoTip rejection, got %v", err)
	}
	admission, err := c.SendWithResponse(context.Background(), []byte{1, 2, 3})
	if err != nil || admission.Accepted || admission.Code != AdmissionNoTip {
		t.Fatalf("admission %+v err %v", admission, err)
	}
}

func TestOversizedTransactionsAreRefusedLocally(t *testing.T) {
	f := newFakeApex(t)
	c := connectTo(t, f, Options{})
	var tooLarge *TooLargeError
	if err := c.SendTransactionBytes(context.Background(), make([]byte, MaxTransactionSize+1)); !errors.As(err, &tooLarge) || tooLarge.Size != MaxTransactionSize+1 {
		t.Fatalf("got %v", err)
	}
	if _, err := c.SendWithResponse(context.Background(), make([]byte, MaxTransactionSize+1)); !errors.As(err, &tooLarge) {
		t.Fatalf("got %v", err)
	}
	if err := c.SendTransactionBytes(context.Background(), make([]byte, MaxTransactionSize)); err != nil {
		t.Fatalf("a 4096-byte transaction must be accepted locally: %v", err)
	}
}

func TestWatchdogReconnectsAfterTheConnectionDrops(t *testing.T) {
	f := newFakeApex(t)
	c := connectTo(t, f, Options{})
	f.closeConnections(0, "maintenance")
	eventually(t, "a reconnect", func() bool { return c.ReconnectsTotal() >= 1 && c.Health() == Healthy })
	if err := c.SendTransactionBytes(context.Background(), []byte{1}); err != nil {
		t.Fatal(err)
	}
	_, _, accepted := f.snapshot()
	if accepted < 2 {
		t.Fatalf("server accepted %d connections", accepted)
	}
}

func TestAutoReconnectOnSendWithoutTheWatchdog(t *testing.T) {
	f := newFakeApex(t)
	c := connectTo(t, f, Options{DisableProactiveReconnect: true})
	f.closeConnections(0, "maintenance")
	eventually(t, "the client to see the close", func() bool { return c.Health() == Closed })
	if c.ReconnectsTotal() != 0 {
		t.Fatal("reconnected without the watchdog")
	}
	if err := c.SendTransactionBytes(context.Background(), []byte{4, 2}); err != nil {
		t.Fatal(err)
	}
	if c.ReconnectsTotal() != 1 || c.Health() != Healthy {
		t.Fatalf("reconnects %d health %v", c.ReconnectsTotal(), c.Health())
	}
	eventually(t, "the packet", func() bool { p, _, _ := f.snapshot(); return len(p) == 1 && bytes.Equal(p[0].wire, []byte{4, 2}) })
}

func TestARefusedConnectionIsReopenedOnTheNextSend(t *testing.T) {
	f := newFakeApex(t)
	c := connectTo(t, f, Options{DisableProactiveReconnect: true, DisableAutoReconnect: true})
	f.closeConnections(2, "unauthorized")
	eventually(t, "the client to see the close", func() bool { return c.Health() == Closed })
	if !c.refusedByEndpoint() {
		t.Fatal("an application close by the endpoint is a refusal")
	}
	if err := c.SendTransactionBytes(context.Background(), []byte{1}); err != nil {
		t.Fatalf("the next send reopens a closed connection: %v", err)
	}
	if c.ReconnectsTotal() != 1 {
		t.Fatalf("reconnects %d", c.ReconnectsTotal())
	}
	if err := c.Reconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.ReconnectsTotal() != 2 || c.Health() != Healthy {
		t.Fatalf("reconnects %d health %v", c.ReconnectsTotal(), c.Health())
	}
}

func TestZeroRTTResumptionSendsInTheFirstFlight(t *testing.T) {
	f := newFakeApex(t)
	c := connectTo(t, f, Options{DisableProactiveReconnect: true})
	if c.ZeroRTTResumptionsTotal() != 0 {
		t.Fatal("first connection counted as a resumption")
	}
	if _, err := c.SendWithResponse(context.Background(), []byte{1}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := c.Reconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.ZeroRTTResumptionsTotal() != 1 {
		t.Fatalf("resumptions %d", c.ZeroRTTResumptionsTotal())
	}
	if err := c.SendTransactionBytes(context.Background(), []byte{8, 8}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the 0-RTT packet", func() bool {
		p, _, _ := f.snapshot()
		return len(p) == 2 && bytes.Equal(p[1].wire, []byte{8, 8})
	})
}

func TestRejectedZeroRTTDataIsSentAgain(t *testing.T) {
	f := newFakeApex(t)
	c := connectTo(t, f, Options{DisableProactiveReconnect: true})
	if _, err := c.SendWithResponse(context.Background(), []byte{1}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	f.restart(false)
	eventually(t, "the client to see the restart", func() bool { return c.Health() == Closed })
	if err := c.SendTransactionBytes(context.Background(), []byte{5, 5, 5}); err != nil {
		t.Fatal(err)
	}
	if c.ZeroRTTResumptionsTotal() != 1 {
		t.Fatalf("expected a 0-RTT attempt, got %d", c.ZeroRTTResumptionsTotal())
	}
	eventually(t, "the resent packet", func() bool {
		p, _, _ := f.snapshot()
		n := 0
		for _, x := range p {
			if bytes.Equal(x.wire, []byte{5, 5, 5}) {
				n++
			}
		}
		return n == 1
	})
	if err := c.SendTransactionBytes(context.Background(), []byte{6}); err != nil {
		t.Fatalf("the connection after a 0-RTT rejection must be usable: %v", err)
	}
}

func TestCloseStopsTheClient(t *testing.T) {
	f := newFakeApex(t)
	c := connectTo(t, f, Options{})
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal("second close must be a no-op")
	}
	if err := c.SendTransactionBytes(context.Background(), []byte{1}); !errors.Is(err, ErrClientStopped) {
		t.Fatalf("got %v", err)
	}
	if c.Health() != Closed {
		t.Fatal("closed client reports healthy")
	}
}

func TestConnectErrors(t *testing.T) {
	ctx := context.Background()
	if _, err := ConnectWithOptions(ctx, Options{Endpoint: "no-port"}, "k"); !errors.Is(err, ErrResolve) {
		t.Fatalf("got %v", err)
	}
	if _, err := ConnectWithOptions(ctx, Options{Endpoint: "does-not-exist.invalid:7001"}, "k"); !errors.Is(err, ErrResolve) {
		t.Fatalf("got %v", err)
	}
	if _, err := ConnectWithOptions(ctx, Options{Endpoint: "127.0.0.1:7001", BindAddr: "999.0.0.1:0"}, "k"); !errors.Is(err, ErrBind) {
		t.Fatalf("got %v", err)
	}
	start := time.Now()
	_, err := ConnectWithOptions(ctx, Options{Endpoint: "127.0.0.1:9", ConnectTimeout: 300 * time.Millisecond}, "k")
	if err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("got %v after %v", err, time.Since(start))
	}
}

func TestConcurrentSendsShareOneConnection(t *testing.T) {
	f := newFakeApex(t)
	c := connectTo(t, f, Options{})
	var wg sync.WaitGroup
	errs := make(chan error, 50)
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- c.SendTransactionBytes(context.Background(), []byte{byte(i)})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "all packets", func() bool { p, _, _ := f.snapshot(); return len(p) == 50 })
	if _, _, accepted := f.snapshot(); accepted != 1 {
		t.Fatalf("server accepted %d connections", accepted)
	}
}

func TestTheWatchdogBacksOffWhileTheEndpointKeepsRefusing(t *testing.T) {
	f := newFakeApex(t)
	c := connectTo(t, f, Options{})
	f.mu.Lock()
	f.refuseAll = true
	f.mu.Unlock()
	f.closeConnections(2, "unauthorized")
	time.Sleep(2500 * time.Millisecond)
	_, _, accepted := f.snapshot()
	retries := accepted - 1
	if retries < 2 || retries > 5 {
		t.Fatalf("expected a doubling back-off (250, 500, 1000 ms), got %d retries in 2.5 s", retries)
	}
	f.mu.Lock()
	f.refuseAll = false
	f.mu.Unlock()
	eventually(t, "recovery once the endpoint accepts again", func() bool { return c.Health() == Healthy })
}
