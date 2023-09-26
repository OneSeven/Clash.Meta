package outbound

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	E "github.com/sagernet/sing/common/exceptions"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/common/uot"
	"io"
	"net"
	"strconv"

	N "github.com/Dreamacro/clash/common/net"
	"github.com/Dreamacro/clash/component/ca"
	"github.com/Dreamacro/clash/component/dialer"
	"github.com/Dreamacro/clash/component/proxydialer"
	C "github.com/Dreamacro/clash/constant"
	"github.com/Dreamacro/clash/transport/socks5"
)

type Socks5 struct {
	*Base
	option         *Socks5Option
	user           string
	pass           string
	tls            bool
	skipCertVerify bool
	tlsConfig      *tls.Config
}

type Socks5Option struct {
	BasicOption
	Name              string `proxy:"name"`
	Server            string `proxy:"server"`
	Port              int    `proxy:"port"`
	UserName          string `proxy:"username,omitempty"`
	Password          string `proxy:"password,omitempty"`
	TLS               bool   `proxy:"tls,omitempty"`
	UDP               bool   `proxy:"udp,omitempty"`
	SkipCertVerify    bool   `proxy:"skip-cert-verify,omitempty"`
	Fingerprint       string `proxy:"fingerprint,omitempty"`
	UDPOverTCP        bool   `proxy:"udp-over-tcp,omitempty"`
	UDPOverTCPVersion int    `proxy:"udp-over-tcp-version,omitempty"`
}

// StreamConnContext implements C.ProxyAdapter
func (ss *Socks5) StreamConnContext(ctx context.Context, c net.Conn, metadata *C.Metadata) (net.Conn, error) {
	if ss.tls {
		cc := tls.Client(c, ss.tlsConfig)
		err := cc.HandshakeContext(ctx)
		c = cc
		if err != nil {
			return nil, fmt.Errorf("%s connect error: %w", ss.addr, err)
		}
	}

	var user *socks5.User
	if ss.user != "" {
		user = &socks5.User{
			Username: ss.user,
			Password: ss.pass,
		}
	}
	if _, err := socks5.ClientHandshake(c, serializesSocksAddr(metadata), socks5.CmdConnect, user); err != nil {
		return nil, err
	}
	if metadata.NetWork == C.UDP && ss.option.UDPOverTCP {
		switch ss.option.UDPOverTCPVersion {
		case 0, uot.Version:
			request := uot.Request{
				IsConnect:   true,
				Destination: uot.RequestDestination(uint8(ss.option.UDPOverTCPVersion)),
			}
			return uot.NewLazyConn(c, request), nil
		case uot.LegacyVersion:
			return uot.NewConn(c, uot.Request{}), nil
		default:
			return nil, E.New("unknown protocol version: ", ss.option.UDPOverTCPVersion)
		}
	}
	return c, nil
}

// DialContext implements C.ProxyAdapter
func (ss *Socks5) DialContext(ctx context.Context, metadata *C.Metadata, opts ...dialer.Option) (_ C.Conn, err error) {
	return ss.DialContextWithDialer(ctx, dialer.NewDialer(ss.Base.DialOptions(opts...)...), metadata)
}

// DialContextWithDialer implements C.ProxyAdapter
func (ss *Socks5) DialContextWithDialer(ctx context.Context, dialer C.Dialer, metadata *C.Metadata) (_ C.Conn, err error) {
	if len(ss.option.DialerProxy) > 0 {
		dialer, err = proxydialer.NewByName(ss.option.DialerProxy, dialer)
		if err != nil {
			return nil, err
		}
	}
	c, err := dialer.DialContext(ctx, "tcp", ss.addr)
	if err != nil {
		return nil, fmt.Errorf("%s connect error: %w", ss.addr, err)
	}
	N.TCPKeepAlive(c)

	defer func(c net.Conn) {
		safeConnClose(c, err)
	}(c)

	c, err = ss.StreamConnContext(ctx, c, metadata)
	if err != nil {
		return nil, err
	}

	return NewConn(c, ss), nil
}

// SupportWithDialer implements C.ProxyAdapter
func (ss *Socks5) SupportWithDialer() C.NetWork {
	return C.TCP
}

