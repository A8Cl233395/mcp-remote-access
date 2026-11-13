package relay

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"mcp-remoteaccess/internal/protocol"
)

const (
	oauthAccessTokenTTL  = 24 * time.Hour
	oauthRefreshTokenTTL = 30 * 24 * time.Hour
	oauthAuthCodeTTL     = 5 * time.Minute
)

type oauthTokenClaims struct {
	Type                string `json:"typ"`
	ClientID            string `json:"cid"`
	Scope               string `json:"scp,omitempty"`
	RedirectURI         string `json:"red,omitempty"`
	CodeChallenge       string `json:"cc,omitempty"`
	CodeChallengeMethod string `json:"ccm,omitempty"`
	Exp                 int64  `json:"exp"`
	JTI                 string `json:"jti"`
}

func randomHex(nBytes int) string {
	b := make([]byte, nBytes)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) matchDevKey(candidate string) bool {
	return s.cfg.DevKey != "" && candidate != "" && subtle.ConstantTimeCompare([]byte(candidate), []byte(s.cfg.DevKey)) == 1
}

func (s *Server) oauthSigningKey() []byte {
	h := sha256.New()
	h.Write([]byte(s.cfg.DevKey))
	h.Write([]byte("|mcp-oauth-token-v1"))
	return h.Sum(nil)
}

func (s *Server) signOAuthToken(claims oauthTokenClaims) (string, error) {
	if s.cfg.DevKey == "" {
		return "", fmt.Errorf("dev key is not configured")
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	encPayload := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, s.oauthSigningKey())
	mac.Write([]byte(encPayload))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return encPayload + "." + sig, nil
}

func (s *Server) verifyOAuthToken(token, expectedType string) (*oauthTokenClaims, bool) {
	if s.cfg.DevKey == "" || token == "" {
		return nil, false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return nil, false
	}
	sigBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, false
	}
	mac := hmac.New(sha256.New, s.oauthSigningKey())
	mac.Write([]byte(parts[0]))
	if !hmac.Equal(sigBytes, mac.Sum(nil)) {
		return nil, false
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, false
	}
	var claims oauthTokenClaims
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return nil, false
	}
	if claims.Type != expectedType || time.Now().Unix() >= claims.Exp {
		return nil, false
	}
	return &claims, true
}

func (s *Server) checkMCPAuth(r *http.Request) bool {
	if s.cfg.DevKey == "" {
		return false
	}
	if s.matchDevKey(r.Header.Get(protocol.HeaderDevKey)) {
		return true
	}
	if s.matchDevKey(r.URL.Query().Get("key")) {
		return true
	}
	authHdr := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(authHdr) > 7 && strings.EqualFold(authHdr[:7], "Bearer ") {
		tok := strings.TrimSpace(authHdr[7:])
		if s.matchDevKey(tok) {
			return true
		}
		if _, ok := s.verifyOAuthToken(tok, "access"); ok {
			return true
		}
	}
	return false
}

