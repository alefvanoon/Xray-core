package freedom

//go:generate go run github.com/xtls/xray-core/common/errors/errorgen

import (
	"context"
	"bytes"
	"crypto/rand"
	"io"
	"math/big"
	"time"
	"fmt"

	"github.com/pires/go-proxyproto"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/dice"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/platform"
	"github.com/xtls/xray-core/common/retry"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/common/signal"
	"github.com/xtls/xray-core/common/task"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/dns"
	"github.com/xtls/xray-core/features/policy"
	"github.com/xtls/xray-core/features/stats"
	"github.com/xtls/xray-core/proxy"
	"github.com/xtls/xray-core/transport"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/stat"
)

var useSplice bool

func init() {
	common.Must(common.RegisterConfig((*Config)(nil), func(ctx context.Context, config interface{}) (interface{}, error) {
		h := new(Handler)
		if err := core.RequireFeatures(ctx, func(pm policy.Manager, d dns.Client) error {
			return h.Init(config.(*Config), pm, d)
		}); err != nil {
			return nil, err
		}
		return h, nil
	}))
	const defaultFlagValue = "NOT_DEFINED_AT_ALL"
	value := platform.NewEnvFlag(platform.UseFreedomSplice).GetValue(func() string { return defaultFlagValue })
	switch value {
	case defaultFlagValue, "auto", "enable":
		useSplice = true
	}
}

// Handler handles Freedom connections.
type Handler struct {
	policyManager policy.Manager
	dns           dns.Client
	config        *Config
}

// Init initializes the Handler with necessary parameters.
func (h *Handler) Init(config *Config, pm policy.Manager, d dns.Client) error {
	h.config = config
	h.policyManager = pm
	h.dns = d

	return nil
}

func (h *Handler) policy() policy.Session {
	p := h.policyManager.ForLevel(h.config.UserLevel)
	if h.config.Timeout > 0 && h.config.UserLevel == 0 {
		p.Timeouts.ConnectionIdle = time.Duration(h.config.Timeout) * time.Second
	}
	return p
}

func (h *Handler) resolveIP(ctx context.Context, domain string, localAddr net.Address) net.Address {
	ips, err := h.dns.LookupIP(domain, dns.IPOption{
		IPv4Enable: (localAddr == nil || localAddr.Family().IsIPv4()) && h.config.preferIP4(),
		IPv6Enable: (localAddr == nil || localAddr.Family().IsIPv6()) && h.config.preferIP6(),
	})
	{ // Resolve fallback
		if (len(ips) == 0 || err != nil) && h.config.hasFallback() && localAddr == nil {
			ips, err = h.dns.LookupIP(domain, dns.IPOption{
				IPv4Enable: h.config.fallbackIP4(),
				IPv6Enable: h.config.fallbackIP6(),
			})
		}
	}
	if err != nil {
		newError("failed to get IP address for domain ", domain).Base(err).WriteToLog(session.ExportIDToError(ctx))
	}
	if len(ips) == 0 {
		return nil
	}
	return net.IPAddress(ips[dice.Roll(len(ips))])
}

func isValidAddress(addr *net.IPOrDomain) bool {
	if addr == nil {
		return false
	}

	a := addr.AsAddress()
	return a != net.AnyIP
}

