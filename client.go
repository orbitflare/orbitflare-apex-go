package apex

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/quic-go/quic-go"
)

const (
	watchdogTick       = 250 * time.Millisecond
	watchdogMaxBackoff = 30 * time.Second
	resolveTimeout     = 2 * time.Second
	maxIdleTimeout     = 10 * time.Second
	ipServerName       = "apex-sender"
)

var (
	errNilTransaction = errors.New("nil transaction")
	errFrameTooLong   = errors.New("admission frame longer than allowed")
)

type ConnectionHealth int

const (
	Healthy ConnectionHealth = iota
	Closed
)

func (h ConnectionHealth) String() string {
	if h == Healthy {
		return "healthy"
	}
	return "closed"
}

type Options struct {
	Endpoint                  string
	MEVProtect                bool
	MaxRetries                *uint16
	BindAddr                  string
	ConnectTimeout            time.Duration
	SendTimeout               time.Duration
	KeepAlive                 time.Duration
	DisableAutoReconnect      bool
	DisableProactiveReconnect bool
}

func DefaultOptions() Options {
	return Options{
		Endpoint:       Frankfurt.QUICEndpoint(),
		ConnectTimeout: 3 * time.Second,
		SendTimeout:    2 * time.Second,
		KeepAlive:      time.Second,
	}
}

func (o Options) withDefaults() Options {
	d := DefaultOptions()
	if o.Endpoint == "" {
		o.Endpoint = d.Endpoint
	}
	if o.ConnectTimeout <= 0 {
		o.ConnectTimeout = d.ConnectTimeout
	}
	if o.SendTimeout <= 0 {
		o.SendTimeout = d.SendTimeout
	}
	if o.KeepAlive <= 0 {
		o.KeepAlive = d.KeepAlive
	}
	return o
}

type Client struct {
	opts      Options
	udp       *net.UDPConn
	transport *quic.Transport
	tlsConf   *tls.Config
	quicConf  *quic.Config
	lookup    string

	remoteMu sync.Mutex
	remote   netip.AddrPort

	mu   sync.Mutex
	conn *quic.Conn

	earlyMu   sync.Mutex
	earlyConn *quic.Conn

	reconnects         atomic.Uint64
	zeroRTTResumptions atomic.Uint64

	closed    atomic.Bool
	stopWatch context.CancelFunc
	watchDone chan struct{}
}

func Connect(ctx context.Context, region Region, apiKey string) (*Client, error) {
	return ConnectWithOptions(ctx, Options{Endpoint: region.QUICEndpoint()}, apiKey)
}

func ConnectWithOptions(ctx context.Context, opts Options, apiKey string) (*Client, error) {
	opts = opts.withDefaults()
	host, portStr, err := net.SplitHostPort(opts.Endpoint)
	if err != nil {
		return nil, wrap(ErrResolve, err)
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return nil, wrap(ErrResolve, fmt.Errorf("%s: bad port", opts.Endpoint))
	}

	serverName, lookup := host, opts.Endpoint
	var remote netip.AddrPort
	if ip, perr := netip.ParseAddr(host); perr == nil {
		serverName, lookup = ipServerName, ""
		remote = netip.AddrPortFrom(ip, uint16(port))
	} else {
		ips, lerr := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if lerr != nil {
			return nil, wrap(ErrResolve, lerr)
		}
		if len(ips) == 0 {
			return nil, wrap(ErrResolve, fmt.Errorf("%s: no address", opts.Endpoint))
		}
		remote = netip.AddrPortFrom(ips[0].Unmap(), uint16(port))
	}

	bind := opts.BindAddr
	if bind == "" {
		bind = "0.0.0.0:0"
		if !remote.Addr().Is4() {
			bind = "[::]:0"
		}
	}
	laddr, err := net.ResolveUDPAddr("udp", bind)
	if err != nil {
		return nil, wrap(ErrBind, err)
	}
	udp, err := net.ListenUDP("udp", laddr)
	if err != nil {
		return nil, wrap(ErrBind, err)
	}

	c := &Client{
		opts:      opts,
		udp:       udp,
		transport: &quic.Transport{Conn: udp},
		tlsConf:   clientTLSConfig(deriveClientKey(apiKey), serverName, tls.NewLRUClientSessionCache(16)),
		quicConf: &quic.Config{
			MaxIdleTimeout:       maxIdleTimeout,
			KeepAlivePeriod:      opts.KeepAlive,
			HandshakeIdleTimeout: opts.ConnectTimeout,
		},
		lookup: lookup,
		remote: remote,
	}
	if _, err := c.getOrConnect(ctx); err != nil {
		c.shutdown()
		return nil, err
	}
	if !opts.DisableProactiveReconnect {
		wctx, cancel := context.WithCancel(context.Background())
		c.stopWatch = cancel
		c.watchDone = make(chan struct{})
		go c.watchdog(wctx)
	}
	return c, nil
}

