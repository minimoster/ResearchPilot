package api

import (
	"net"
	"testing"
)

func TestBlockedRemoteIPRejectsSpecialUseNetworks(t *testing.T) {
	for _, address := range []string{"0.0.0.1", "100.64.0.1", "100.127.255.254", "198.18.0.1", "240.0.0.1", "255.255.255.255", "::ffff:100.64.0.1"} {
		if !blockedRemoteIP(net.ParseIP(address)) {
			t.Errorf("accepted special-use remote IP %s", address)
		}
	}
	for _, address := range []string{"1.1.1.1", "100.63.255.255", "100.128.0.1", "2606:4700:4700::1111"} {
		if blockedRemoteIP(net.ParseIP(address)) {
			t.Errorf("rejected public remote IP %s", address)
		}
	}
}