// Process implements proxy.Outbound.
func (h *Handler) Process(ctx context.Context, link *transport.Link, dialer internet.Dialer) error {
	outbounds := session.OutboundsFromContext(ctx)
	ob := outbounds[len(outbounds)-1]
	if !ob.Target.IsValid() {
		return newError("target not specified.")
	}
	ob.Name = "freedom"
	ob.CanSpliceCopy = 1
	inbound := session.InboundFromContext(ctx)

	destination := ob.Target
	UDPOverride := net.UDPDestination(nil, 0)
	if h.config.DestinationOverride != nil {
		server := h.config.DestinationOverride.Server
		if isValidAddress(server.Address) {
			destination.Address = server.Address.AsAddress()
			UDPOverride.Address = destination.Address
		}
		if server.Port != 0 {
			destination.Port = net.Port(server.Port)
			UDPOverride.Port = destination.Port
		}
	}

	input := link.Reader
	output := link.Writer

	var conn stat.Connection
	err := retry.ExponentialBackoff(5, 100).On(func() error {
		dialDest := destination
		if h.config.hasStrategy() && dialDest.Address.Family().IsDomain() {
			ip := h.resolveIP(ctx, dialDest.Address.Domain(), dialer.Address())
			if ip != nil {
				dialDest = net.Destination{
					Network: dialDest.Network,
					Address: ip,
					Port:    dialDest.Port,
				}
				newError("dialing to ", dialDest).WriteToLog(session.ExportIDToError(ctx))
			} else if h.config.forceIP() {
				return dns.ErrEmptyResponse
			}
		}

		rawConn, err := dialer.Dial(ctx, dialDest)
		if err != nil {
			return err
		}

		if h.config.ProxyProtocol > 0 && h.config.ProxyProtocol <= 2 {
			version := byte(h.config.ProxyProtocol)
			srcAddr := inbound.Source.RawNetAddr()
			dstAddr := rawConn.RemoteAddr()
			header := proxyproto.HeaderProxyFromAddrs(version, srcAddr, dstAddr)
			if _, err = header.WriteTo(rawConn); err != nil {
				rawConn.Close()
				return err
			}
		}

		conn = rawConn
		return nil
	})
	if err != nil {
		return newError("failed to open connection to ", destination).Base(err)
	}
	defer conn.Close()
	newError("connection opened to ", destination, ", local endpoint ", conn.LocalAddr(), ", remote endpoint ", conn.RemoteAddr()).WriteToLog(session.ExportIDToError(ctx))

	var newCtx context.Context
	var newCancel context.CancelFunc
	if session.TimeoutOnlyFromContext(ctx) {
		newCtx, newCancel = context.WithCancel(context.Background())
	}

	plcy := h.policy()
	ctx, cancel := context.WithCancel(ctx)
	timer := signal.CancelAfterInactivity(ctx, func() {
		cancel()
		if newCancel != nil {
			newCancel()
		}
	}, plcy.Timeouts.ConnectionIdle)

	requestDone := func() error {
		defer timer.SetTimeout(plcy.Timeouts.DownlinkOnly)

		var writer buf.Writer
		if destination.Network == net.Network_TCP {
			if h.config.Fragment != nil {
				newError("FRAGMENT", h.config.Fragment.PacketsFrom, h.config.Fragment.PacketsTo, h.config.Fragment.LengthMin, h.config.Fragment.LengthMax,
					h.config.Fragment.IntervalMin, h.config.Fragment.IntervalMax).AtDebug().WriteToLog(session.ExportIDToError(ctx))
				writer = buf.NewWriter(&FragmentWriter{
					fragment: h.config.Fragment,
					writer:   conn,
				})
			} else {
				writer = buf.NewWriter(conn)
			}
		} else {
			writer = NewPacketWriter(conn, h, ctx, UDPOverride)
		}

		if err := buf.Copy(input, writer, buf.UpdateActivity(timer)); err != nil {
			return newError("failed to process request").Base(err)
		}

		return nil
	}

	responseDone := func() error {
		defer timer.SetTimeout(plcy.Timeouts.UplinkOnly)
		if destination.Network == net.Network_TCP {
			var writeConn net.Conn
			var inTimer *signal.ActivityTimer
			if inbound := session.InboundFromContext(ctx); inbound != nil && inbound.Conn != nil && useSplice {
				writeConn = inbound.Conn
				inTimer = inbound.Timer
			}
			return proxy.CopyRawConnIfExist(ctx, conn, writeConn, link.Writer, timer, inTimer)
		}
		reader := NewPacketReader(conn, UDPOverride)
		if err := buf.Copy(reader, output, buf.UpdateActivity(timer)); err != nil {
			return newError("failed to process response").Base(err)
		}
		return nil
	}

	if newCtx != nil {
		ctx = newCtx
	}

	if err := task.Run(ctx, requestDone, task.OnSuccess(responseDone, task.Close(output))); err != nil {
		return newError("connection ends").Base(err)
	}

	return nil
}

