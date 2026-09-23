package main

import (
	"net"
	"strings"
	"testing"
)

func TestContainerDetection(t *testing.T) {
	cases := []struct {
		name      string
		dockerEnv bool
		cgroup    string
		want      bool
	}{
		{"native server", false, "0::/init.scope", false},
		{"docker marker file", true, "", true},
		{"docker cgroup", false, "0::/docker/6f2c…", true},
		{"containerd", false, "0::/system.slice/containerd.service", true},
		{"kubernetes", false, "0::/kubepods/besteffort/pod123", true},
	}
	for _, c := range cases {
		if got := inContainerFrom(c.dockerEnv, c.cgroup); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
	// Docker's default bridge gives the container a 172.16/12 address.
	if !looksBridged("eth0", net.ParseIP("172.17.0.2")) {
		t.Error("the Docker bridge should be recognised")
	}
	for _, ip := range []string{"192.168.1.10", "10.0.0.5", "172.32.0.1"} {
		if looksBridged("eth0", net.ParseIP(ip)) {
			t.Errorf("%s is not the Docker bridge", ip)
		}
	}
	if looksBridged("ens3", net.ParseIP("172.17.0.2")) {
		t.Error("a host interface must not be taken for the bridge")
	}
}

func TestContainerMessage(t *testing.T) {
	if n := containerNoteFor(false, true); n.InContainer || n.Message != "" {
		t.Error("a native install must say nothing")
	}
	bridged := containerNoteFor(true, false)
	if !strings.Contains(bridged.Message, "--network host") {
		t.Errorf("the bridged case must tell what to do: %q", bridged.Message)
	}
	host := containerNoteFor(true, true)
	if strings.Contains(host.Message, "NAT") || !strings.Contains(host.Message, "updates") {
		t.Errorf("with the host network, only the update caveat remains: %q", host.Message)
	}
}