func (c *Client) watchdog(ctx context.Context) {
	defer close(c.watchDone)
	wait := watchdogTick
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if c.Health() == Healthy {
			wait = watchdogTick
		} else {
			if c.refusedByEndpoint() {
				wait = min(wait*2, watchdogMaxBackoff)
			} else {
				wait = watchdogTick
			}
			_, _ = c.getOrConnect(ctx)
		}
		timer.Reset(wait)
	}
}

func connClosed(conn *quic.Conn) bool {
	select {
	case <-conn.Context().Done():
		return true
	default:
		return false
	}
}

func (c *Client) getOrConnect(ctx context.Context) (*quic.Conn, error) {
	if c.closed.Load() {
		return nil, ErrClientStopped
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil && !connClosed(c.conn) {
		return c.conn, nil
	}
	if c.conn != nil {
		c.reconnects.Add(1)
		c.refreshRemote(ctx)
	}
	dctx, cancel := context.WithTimeout(ctx, c.opts.ConnectTimeout)
	defer cancel()
	conn, err := c.transport.DialEarly(dctx, net.UDPAddrFromAddrPort(c.RemoteAddr()), c.tlsConf, c.quicConf)
	if err != nil {
		if dctx.Err() != nil && ctx.Err() == nil {
			return nil, ErrTimeout
		}
		return nil, wrap(ErrConnect, err)
	}
	select {
	case <-conn.HandshakeComplete():
	default:
		c.zeroRTTResumptions.Add(1)
		c.earlyMu.Lock()
		c.earlyConn = conn
		c.earlyMu.Unlock()
	}
	c.conn = conn
	return conn, nil
}

func (c *Client) earlyDataAccepted(ctx context.Context) (*quic.Conn, bool) {
	c.earlyMu.Lock()
	pending := c.earlyConn
	c.earlyConn = nil
	c.earlyMu.Unlock()
	if pending == nil {
		return nil, true
	}
	select {
	case <-pending.HandshakeComplete():
	case <-pending.Context().Done():
		return pending, false
	case <-ctx.Done():
		return pending, false
	}
	if pending.ConnectionState().Used0RTT {
		return nil, true
	}
	return c.adoptNext(ctx, pending), false
}

func (c *Client) adoptNext(ctx context.Context, rejected *quic.Conn) *quic.Conn {
	next, err := rejected.NextConnection(ctx)
	if err != nil {
		return rejected
	}
	c.mu.Lock()
	if c.conn == rejected {
		c.conn = next
	}
	c.mu.Unlock()
	return next
}

func (c *Client) refusedByEndpoint() bool {
	if !c.mu.TryLock() {
		return false
	}
	defer c.mu.Unlock()
	if c.conn == nil || !connClosed(c.conn) {
		return false
	}
	return closedByEndpoint(context.Cause(c.conn.Context()))
}

func closedByEndpoint(cause error) bool {
	var appErr *quic.ApplicationError
	if errors.As(cause, &appErr) {
		return appErr.Remote
	}
	var trErr *quic.TransportError
	return errors.As(cause, &trErr) && trErr.Remote && trErr.ErrorCode == quic.ApplicationErrorErrorCode
}

func (c *Client) Health() ConnectionHealth {
	if !c.mu.TryLock() {
		return Closed
	}
	defer c.mu.Unlock()
	if c.conn != nil && !connClosed(c.conn) {
		return Healthy
	}
	return Closed
}

func (c *Client) ReconnectsTotal() uint64 {
	return c.reconnects.Load()
}

func (c *Client) ZeroRTTResumptionsTotal() uint64 {
	return c.zeroRTTResumptions.Load()
}

func (c *Client) RemoteAddr() netip.AddrPort {
	c.remoteMu.Lock()
	defer c.remoteMu.Unlock()
	return c.remote
}

func (c *Client) Options() Options {
	return c.opts
}

func sameFamily(found []netip.AddrPort, current netip.AddrPort) (netip.AddrPort, bool) {
	want4 := current.Addr().Unmap().Is4()
	for _, a := range found {
		if a.Addr().Unmap().Is4() == want4 {
			return a, true
		}
	}
	return netip.AddrPort{}, false
}

func (c *Client) refreshRemote(ctx context.Context) {
	if c.lookup == "" {
		return
	}
	host, _, err := net.SplitHostPort(c.lookup)
	if err != nil {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	ips, err := net.DefaultResolver.LookupNetIP(rctx, "ip", host)
	if err != nil {
		return
	}
	current := c.RemoteAddr()
	found := make([]netip.AddrPort, 0, len(ips))
	for _, ip := range ips {
		found = append(found, netip.AddrPortFrom(ip.Unmap(), current.Port()))
	}
	if next, ok := sameFamily(found, current); ok && next != current {
		c.remoteMu.Lock()
		c.remote = next
		c.remoteMu.Unlock()
	}
}

func (c *Client) Reconnect(ctx context.Context) error {
	c.mu.Lock()
	if c.conn != nil {
		old := c.conn
		c.conn = nil
		c.reconnects.Add(1)
		_ = old.CloseWithError(0, "reconnect")
	}
	c.mu.Unlock()
	_, err := c.getOrConnect(ctx)
	return err
}

func (c *Client) SendTransaction(ctx context.Context, tx *solana.Transaction) (solana.Signature, error) {
	if tx == nil || len(tx.Signatures) == 0 {
		return solana.Signature{}, ErrNoSignature
	}
	signature := tx.Signatures[0]
	wire, err := SerializeTransaction(tx)
	if err != nil {
		return solana.Signature{}, err
	}
	if err := c.SendTransactionBytes(ctx, wire); err != nil {
		return solana.Signature{}, err
	}
	return signature, nil
}

func (c *Client) SendTransactionBytes(ctx context.Context, wire []byte) error {
	if len(wire) > MaxTransactionSize {
		return &TooLargeError{Size: len(wire)}
	}
	header, trailer := FrameParts(len(wire), c.opts.MEVProtect, c.opts.MaxRetries)
	err := c.writeUni(ctx, header[:], wire, trailer)
	if err == nil || c.opts.DisableAutoReconnect || errors.Is(err, ErrClientStopped) || ctx.Err() != nil {
		return err
	}
	if err := c.Reconnect(ctx); err != nil {
		return err
	}
	return c.writeUni(ctx, header[:], wire, trailer)
}

func (c *Client) SendTransactionWithResponse(ctx context.Context, tx *solana.Transaction) (solana.Signature, error) {
	wire, err := SerializeTransaction(tx)
	if err != nil {
		return solana.Signature{}, err
	}
	admission, err := c.SendWithResponse(ctx, wire)
	if err != nil {
		return solana.Signature{}, err
	}
	if !admission.Accepted {
		return solana.Signature{}, &RejectedError{Code: admission.Code, Message: admission.Message}
	}
	return admission.Signature, nil
}

func (c *Client) SendWithResponse(ctx context.Context, wire []byte) (Admission, error) {
	if len(wire) > MaxTransactionSize {
		return Admission{}, &TooLargeError{Size: len(wire)}
	}
	header, trailer := FrameParts(len(wire), c.opts.MEVProtect, c.opts.MaxRetries)
	admission, err := c.writeBi(ctx, header[:], wire, trailer)
	if err == nil || c.opts.DisableAutoReconnect || errors.Is(err, ErrClientStopped) || ctx.Err() != nil {
		return admission, err
	}
	if err := c.Reconnect(ctx); err != nil {
		return Admission{}, err
	}
	return c.writeBi(ctx, header[:], wire, trailer)
}

func (c *Client) writeUni(ctx context.Context, header, wire, trailer []byte) error {
	conn, err := c.getOrConnect(ctx)
	if err != nil {
		return err
	}
	err = c.writeUniOn(ctx, conn, header, wire, trailer)
	if errors.Is(err, quic.Err0RTTRejected) {
		c.earlyMu.Lock()
		c.earlyConn = nil
		c.earlyMu.Unlock()
		return c.writeUniOn(ctx, c.adoptNext(ctx, conn), header, wire, trailer)
	}
	if err != nil {
		return err
	}
	if next, accepted := c.earlyDataAccepted(ctx); !accepted {
		return c.writeUniOn(ctx, next, header, wire, trailer)
	}
	return nil
}

func (c *Client) writeUniOn(ctx context.Context, conn *quic.Conn, header, wire, trailer []byte) error {
	sctx, cancel := context.WithTimeout(ctx, c.opts.SendTimeout)
	defer cancel()
	stream, err := conn.OpenUniStreamSync(sctx)
	if err != nil {
		return classify(sctx, ErrConnection, err)
	}
	if deadline, ok := sctx.Deadline(); ok {
		_ = stream.SetWriteDeadline(deadline)
	}
	for _, part := range [][]byte{header, wire, trailer} {
		if _, err := stream.Write(part); err != nil {
			return classify(sctx, ErrWrite, err)
		}
	}
	if err := stream.Close(); err != nil {
		return wrap(ErrClosed, err)
	}
	return nil
}

func (c *Client) writeBi(ctx context.Context, header, wire, trailer []byte) (Admission, error) {
	conn, err := c.getOrConnect(ctx)
	if err != nil {
		return Admission{}, err
	}
	sctx, cancel := context.WithTimeout(ctx, c.opts.SendTimeout)
	defer cancel()
	stream, err := conn.OpenStreamSync(sctx)
	if err != nil {
		return Admission{}, classify(sctx, ErrConnection, err)
	}
	if deadline, ok := sctx.Deadline(); ok {
		_ = stream.SetDeadline(deadline)
	}
	for _, part := range [][]byte{header, wire, trailer} {
		if _, err := stream.Write(part); err != nil {
			return Admission{}, classify(sctx, ErrWrite, err)
		}
	}
	if err := stream.Close(); err != nil {
		return Admission{}, wrap(ErrClosed, err)
	}
	frame, err := io.ReadAll(io.LimitReader(stream, MaxAdmissionFrame+1))
	if err != nil {
		return Admission{}, classify(sctx, ErrRead, err)
	}
	if len(frame) > MaxAdmissionFrame {
		return Admission{}, wrap(ErrRead, errFrameTooLong)
	}
	admission, ok := DecodeAdmission(frame)
	if !ok {
		return Admission{}, ErrBadAdmission
	}
	return admission, nil
}

func classify(sctx context.Context, kind, err error) error {
	if sctx.Err() != nil {
		return ErrTimeout
	}
	return wrap(kind, err)
}

func (c *Client) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	if c.stopWatch != nil {
		c.stopWatch()
		<-c.watchDone
	}
	c.mu.Lock()
	if c.conn != nil {
		_ = c.conn.CloseWithError(0, "client closed")
	}
	c.mu.Unlock()
	c.shutdown()
	return nil
}

func (c *Client) shutdown() {
	_ = c.transport.Close()
	_ = c.udp.Close()
}
