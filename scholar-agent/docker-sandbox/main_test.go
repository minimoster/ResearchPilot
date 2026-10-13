package main

import "testing"

func TestSandboxListenAddrRequiresTokenForRemoteAccess(t *testing.T) {
	for _, token := range []string{"", "   "} {
		t.Setenv("SANDBOX_API_TOKEN", token)
		if got := sandboxListenAddr(); got != "127.0.0.1:8082" {
			t.Fatalf("without token listen=%q", got)
		}
	}
	t.Setenv("SANDBOX_API_TOKEN", "configured-secret")
	if got := sandboxListenAddr(); got != ":8082" {
		t.Fatalf("with token listen=%q", got)
	}
}
