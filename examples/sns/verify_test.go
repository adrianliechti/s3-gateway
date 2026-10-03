package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestVerifierRejectsTampering(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/SimpleNotificationService.pem" {
			t.Errorf("unexpected certificate URL %s", r.URL.Path)
		}
		_ = pem.Encode(w, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	}))
	defer server.Close()
	verify, err := LoadVerifier(context.Background(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	n := Notification{Type: "Notification", MessageID: "example-id", TopicARN: "arn:aws:sns:us-east-1:000000000000:example", Message: `{"Records":[]}`, Timestamp: time.Now().UTC().Format(time.RFC3339), SignatureVersion: "2"}
	canonical := "Message\n" + n.Message + "\nMessageId\n" + n.MessageID + "\nTimestamp\n" + n.Timestamp + "\nTopicArn\n" + n.TopicARN + "\nType\nNotification\n"
	sum := sha256.Sum256([]byte(canonical))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	n.Signature = base64.StdEncoding.EncodeToString(signature)
	if err = verify(n); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Notification){func(n *Notification) { n.Message = "changed" }, func(n *Notification) { n.TopicARN += "-other" }, func(n *Notification) { n.SignatureVersion = "1" }, func(n *Notification) { n.Timestamp = "invalid" }} {
		changed := n
		mutate(&changed)
		if verify(changed) == nil {
			t.Fatal("accepted tampered notification")
		}
	}
}
