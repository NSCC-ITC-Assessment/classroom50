package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestServiceSecretExists(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   bool
		errNil bool
	}{
		{"exists", http.StatusOK, true, true},
		{"absent", http.StatusNotFound, false, true},
		{"other error", http.StatusInternalServerError, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.status == http.StatusOK {
					_, _ = w.Write([]byte(`{"name":"CLASSROOM50_SERVICE_TOKEN"}`))
					return
				}
				w.WriteHeader(tc.status)
			}))
			t.Cleanup(server.Close)
			client := newTestRESTClient(t, server)

			got, err := serviceSecretExists(client, "o", "classroom50")
			if got != tc.want {
				t.Errorf("exists = %v, want %v", got, tc.want)
			}
			if tc.errNil && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if !tc.errNil && err == nil {
				t.Errorf("expected an error for status %d", tc.status)
			}
		})
	}
}

func TestValidateServiceToken(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		wantErr   bool
		errSubstr string
	}{
		{"valid", http.StatusOK, false, ""},
		{"revoked", http.StatusUnauthorized, true, "invalid, expired, or revoked"},
		{"no access", http.StatusNotFound, true, "can't read"},
		{"forbidden", http.StatusForbidden, true, "can't read"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.Contains(r.URL.Path, "/contents/") {
					t.Errorf("validate should read repo contents, got path %s", r.URL.Path)
				}
				if tc.status == http.StatusOK {
					_, _ = w.Write([]byte(`[]`))
					return
				}
				w.WriteHeader(tc.status)
			}))
			t.Cleanup(server.Close)
			client := newTestRESTClient(t, server)

			err := validateServiceTokenWithClient(client, "cs50")
			if tc.wantErr && err == nil {
				t.Fatalf("expected an error for status %d", tc.status)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.errSubstr != "" && !strings.Contains(err.Error(), tc.errSubstr) {
				t.Errorf("error %q should contain %q", err.Error(), tc.errSubstr)
			}
			// The no-access message must carry the actionable fix.
			if tc.status == http.StatusNotFound {
				if !strings.Contains(err.Error(), "Resource owner") || !strings.Contains(err.Error(), "Contents") {
					t.Errorf("no-access error should explain the resource-owner + Contents fix: %q", err.Error())
				}
			}
		})
	}
}
