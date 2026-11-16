package cli

import (
	"net/http"
	"testing"
	"time"
)

func TestExtendClientTimeoutForLongOperation(t *testing.T) {
	client := &Client{HTTPClient: &http.Client{Timeout: 10 * time.Minute}}

	extendClientTimeoutForLongOperation(client, 2*time.Hour)

	if client.HTTPClient.Timeout != 2*time.Hour+2*time.Minute {
		t.Fatalf("timeout = %s, want 2h2m", client.HTTPClient.Timeout)
	}
}

func TestExtendClientTimeoutKeepsLongerExistingTimeout(t *testing.T) {
	client := &Client{HTTPClient: &http.Client{Timeout: 30 * time.Minute}}

	extendClientTimeoutForLongOperation(client, 5*time.Minute)

	if client.HTTPClient.Timeout != 30*time.Minute {
		t.Fatalf("timeout = %s, want existing 30m", client.HTTPClient.Timeout)
	}
}