func NewPacketReader(conn net.Conn, UDPOverride net.Destination) buf.Reader {
	iConn := conn
	statConn, ok := iConn.(*stat.CounterConnection)
	if ok {
		iConn = statConn.Connection
	}
	var counter stats.Counter
	if statConn != nil {
		counter = statConn.ReadCounter
	}
	if c, ok := iConn.(*internet.PacketConnWrapper); ok && UDPOverride.Address == nil && UDPOverride.Port == 0 {
		return &PacketReader{
			PacketConnWrapper: c,
			Counter:           counter,
		}
	}
	return &buf.PacketReader{Reader: conn}
}

type PacketReader struct {
	*internet.PacketConnWrapper
	stats.Counter
}

func (r *PacketReader) ReadMultiBuffer() (buf.MultiBuffer, error) {
	b := buf.New()
	b.Resize(0, buf.Size)
	n, d, err := r.PacketConnWrapper.ReadFrom(b.Bytes())
	if err != nil {
		b.Release()
		return nil, err
	}
	b.Resize(0, int32(n))
	b.UDP = &net.Destination{
		Address: net.IPAddress(d.(*net.UDPAddr).IP),
		Port:    net.Port(d.(*net.UDPAddr).Port),
		Network: net.Network_UDP,
	}
	if r.Counter != nil {
		r.Counter.Add(int64(n))
	}
	return buf.MultiBuffer{b}, nil
}

func NewPacketWriter(conn net.Conn, h *Handler, ctx context.Context, UDPOverride net.Destination) buf.Writer {
	iConn := conn
	statConn, ok := iConn.(*stat.CounterConnection)
	if ok {
		iConn = statConn.Connection
	}
	var counter stats.Counter
	if statConn != nil {
		counter = statConn.WriteCounter
	}
	if c, ok := iConn.(*internet.PacketConnWrapper); ok {
		return &PacketWriter{
			PacketConnWrapper: c,
			Counter:           counter,
			Handler:           h,
			Context:           ctx,
			UDPOverride:       UDPOverride,
		}
	}
	return &buf.SequentialWriter{Writer: conn}
}

type PacketWriter struct {
	*internet.PacketConnWrapper
	stats.Counter
	*Handler
	context.Context
	UDPOverride net.Destination
}

func (w *PacketWriter) WriteMultiBuffer(mb buf.MultiBuffer) error {
	for {
		mb2, b := buf.SplitFirst(mb)
		mb = mb2
		if b == nil {
			break
		}
		var n int
		var err error
		if b.UDP != nil {
			if w.UDPOverride.Address != nil {
				b.UDP.Address = w.UDPOverride.Address
			}
			if w.UDPOverride.Port != 0 {
				b.UDP.Port = w.UDPOverride.Port
			}
			if w.Handler.config.hasStrategy() && b.UDP.Address.Family().IsDomain() {
				ip := w.Handler.resolveIP(w.Context, b.UDP.Address.Domain(), nil)
				if ip != nil {
					b.UDP.Address = ip
				}
			}
			destAddr, _ := net.ResolveUDPAddr("udp", b.UDP.NetAddr())
			if destAddr == nil {
				b.Release()
				continue
			}
			n, err = w.PacketConnWrapper.WriteTo(b.Bytes(), destAddr)
		} else {
			n, err = w.PacketConnWrapper.Write(b.Bytes())
		}
		b.Release()
		if err != nil {
			buf.ReleaseMulti(mb)
			return err
		}
		if w.Counter != nil {
			w.Counter.Add(int64(n))
		}
	}
	return nil
}

type FragmentWriter struct {
	fragment *Fragment
	writer   io.Writer
	count    uint64
}

