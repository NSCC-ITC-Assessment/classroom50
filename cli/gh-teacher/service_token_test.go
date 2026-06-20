package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/crypto/nacl/box"

	"github.com/foundation50/gh-teacher/internal/githubtest"
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
			client := githubtest.NewTestClient(t, server)

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
			client := githubtest.NewTestClient(t, server)

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

// TestProvisionServiceSecret_PutStatus pins the PUT status handling: the
// Actions-secret upload must succeed on 201 (created) and 204 (updated),
// and the new assertion must reject any other 2xx (e.g. a 200 that means
// the write didn't land as a create/update) rather than reporting a
// stored token. The handler serves a valid NaCl public key on the GET so
// sealbox encryption succeeds and the flow reaches the PUT.
func TestProvisionServiceSecret_PutStatus(t *testing.T) {
	pub, _, err := box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	keyB64 := base64.StdEncoding.EncodeToString(pub[:])

	cases := []struct {
		name      string
		putStatus int
		wantErr   bool
	}{
		{"created", http.StatusCreated, false},
		{"updated", http.StatusNoContent, false},
		{"unexpected 2xx rejected", http.StatusOK, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/repos/o/classroom50/actions/secrets/public-key", func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]string{"key_id": "kid-1", "key": keyB64})
			})
			mux.HandleFunc("/repos/o/classroom50/actions/secrets/CLASSROOM50_SERVICE_TOKEN", func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPut {
					t.Errorf("secret upload method = %s, want PUT", r.Method)
				}
				w.WriteHeader(tc.putStatus)
			})
			server := httptest.NewServer(mux)
			t.Cleanup(server.Close)
			client := githubtest.NewTestClient(t, server)

			err := provisionServiceSecret(client, io.Discard, "o", "classroom50", []byte("ghp_test"), "stored")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("status %d should be rejected by the 201/204 assertion", tc.putStatus)
				}
				if !strings.Contains(err.Error(), "unexpected status") {
					t.Errorf("error %q should mention 'unexpected status'", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("status %d should succeed, got %v", tc.putStatus, err)
			}
		})
	}
}
