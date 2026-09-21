//go:build linux

package main

import (
	"fmt"
	"net"
	"syscall"
	"time"
	"unsafe"
)

// Horodatage des reponses par le noyau (SO_TIMESTAMPNS) : l'heure de
// reception est relevee par la pile reseau a l'arrivee du paquet, et non
// quand notre boucle de lecture se reveille. Sans cela, toute charge du
// processus (requetes web, ramasse-miettes, calculs) s'ajoute au temps
// de reponse mesure : c'est exactement ce qui fausse les graphes sous
// charge.
func enableKernelRxTimestamps(conn net.PacketConn) bool {
	sc, ok := conn.(syscall.Conn)
	if !ok {
		return false
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return false
	}
	var serr error
	if err := raw.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_TIMESTAMPNS, 1)
	}); err != nil {
		return false
	}
	return serr == nil
}

// readStamped lit un paquet et renvoie son heure d'arrivee en
// nanosecondes Unix : celle du noyau si elle est disponible, sinon
// l'heure courante.
func readStamped(conn net.PacketConn, buf, oob []byte) (int, int64, error) {
	ipc, ok := conn.(*net.IPConn)
	if !ok {
		n, _, err := conn.ReadFrom(buf)
		return n, time.Now().UnixNano(), err
	}
	n, oobn, _, _, err := ipc.ReadMsgIP(buf, oob)
	if err != nil {
		return n, 0, err
	}
	return n, stampFromOOB(oob[:oobn]), nil
}

// stampFromOOB extracts the kernel receive time from ancillary data, or
// returns the current time if there is none.
func stampFromOOB(oob []byte) int64 {
	if len(oob) > 0 {
		if msgs, perr := syscall.ParseSocketControlMessage(oob); perr == nil {
			for _, m := range msgs {
				if m.Header.Level == syscall.SOL_SOCKET && m.Header.Type == syscall.SCM_TIMESTAMPNS &&
					len(m.Data) >= int(unsafe.Sizeof(syscall.Timespec{})) {
					ts := (*syscall.Timespec)(unsafe.Pointer(&m.Data[0]))
					return ts.Nano()
				}
			}
		}
	}
	return time.Now().UnixNano()
}

// kernelTCPRTT lit le RTT de la poignee de main mesure par le noyau
// (TCP_INFO, tcpi_rtt, en microsecondes).
func kernelTCPRTT(c net.Conn) (float64, bool) {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return 0, false
	}
	raw, err := tc.SyscallConn()
	if err != nil {
		return 0, false
	}
	var info syscall.TCPInfo
	var gerr syscall.Errno
	raw.Control(func(fd uintptr) {
		l := uint32(unsafe.Sizeof(info))
		_, _, gerr = syscall.Syscall6(syscall.SYS_GETSOCKOPT, fd, syscall.IPPROTO_TCP,
			syscall.TCP_INFO, uintptr(unsafe.Pointer(&info)), uintptr(unsafe.Pointer(&l)), 0)
	})
	if gerr != 0 || info.Rtt == 0 {
		return 0, false
	}
	return float64(info.Rtt), true
}

// setTTL sets the TTL (IPv4) or hop limit (IPv6) of packets sent on a raw
// socket. Used only on the traceroute sockets.
func setTTL(conn net.PacketConn, ttl int, v6 bool) error {
	sc, ok := conn.(syscall.Conn)
	if !ok {
		return fmt.Errorf("socket does not support TTL control")
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	raw.Control(func(fd uintptr) {
		if v6 {
			serr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IPV6, syscall.IPV6_UNICAST_HOPS, ttl)
		} else {
			serr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_TTL, ttl)
		}
	})
	return serr
}