// Write function with the custom logic to split the "Hello packet".
func (f *FragmentWriter) Write(b []byte) (int, error) {
	// --- START: Custom logic for "Hello packet" ---

	// Define the specific string that identifies the packet to be split.
	// CORRECTED: Variable name cannot contain a hyphen.
	targetSubstring := []byte("www.zzula.ir")
	
	// Check if the incoming data contains the target string.
	if bytes.Contains(b, targetSubstring) {
		// Define the precise marker for the split. We want to split between the two 'z's.
		// Finding "zzula.ir" is a reliable way to locate the split point.
		splitMarker := []byte("zzula.ir")
		markerIndex := bytes.Index(b, splitMarker)

		// If the marker is found, proceed with the special split logic.
		if markerIndex != -1 {
			// The split happens right after the first 'z'.
			splitPoint := markerIndex + 1

			part1 := b[:splitPoint]
			part2 := b[splitPoint:]

			// Write the first part of the packet.
			n1, err := f.writer.Write(part1)
			if err != nil {
				// If the write fails, return bytes written and the error.
				return n1, err
			}

			// Pause for the specified delay.
			time.Sleep(time.Duration(randBetween(int64(f.fragment.IntervalMin), int64(f.fragment.IntervalMax))) * time.Millisecond)
			fmt.Println("Special string  detected, splitting packet.")
			// Write the second part of the packet.
			n2, err := f.writer.Write(part2)
			if err != nil {
				// If the second write fails, return total bytes written and the error.
				return n1 + n2, err
			}

			// On success, report that the entire original buffer was written.
			return len(b), nil
		}
	}

	targetSubstring = []byte("orgtgju.org")
	
	if bytes.Contains(b, targetSubstring) {
		// Define the precise marker for the split. We want to split between the two 'z's.
		// Finding "zzula.ir" is a reliable way to locate the split point.
		splitMarker := []byte("orgtgju.org")
		markerIndex := bytes.Index(b, splitMarker)

		// If the marker is found, proceed with the special split logic.
		if markerIndex != -1 {
			// The split happens right after the first 'z'.
			splitPoint := markerIndex + 3

			part1 := b[:splitPoint]
			part2 := b[splitPoint:]

			// Write the first part of the packet.
			n1, err := f.writer.Write(part1)
			if err != nil {
				// If the write fails, return bytes written and the error.
				return n1, err
			}

			// Pause for the specified delay.
			time.Sleep(time.Duration(randBetween(int64(f.fragment.IntervalMin), int64(f.fragment.IntervalMax))) * time.Millisecond)
			fmt.Println("Special string  detected tgju, splitting packet.")
			// Write the second part of the packet.
			n2, err := f.writer.Write(part2)
			if err != nil {
				// If the second write fails, return total bytes written and the error.
				return n1 + n2, err
			}

			// On success, report that the entire original buffer was written.
			return len(b), nil
		}
	}
	// --- END: Custom logic. Fallback to original logic if not the special packet. ---

	f.count++

	// This is the original logic for generic fragmentation.
	if f.fragment.Fixed > 0 && len(b) > int(f.fragment.Fixed) {
		fragPart := b[:len(b)-int(f.fragment.Fixed)]
		fixedPart := b[len(b)-int(f.fragment.Fixed):]

		var totalWritten int
		for from := 0; ; {
			to := from + int(randBetween(int64(f.fragment.LengthMin), int64(f.fragment.LengthMax)))
			if to > len(fragPart) {
				to = len(fragPart)
			}
			n, err := f.writer.Write(fragPart[from:to])
			totalWritten += n
			from += n
			if err != nil {
				return totalWritten, err
			}
			if from >= len(fragPart) {
				break
			}
			time.Sleep(time.Duration(randBetween(int64(f.fragment.IntervalMin), int64(f.fragment.IntervalMax))) * time.Millisecond)
		}

		n, err := f.writer.Write(fixedPart)
		totalWritten += n
		if err != nil {
			return totalWritten, err
		}
		return len(b), nil
	}

	if f.fragment.PacketsFrom != 0 && (f.count < f.fragment.PacketsFrom || f.count > f.fragment.PacketsTo) {
		return f.writer.Write(b)
	}

	var totalWritten int
	for from := 0; ; {
		to := from + int(randBetween(int64(f.fragment.LengthMin), int64(f.fragment.LengthMax)))
		if to > len(b) {
			to = len(b)
		}
		n, err := f.writer.Write(b[from:to])
		totalWritten += n
		from += n
		if err != nil {
			return totalWritten, err
		}
		if from >= len(b) {
			return totalWritten, nil
		}
		time.Sleep(time.Duration(randBetween(int64(f.fragment.IntervalMin), int64(f.fragment.IntervalMax))) * time.Millisecond)
	}
}

// stolen from github.com/xtls/xray-core/transport/internet/reality
func randBetween(left int64, right int64) int64 {
	if left == right {
		return left
	}
	bigInt, _ := rand.Int(rand.Reader, big.NewInt(right-left))
	return left + bigInt.Int64()
}
