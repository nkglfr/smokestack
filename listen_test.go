package main

import (
	"strings"
	"testing"
)

func TestResolveListen(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	none := env(nil)
	cases := []struct {
		name string
		cfg  Config
		src  listenSource
		want string
	}{
		{"default", Config{}, listenSource{getenv: none}, "127.0.0.1:8080"},
		{"older config", Config{Listen: "0.0.0.0:9000"}, listenSource{getenv: none}, "0.0.0.0:9000"},
		{"config fields", Config{ListenIP: "192.0.2.10", ListenPort: 8443}, listenSource{getenv: none}, "192.0.2.10:8443"},
		{"env beats config", Config{ListenIP: "192.0.2.10"},
			listenSource{getenv: env(map[string]string{"SMOKESTACK_LISTEN_IP": "0.0.0.0", "SMOKESTACK_LISTEN_PORT": "80"})}, "0.0.0.0:80"},
		{"port only", Config{}, listenSource{getenv: env(map[string]string{"SMOKESTACK_LISTEN_PORT": "9090"})}, "127.0.0.1:9090"},
		{"IPv6 without brackets", Config{}, listenSource{getenv: env(map[string]string{"SMOKESTACK_LISTEN_IP": "::"})}, "[::]:8080"},
		{"IPv6 with brackets", Config{}, listenSource{getenv: env(map[string]string{"SMOKESTACK_LISTEN_IP": "[2001:db8::5]"})}, "[2001:db8::5]:8080"},
		{"all interfaces", Config{}, listenSource{getenv: env(map[string]string{"SMOKESTACK_LISTEN_IP": "*"})}, ":8080"},
		{"flags beat env", Config{}, listenSource{flagIP: "127.0.0.1", flagPort: "7000",
			getenv: env(map[string]string{"SMOKESTACK_LISTEN_IP": "0.0.0.0"})}, "127.0.0.1:7000"},
		{"-listen flag", Config{}, listenSource{flagListen: "[::1]:8081", getenv: none}, "[::1]:8081"},
	}
	for _, c := range cases {
		got, err := resolveListen(c.cfg, c.src)
		if err != nil || got != c.want {
			t.Errorf("%s: got %q (%v), want %q", c.name, got, err, c.want)
		}
	}
	for name, src := range map[string]listenSource{
		"SMOKESTACK_LISTEN_IP":   {getenv: env(map[string]string{"SMOKESTACK_LISTEN_IP": "localhostt"})},
		"SMOKESTACK_LISTEN_PORT": {getenv: env(map[string]string{"SMOKESTACK_LISTEN_PORT": "99999"})},
		"-port":                  {flagPort: "http", getenv: none},
	} {
		if _, err := resolveListen(Config{}, src); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("an invalid %s must be rejected with a message naming it, got %v", name, err)
		}
	}
	if !strings.Contains(listenNote("[::]:8080"), "all interfaces") || !strings.Contains(listenNote("127.0.0.1:8080"), "this machine only") {
		t.Error("log note")
	}
}