// ListenPacketContext implements C.ProxyAdapter
func (ss *Socks5) ListenPacketContext(ctx context.Context, metadata *C.Metadata, opts ...dialer.Option) (_ C.PacketConn, err error) {
	var cDialer C.Dialer = dialer.NewDialer(ss.Base.DialOptions(opts...)...)
	if len(ss.option.DialerProxy) > 0 {
		cDialer, err = proxydialer.NewByName(ss.option.DialerProxy, cDialer)
		if err != nil {
			return nil, err
		}
	}
	c, err := cDialer.DialContext(ctx, "tcp", ss.addr)
	if err != nil {
		err = fmt.Errorf("%s connect error: %w", ss.addr, err)
		return
	}

	if ss.tls {
		cc := tls.Client(c, ss.tlsConfig)
		ctx, cancel := context.WithTimeout(context.Background(), C.DefaultTLSTimeout)
		defer cancel()
		err = cc.HandshakeContext(ctx)
		c = cc
	}

	defer func(c net.Conn) {
		safeConnClose(c, err)
	}(c)

	N.TCPKeepAlive(c)
	var user *socks5.User
	if ss.user != "" {
		user = &socks5.User{
			Username: ss.user,
			Password: ss.pass,
		}
	}

	bindAddr, err := socks5.ClientHandshake(c, serializesSocksAddr(metadata), socks5.CmdUDPAssociate, user)
	if err != nil {
		err = fmt.Errorf("client hanshake error: %w", err)
		return
	}

	// Support unspecified UDP bind address.
	bindUDPAddr := bindAddr.UDPAddr()
	if bindUDPAddr == nil {
		err = errors.New("invalid UDP bind address")
		return
	} else if bindUDPAddr.IP.IsUnspecified() {
		serverAddr, err := resolveUDPAddr(ctx, "udp", ss.Addr())
		if err != nil {
			return nil, err
		}

		bindUDPAddr.IP = serverAddr.IP
	}

	pc, err := cDialer.ListenPacket(ctx, "udp", "", bindUDPAddr.AddrPort())
	if err != nil {
		return
	}

	go func() {
		io.Copy(io.Discard, c)
		c.Close()
		// A UDP association terminates when the TCP connection that the UDP
		// ASSOCIATE request arrived on terminates. RFC1928
		pc.Close()
	}()
	if ss.option.UDPOverTCP {
		destination := M.SocksaddrFromNet(metadata.UDPAddr())
		switch ss.option.UDPOverTCPVersion {
		case 0, uot.Version:
			return newPacketConn(uot.NewLazyConn(c, uot.Request{Destination: destination}), ss), nil
		case uot.LegacyVersion:
			return newPacketConn(uot.NewConn(c, uot.Request{Destination: destination}), ss), nil
		default:
			return nil, E.New("unknown protocol version: ", ss.option.UDPOverTCPVersion)
		}
	}
	return newPacketConn(&socksPacketConn{PacketConn: pc, rAddr: bindUDPAddr, tcpConn: c}, ss), nil
}

// SupportUOT implements C.ProxyAdapter
func (ss *Socks5) SupportUOT() bool {
	return ss.option.UDPOverTCP
}

func NewSocks5(option Socks5Option) (*Socks5, error) {
	var tlsConfig *tls.Config
	if option.TLS {
		tlsConfig = &tls.Config{
			InsecureSkipVerify: option.SkipCertVerify,
			ServerName:         option.Server,
		}

		var err error
		tlsConfig, err = ca.GetSpecifiedFingerprintTLSConfig(tlsConfig, option.Fingerprint)
		if err != nil {
			return nil, err
		}
	}

	return &Socks5{
		Base: &Base{
			name:   option.Name,
			addr:   net.JoinHostPort(option.Server, strconv.Itoa(option.Port)),
			tp:     C.Socks5,
			udp:    option.UDP,
			tfo:    option.TFO,
			mpTcp:  option.MPTCP,
			iface:  option.Interface,
			rmark:  option.RoutingMark,
			prefer: C.NewDNSPrefer(option.IPVersion),
		},
		option:         &option,
		user:           option.UserName,
		pass:           option.Password,
		tls:            option.TLS,
		skipCertVerify: option.SkipCertVerify,
		tlsConfig:      tlsConfig,
	}, nil
}

type socksPacketConn struct {
	net.PacketConn
	rAddr   net.Addr
	tcpConn net.Conn
}

func (uc *socksPacketConn) WriteTo(b []byte, addr net.Addr) (n int, err error) {
	packet, err := socks5.EncodeUDPPacket(socks5.ParseAddrToSocksAddr(addr), b)
	if err != nil {
		return
	}
	return uc.PacketConn.WriteTo(packet, uc.rAddr)
}

func (uc *socksPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	n, _, e := uc.PacketConn.ReadFrom(b)
	if e != nil {
		return 0, nil, e
	}
	addr, payload, err := socks5.DecodeUDPPacket(b)
	if err != nil {
		return 0, nil, err
	}

	udpAddr := addr.UDPAddr()
	if udpAddr == nil {
		return 0, nil, errors.New("parse udp addr error")
	}

	// due to DecodeUDPPacket is mutable, record addr length
	copy(b, payload)
	return n - len(addr) - 3, udpAddr, nil
}

func (uc *socksPacketConn) Close() error {
	uc.tcpConn.Close()
	return uc.PacketConn.Close()
}
