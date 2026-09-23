package main

import (
	"net"
	"os"
	"strings"
)

// Running in a container is fine for a test, but the measurements are only
// as good as the container's network. Rather than leaving that in the
// documentation only, the service says so itself: once in the log at start,
// and as a note in the back-office.

type containerNote struct {
	InContainer bool   `json:"in_container"`
	HostNetwork bool   `json:"host_network"`
	Message     string `json:"message"`
}

// inContainerFrom keeps the decision testable: the markers are read from
// the system by inContainer.
func inContainerFrom(dockerEnv bool, cgroup string) bool {
	if dockerEnv {
		return true
	}
	for _, marker := range []string{"docker", "containerd", "kubepods", "lxc"} {
		if strings.Contains(cgroup, marker) {
			return true
		}
	}
	return false
}

func inContainer() bool {
	_, err := os.Stat("/.dockerenv")
	cgroup, _ := os.ReadFile("/proc/1/cgroup")
	return inContainerFrom(err == nil, string(cgroup))
}

// hostNetwork guesses whether the container shares the host's network
// stack. With Docker's default bridge the container sits on a private
// 172.16/12 address behind NAT, and every probe crosses it.
// looksBridged reports whether an interface looks like Docker's default
// bridge: an "eth" interface on the private 172.16/12 range it uses.
func looksBridged(name string, ip net.IP) bool {
	v4 := ip.To4()
	if v4 == nil || !strings.HasPrefix(name, "eth") {
		return false
	}
	return v4[0] == 172 && v4[1]&0xf0 == 16
}

func hostNetwork() bool {
	ifaces, err := net.Interfaces()
	if err != nil {
		return true // cannot tell: do not cry wolf
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && looksBridged(ifc.Name, n.IP) {
				return false
			}
		}
	}
	return true
}

func containerStatus() containerNote { return containerNoteFor(inContainer(), hostNetwork()) }

func containerNoteFor(in, host bool) containerNote {
	if !in {
		return containerNote{}
	}
	n := containerNote{InContainer: true, HostNetwork: host}
	if n.HostNetwork {
		n.Message = "running in a container with the host network: measurements are as accurate as a native install, " +
			"but in-place signed updates and the automatic rollback are unavailable (update by pulling a new image)"
	} else {
		n.Message = "running in a container behind Docker's bridge: every probe crosses a NAT, which adds latency and jitter, " +
			"traceroutes start with the bridge as first hop, and IPv6 is usually off. Restart with --network host"
	}
	return n
}
