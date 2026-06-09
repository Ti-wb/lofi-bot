package obs

import (
	"strings"
	"testing"
)

func TestBuildIdentifyAllowsEmptyPasswordWhenAuthenticationDisabled(t *testing.T) {
	identify, err := buildIdentify(helloData{RPCVersion: 1}, "")
	if err != nil {
		t.Fatalf("build identify without OBS password: %v", err)
	}
	if identify.RPCVersion != 1 {
		t.Fatalf("rpc version = %d, want 1", identify.RPCVersion)
	}
	if identify.Authentication != "" {
		t.Fatalf("expected empty authentication when OBS auth is disabled, got %q", identify.Authentication)
	}
}

func TestBuildIdentifyFailsWhenAuthenticationRequiresEmptyPassword(t *testing.T) {
	_, err := buildIdentify(helloData{
		RPCVersion: 1,
		Authentication: &authenticationData{
			Challenge: "challenge",
			Salt:      "salt",
		},
	}, "")
	if err == nil {
		t.Fatal("expected identify build to fail when OBS requires auth and password is empty")
	}
	if !strings.Contains(err.Error(), "OBS authentication required but password is empty") {
		t.Fatalf("unexpected error: %v", err)
	}
}
