// Development-only issuer. Anyone on this loopback endpoint can request a JWT.
package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func main() {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		log.Fatal(err)
	}
	const issuer = "http://127.0.0.1:9003"
	const kid = "local-abac-example"
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/jwks.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{"kty": "RSA", "use": "sig", "alg": "RS256", "kid": kid, "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())}}})
	})
	mux.HandleFunc("GET /token", func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{"iss": issuer, "aud": "s3-gateway", "sub": "example-user", "iat": now.Unix(), "exp": now.Add(30 * time.Minute).Unix(), "realm_access": map[string]any{"roles": []string{"storage"}}, "team": "analytics"})
		token.Header["kid"] = kid
		raw, err := token.SignedString(key)
		if err != nil {
			http.Error(w, "signing failed", 500)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprintln(w, raw)
	})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	server := &http.Server{Addr: "127.0.0.1:9003", Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	log.Printf("Demo issuer listening at %s; restart the S3 gateway too if this signing key changes", issuer)
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
