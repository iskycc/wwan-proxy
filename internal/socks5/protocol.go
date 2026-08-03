package socks5

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
)

const (
	version5 = 5

	methodNone     = 0
	methodPassword = 2
	methodReject   = 0xff

	cmdConnect      = 1
	cmdBind         = 2
	cmdUDPAssociate = 3

	repSuccess             = 0
	repGeneralFailure      = 1
	repNotAllowed          = 2
	repNetworkUnreachable  = 3
	repHostUnreachable     = 4
	repConnectionRefused   = 5
	repTTLExpired          = 6
	repCommandNotSupported = 7
	repAddressNotSupported = 8

	atypIPv4   = 1
	atypDomain = 3
	atypIPv6   = 4
)

type address struct {
	Host string
	Port uint16
}

func (a address) String() string { return net.JoinHostPort(a.Host, fmt.Sprint(a.Port)) }

func readAddress(r io.Reader) (address, error) {
	var typ [1]byte
	if _, err := io.ReadFull(r, typ[:]); err != nil {
		return address{}, err
	}
	var host string
	switch typ[0] {
	case atypIPv4:
		b := make([]byte, net.IPv4len)
		if _, err := io.ReadFull(r, b); err != nil {
			return address{}, err
		}
		host = net.IP(b).String()
	case atypIPv6:
		b := make([]byte, net.IPv6len)
		if _, err := io.ReadFull(r, b); err != nil {
			return address{}, err
		}
		host = net.IP(b).String()
	case atypDomain:
		var n [1]byte
		if _, err := io.ReadFull(r, n[:]); err != nil {
			return address{}, err
		}
		if n[0] == 0 {
			return address{}, fmt.Errorf("empty domain")
		}
		b := make([]byte, int(n[0]))
		if _, err := io.ReadFull(r, b); err != nil {
			return address{}, err
		}
		host = string(b)
	default:
		return address{}, errAddressType
	}
	var port [2]byte
	if _, err := io.ReadFull(r, port[:]); err != nil {
		return address{}, err
	}
	return address{Host: host, Port: binary.BigEndian.Uint16(port[:])}, nil
}

func appendAddress(dst []byte, ip net.IP, port int) []byte {
	if v4 := ip.To4(); v4 != nil {
		dst = append(dst, atypIPv4)
		dst = append(dst, v4...)
	} else {
		dst = append(dst, atypIPv6)
		dst = append(dst, ip.To16()...)
	}
	return binary.BigEndian.AppendUint16(dst, uint16(port))
}

func writeReply(w io.Writer, rep byte, addr net.Addr) error {
	ip := net.IPv4zero
	port := 0
	switch a := addr.(type) {
	case *net.TCPAddr:
		ip, port = a.IP, a.Port
	case *net.UDPAddr:
		ip, port = a.IP, a.Port
	}
	if ip == nil || ip.IsUnspecified() {
		ip = net.IPv4zero
	}
	b := appendAddress([]byte{version5, rep, 0}, ip, port)
	_, err := w.Write(b)
	return err
}

func parseUDPDatagram(b []byte) (address, []byte, error) {
	if len(b) < 4 || b[0] != 0 || b[1] != 0 {
		return address{}, nil, fmt.Errorf("invalid UDP reserved field")
	}
	if b[2] != 0 {
		return address{}, nil, errFragmentedUDP
	}
	a, used, err := addressFromBytes(b[3:])
	if err != nil {
		return address{}, nil, err
	}
	return a, b[3+used:], nil
}

func addressFromBytes(b []byte) (address, int, error) {
	if len(b) < 1 {
		return address{}, 0, io.ErrUnexpectedEOF
	}
	var hostLen, off int
	switch b[0] {
	case atypIPv4:
		hostLen, off = 4, 1
	case atypIPv6:
		hostLen, off = 16, 1
	case atypDomain:
		if len(b) < 2 || b[1] == 0 {
			return address{}, 0, io.ErrUnexpectedEOF
		}
		hostLen, off = int(b[1]), 2
	default:
		return address{}, 0, errAddressType
	}
	if len(b) < off+hostLen+2 {
		return address{}, 0, io.ErrUnexpectedEOF
	}
	hostBytes := b[off : off+hostLen]
	host := string(hostBytes)
	if b[0] != atypDomain {
		host = net.IP(hostBytes).String()
	}
	port := binary.BigEndian.Uint16(b[off+hostLen : off+hostLen+2])
	return address{Host: host, Port: port}, off + hostLen + 2, nil
}

func makeUDPDatagram(from *net.UDPAddr, payload []byte) []byte {
	b := appendAddress([]byte{0, 0, 0}, from.IP, from.Port)
	return append(b, payload...)
}
