package root

import "testing"

func TestIsLoopbackListenAddr(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:8082", true},
		{"[::1]:8082", true},
		{"localhost:8082", true},
		{"unix:///tmp/agent.sock", true},
		{"unix://", true},
		{":8082", false},
		{"0.0.0.0:8082", false},
		{"[::]:8082", false},
		{"192.168.1.1:8082", false},
	} {
		t.Run(tc.addr, func(t *testing.T) {
			if got := isLoopbackListenAddr(tc.addr); got != tc.want {
				t.Errorf("isLoopbackListenAddr(%q) = %v, want %v", tc.addr, got, tc.want)
			}
		})
	}
}
