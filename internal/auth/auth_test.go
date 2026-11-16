package auth

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPasswordRoundTrip(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("unexpected hash format: %s", h)
	}
	if ok, err := VerifyPassword("correct horse battery", h); err != nil || !ok {
		t.Fatalf("right password: %v, %v", ok, err)
	}
	for _, wrong := range []string{"", "correct horse batter", "Correct horse battery", "correct horse battery "} {
		if ok, err := VerifyPassword(wrong, h); err != nil || ok {
			t.Errorf("password %q accepted: %v, %v", wrong, ok, err)
		}
	}
	h2, _ := HashPassword("correct horse battery")
	if h == h2 {
		t.Error("two hashes of one password are identical: the salt is not random")
	}
}

func TestVerifyPasswordRejectsMalformedHashes(t *testing.T) {
	good, _ := HashPassword("pw-12345678")
	parts := strings.Split(good, "$")
	bad := []string{
		"",
		"plaintext",
		"$argon2i$v=19$m=19456,t=2,p=1$" + parts[4] + "$" + parts[5],
		"$argon2id$v=18$m=19456,t=2,p=1$" + parts[4] + "$" + parts[5],
		"$argon2id$v=19$m=999999999,t=2,p=1$" + parts[4] + "$" + parts[5], // memory bomb
		"$argon2id$v=19$m=19456,t=999,p=1$" + parts[4] + "$" + parts[5],   // time bomb
		"$argon2id$v=19$m=19456,t=2,p=1$!!!$" + parts[5],
		"$argon2id$v=19$m=19456,t=2,p=1$" + parts[4] + "$",
	}
	for _, h := range bad {
		if ok, err := VerifyPassword("pw-12345678", h); ok || err == nil {
			t.Errorf("hash %q: ok=%v err=%v, want an error", h, ok, err)
		}
	}
}

func TestTokens(t *testing.T) {
	seen := map[string]bool{}
	for range 50 {
		tok, hash, err := NewToken()
		if err != nil {
			t.Fatal(err)
		}
		if len(tok) != 43 { // 32 bytes, unpadded base64url
			t.Fatalf("token length %d", len(tok))
		}
		if !bytes.Equal(hash, HashToken(tok)) || len(hash) != 32 {
			t.Fatal("stored hash does not match the token")
		}
		if strings.Contains(string(hash), tok) || seen[tok] {
			t.Fatal("token reused or stored in clear")
		}
		seen[tok] = true
	}
	if bytes.Equal(HashToken("a"), HashToken("b")) {
		t.Error("different tokens hash alike")
	}
}

func TestCookieAttributes(t *testing.T) {
	exp := time.Now().Add(time.Hour)
	for _, secure := range []bool{false, true} {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		if secure {
			req.Header.Set("X-Forwarded-Proto", "https")
		}
		rec := httptest.NewRecorder()
		SetCookie(rec, req, "tok", exp)
		c := rec.Result().Cookies()[0]
		wantName := map[bool]string{false: "shipyard_session", true: "__Host-shipyard_session"}[secure]
		if c.Name != wantName || c.Value != "tok" {
			t.Errorf("secure=%v: cookie %s=%s", secure, c.Name, c.Value)
		}
		if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Domain != "" || c.Path != "/" || c.Secure != secure {
			t.Errorf("secure=%v: bad attributes %+v", secure, c)
		}
		// Reading uses the same name rule.
		back := httptest.NewRequest(http.MethodGet, "/", nil)
		if secure {
			back.Header.Set("X-Forwarded-Proto", "https")
		}
		back.AddCookie(c)
		if got := TokenFromRequest(back); got != "tok" {
			t.Errorf("secure=%v: token from request = %q", secure, got)
		}
	}
	// A plain-HTTP cookie must not be accepted as the __Host- one, and vice versa.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.AddCookie(&http.Cookie{Name: "shipyard_session", Value: "plain"})
	if got := TokenFromRequest(req); got != "" {
		t.Errorf("https request accepted the plain cookie: %q", got)
	}
}
