package conf

import (
	"net"
	"strconv"
	"strings"

	v2net "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/proxy/freedom"
	"google.golang.org/protobuf/proto"
)

type FreedomConfig struct {
	DomainStrategy string    `json:"domainStrategy"`
	Timeout        *uint32   `json:"timeout"`
	Redirect       string    `json:"redirect"`
	UserLevel      uint32    `json:"userLevel"`
	Fragment       *Fragment `json:"fragment"`
	ProxyProtocol  uint32    `json:"proxyProtocol"`
}

type Fragment struct {
	Packets  string `json:"packets"`
	Length   string `json:"length"`
	Interval string `json:"interval"`
	Fixed    uint64 `json:"fixed"`
}

// Build implements Buildable
func (c *FreedomConfig) Build() (proto.Message, error) {
	config := new(freedom.Config)
	switch strings.ToLower(c.DomainStrategy) {
	case "asis", "":
		config.DomainStrategy = freedom.Config_AS_IS
	case "useip":
		config.DomainStrategy = freedom.Config_USE_IP
	case "useipv4":
		config.DomainStrategy = freedom.Config_USE_IP4
	case "useipv6":
		config.DomainStrategy = freedom.Config_USE_IP6
	case "useipv4v6":
		config.DomainStrategy = freedom.Config_USE_IP46
	case "useipv6v4":
		config.DomainStrategy = freedom.Config_USE_IP64
	case "forceip":
		config.DomainStrategy = freedom.Config_FORCE_IP
	case "forceipv4":
		config.DomainStrategy = freedom.Config_FORCE_IP4
	case "forceipv6":
		config.DomainStrategy = freedom.Config_FORCE_IP6
	case "forceipv4v6":
		config.DomainStrategy = freedom.Config_FORCE_IP46
	case "forceipv6v4":
		config.DomainStrategy = freedom.Config_FORCE_IP64
	default:
		return nil, newError("unsupported domain strategy: ", c.DomainStrategy)
	}

	if c.Fragment != nil {
		config.Fragment = new(freedom.Fragment)
		var err, err2 error

		switch strings.ToLower(c.Fragment.Packets) {
		case "tlshello":
			// TLS Hello Fragmentation (into multiple handshake messages)
			config.Fragment.PacketsFrom = 0
			config.Fragment.PacketsTo = 1
		case "":
			// TCP Segmentation (all packets)
			config.Fragment.PacketsFrom = 0
			config.Fragment.PacketsTo = 0
		default:
			// TCP Segmentation (range)
			packetsFromTo := strings.Split(c.Fragment.Packets, "-")
			if len(packetsFromTo) == 2 {
				config.Fragment.PacketsFrom, err = strconv.ParseUint(packetsFromTo[0], 10, 64)
				config.Fragment.PacketsTo, err2 = strconv.ParseUint(packetsFromTo[1], 10, 64)
			} else {
				config.Fragment.PacketsFrom, err = strconv.ParseUint(packetsFromTo[0], 10, 64)
				config.Fragment.PacketsTo = config.Fragment.PacketsFrom
			}
			if err != nil {
				return nil, newError("Invalid PacketsFrom").Base(err)
			}
			if err2 != nil {
				return nil, newError("Invalid PacketsTo").Base(err2)
			}
			if config.Fragment.PacketsFrom > config.Fragment.PacketsTo {
				config.Fragment.PacketsFrom, config.Fragment.PacketsTo = config.Fragment.PacketsTo, config.Fragment.PacketsFrom
			}
			if config.Fragment.PacketsFrom == 0 {
				return nil, newError("PacketsFrom can't be 0")
			}
		}

		{
			if c.Fragment.Length == "" {
				return nil, newError("Length can't be empty")
			}
			lengthMinMax := strings.Split(c.Fragment.Length, "-")
			if len(lengthMinMax) == 2 {
				config.Fragment.LengthMin, err = strconv.ParseUint(lengthMinMax[0], 10, 64)
				config.Fragment.LengthMax, err2 = strconv.ParseUint(lengthMinMax[1], 10, 64)
			} else {
				config.Fragment.LengthMin, err = strconv.ParseUint(lengthMinMax[0], 10, 64)
				config.Fragment.LengthMax = config.Fragment.LengthMin
			}
			if err != nil {
				return nil, newError("Invalid LengthMin").Base(err)
			}
			if err2 != nil {
				return nil, newError("Invalid LengthMax").Base(err2)
			}
			if config.Fragment.LengthMin > config.Fragment.LengthMax {
				config.Fragment.LengthMin, config.Fragment.LengthMax = config.Fragment.LengthMax, config.Fragment.LengthMin
			}
			if config.Fragment.LengthMin == 0 {
				return nil, newError("LengthMin can't be 0")
			}
		}

		{
			if c.Fragment.Interval == "" {
				return nil, newError("Interval can't be empty")
			}
			intervalMinMax := strings.Split(c.Fragment.Interval, "-")
			if len(intervalMinMax) == 2 {
				config.Fragment.IntervalMin, err = strconv.ParseUint(intervalMinMax[0], 10, 64)
				config.Fragment.IntervalMax, err2 = strconv.ParseUint(intervalMinMax[1], 10, 64)
			} else {
				config.Fragment.IntervalMin, err = strconv.ParseUint(intervalMinMax[0], 10, 64)
				config.Fragment.IntervalMax = config.Fragment.IntervalMin
			}
			if err != nil {
				return nil, newError("Invalid IntervalMin").Base(err)
			}
			if err2 != nil {
				return nil, newError("Invalid IntervalMax").Base(err2)
			}
			if config.Fragment.IntervalMin > config.Fragment.IntervalMax {
				config.Fragment.IntervalMin, config.Fragment.IntervalMax = config.Fragment.IntervalMax, config.Fragment.IntervalMin
			}
		}

		{
			config.Fragment.Fixed = c.Fragment.Fixed
		}

	}

	if c.Timeout != nil {
		config.Timeout = *c.Timeout
	}
	config.UserLevel = c.UserLevel
	if len(c.Redirect) > 0 {
		host, portStr, err := net.SplitHostPort(c.Redirect)
		if err != nil {
			return nil, newError("invalid redirect address: ", c.Redirect, ": ", err).Base(err)
		}
		port, err := v2net.PortFromString(portStr)
		if err != nil {
			return nil, newError("invalid redirect port: ", c.Redirect, ": ", err).Base(err)
		}
		config.DestinationOverride = &freedom.DestinationOverride{
			Server: &protocol.ServerEndpoint{
				Port: uint32(port),
			},
		}

		if len(host) > 0 {
			config.DestinationOverride.Server.Address = v2net.NewIPOrDomain(v2net.ParseAddress(host))
		}
	}
	if c.ProxyProtocol > 0 && c.ProxyProtocol <= 2 {
		config.ProxyProtocol = c.ProxyProtocol
	}
	return config, nil
}
