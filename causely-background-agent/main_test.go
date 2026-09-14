package main

import (
	"net/http"
	"testing"
)

func TestValidTriggerAuth_NoSecretConfiguredAllowsAnyRequest(t *testing.T) {
	req, _ := http.NewRequest("POST", "/trigger", nil)
	if !validTriggerAuth(req, "") {
		t.Error("expected no configured secret to allow the request")
	}
}

func TestValidTriggerAuth_CorrectBearerTokenAllowed(t *testing.T) {
	req, _ := http.NewRequest("POST", "/trigger", nil)
	req.Header.Set("Authorization", "Bearer s3cr3t")
	if !validTriggerAuth(req, "s3cr3t") {
		t.Error("expected the correct bearer token to be allowed")
	}
}

func TestValidTriggerAuth_WrongOrMissingTokenRejected(t *testing.T) {
	cases := []struct {
		name   string
		header string
	}{
		{"wrong token", "Bearer nope"},
		{"missing header", ""},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest("POST", "/trigger", nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			if validTriggerAuth(req, "s3cr3t") {
				t.Errorf("expected %q to be rejected against secret \"s3cr3t\"", tt.header)
			}
		})
	}
}