func (s *Server) writeMCPUnauthorized(w http.ResponseWriter, r *http.Request) {
	origin := requestOrigin(r)
	metaPath := "/.well-known/oauth-protected-resource/mcp"
	if r.URL.Path == "/sse" {
		metaPath = "/.well-known/oauth-protected-resource/sse"
	}
	w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="mcp-remoteaccess", resource_metadata="%s%s"`, origin, metaPath))
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

func requestOrigin(r *http.Request) string {
	proto := "http"
	if r.TLS != nil {
		proto = "https"
	}
	if xfp := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]); xfp != "" {
		proto = xfp
	}
	host := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Host"), ",")[0])
	if host == "" {
		host = r.Host
	}
	if host == "" {
		host = "localhost"
	}
	return proto + "://" + host
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeOAuthError(w http.ResponseWriter, status int, code, desc string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, status, map[string]string{
		"error":             code,
		"error_description": desc,
	})
}

func (s *Server) handleOAuthProtectedResource(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	origin := requestOrigin(r)
	resourcePath := "/mcp"
	if strings.HasSuffix(r.URL.Path, "/sse") {
		resourcePath = "/sse"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                 origin + resourcePath,
		"authorization_servers":    []string{origin},
		"bearer_methods_supported": []string{"header"},
	})
}

func (s *Server) handleOAuthAuthorizationServer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	origin := requestOrigin(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                origin,
		"authorization_endpoint":                origin + "/oauth/authorize",
		"token_endpoint":                        origin + "/oauth/token",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "client_credentials", "refresh_token"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_post", "client_secret_basic"},
		"code_challenge_methods_supported":      []string{"S256", "plain"},
	})
}

// handleOAuthAuthorize immediately issues a stateless short-lived signed code and redirects back.
// Security is enforced at /oauth/token, which strictly requires client_secret == DevKey.
func (s *Server) handleOAuthAuthorize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	_ = r.ParseForm()
	responseType := r.FormValue("response_type")
	if responseType != "code" {
		http.Error(w, "unsupported response_type: expected code", http.StatusBadRequest)
		return
	}
	redirectURI := strings.TrimSpace(r.FormValue("redirect_uri"))
	redirURL, err := url.Parse(redirectURI)
	if err != nil || redirURL.Scheme == "" {
		http.Error(w, "invalid redirect_uri", http.StatusBadRequest)
		return
	}
	scheme := strings.ToLower(redirURL.Scheme)
	if scheme == "javascript" || scheme == "data" || scheme == "vbscript" {
		http.Error(w, "disallowed redirect_uri scheme", http.StatusBadRequest)
		return
	}

	clientID := strings.TrimSpace(r.FormValue("client_id"))
	if clientID == "" {
		clientID = "mcp-client"
	}
	code, err := s.signOAuthToken(oauthTokenClaims{
		Type:                "code",
		ClientID:            clientID,
		Scope:               strings.TrimSpace(r.FormValue("scope")),
		RedirectURI:         redirectURI,
		CodeChallenge:       strings.TrimSpace(r.FormValue("code_challenge")),
		CodeChallengeMethod: strings.TrimSpace(r.FormValue("code_challenge_method")),
		Exp:                 time.Now().Add(oauthAuthCodeTTL).Unix(),
		JTI:                 randomHex(8),
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	q := redirURL.Query()
	q.Set("code", code)
	if st := r.FormValue("state"); st != "" {
		q.Set("state", st)
	}
	redirURL.RawQuery = q.Encode()
	http.Redirect(w, r, redirURL.String(), http.StatusFound)
}

func parseOAuthClientBasicAuth(r *http.Request) (clientID, clientSecret string, ok bool) {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(auth) <= 6 || !strings.EqualFold(auth[:6], "Basic ") {
		return "", "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(auth[6:]))
	if err != nil {
		return "", "", false
	}
	idx := strings.IndexByte(string(decoded), ':')
	if idx < 0 {
		return "", "", false
	}
	rawID := string(decoded[:idx])
	rawSecret := string(decoded[idx+1:])
	if unescapedID, err := url.QueryUnescape(rawID); err == nil {
		rawID = unescapedID
	}
	return rawID, rawSecret, true
}

func (s *Server) matchClientSecret(rawSecret string) bool {
	if s.matchDevKey(rawSecret) {
		return true
	}
	if unescaped, err := url.QueryUnescape(rawSecret); err == nil && unescaped != rawSecret {
		return s.matchDevKey(unescaped)
	}
	return false
}

func verifyPKCE(challenge, method, verifier string) bool {
	if challenge == "" {
		return true
	}
	if verifier == "" {
		return false
	}
	switch strings.ToUpper(method) {
	case "", "PLAIN":
		return subtle.ConstantTimeCompare([]byte(verifier), []byte(challenge)) == 1
	case "S256":
		sum := sha256.Sum256([]byte(verifier))
		computed := base64.RawURLEncoding.EncodeToString(sum[:])
		return subtle.ConstantTimeCompare([]byte(computed), []byte(challenge)) == 1
	default:
		return false
	}
}

func (s *Server) handleOAuthToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.DevKey == "" {
		writeOAuthError(w, http.StatusServiceUnavailable, "server_error", "dev key is not configured")
		return
	}

	params := url.Values{}
	ct := strings.ToLower(r.Header.Get("Content-Type"))
	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, 256*1024))
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "failed to read token request")
		return
	}
	if strings.Contains(ct, "application/json") {
		var jm map[string]any
		if err := json.Unmarshal(bodyBytes, &jm); err != nil {
			writeOAuthError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
			return
		}
		for k, v := range jm {
			if str, ok := v.(string); ok {
				params.Set(k, str)
			}
		}
	} else {
		parsed, err := url.ParseQuery(string(bodyBytes))
		if err != nil {
			writeOAuthError(w, http.StatusBadRequest, "invalid_request", "invalid form body")
			return
		}
		params = parsed
	}

	clientID := strings.TrimSpace(params.Get("client_id"))
	clientSecret := params.Get("client_secret")
	if bID, bSecret, hasBasic := parseOAuthClientBasicAuth(r); hasBasic {
		if clientID == "" {
			clientID = bID
		}
		if clientSecret == "" {
			clientSecret = bSecret
		}
	}

	grantType := strings.TrimSpace(params.Get("grant_type"))
	switch grantType {
	case "client_credentials":
		if !s.matchClientSecret(clientSecret) {
			writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "invalid client_secret")
			return
		}
		if clientID == "" {
			clientID = "mcp-client"
		}
		s.issueOAuthTokenPair(w, clientID, strings.TrimSpace(params.Get("scope")))

	case "authorization_code":
		if !s.matchClientSecret(clientSecret) {
			writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "invalid client_secret")
			return
		}
		code := strings.TrimSpace(params.Get("code"))
		claims, ok := s.verifyOAuthToken(code, "code")
		if !ok {
			writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "invalid or expired authorization code")
			return
		}
		if clientID != "" && claims.ClientID != "" && clientID != claims.ClientID {
			writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "client_id mismatch")
			return
		}
		redirectURI := strings.TrimSpace(params.Get("redirect_uri"))
		if claims.RedirectURI != "" && redirectURI != "" && redirectURI != claims.RedirectURI {
			writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri mismatch")
			return
		}
		if !verifyPKCE(claims.CodeChallenge, claims.CodeChallengeMethod, params.Get("code_verifier")) {
			writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
			return
		}
		cid := claims.ClientID
		if cid == "" {
			cid = clientID
		}
		if cid == "" {
			cid = "mcp-client"
		}
		s.issueOAuthTokenPair(w, cid, claims.Scope)

	case "refresh_token":
		rt := strings.TrimSpace(params.Get("refresh_token"))
		claims, ok := s.verifyOAuthToken(rt, "refresh")
		if !ok {
			writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "invalid or expired refresh_token")
			return
		}
		if clientSecret != "" && !s.matchClientSecret(clientSecret) {
			writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "invalid client_secret")
			return
		}
		s.issueOAuthTokenPair(w, claims.ClientID, claims.Scope)

	default:
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "supported grant_types: authorization_code, client_credentials, refresh_token")
	}
}

func (s *Server) issueOAuthTokenPair(w http.ResponseWriter, clientID, scope string) {
	now := time.Now()
	accessToken, err := s.signOAuthToken(oauthTokenClaims{
		Type:     "access",
		ClientID: clientID,
		Scope:    scope,
		Exp:      now.Add(oauthAccessTokenTTL).Unix(),
		JTI:      randomHex(8),
	})
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	refreshToken, err := s.signOAuthToken(oauthTokenClaims{
		Type:     "refresh",
		ClientID: clientID,
		Scope:    scope,
		Exp:      now.Add(oauthRefreshTokenTTL).Unix(),
		JTI:      randomHex(8),
	})
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  accessToken,
		"token_type":    "Bearer",
		"expires_in":    int(oauthAccessTokenTTL.Seconds()),
		"refresh_token": refreshToken,
	})
}
