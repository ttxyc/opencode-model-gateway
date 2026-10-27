package updater

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"testing"
)

func TestVersionGreater(t *testing.T) {
	if !versionGreater("1.2.0", "1.1.9") {
		t.Fatal("expected newer")
	}
	if versionGreater("1.1.0", "1.1.0") {
		t.Fatal("expected equal")
	}
	if versionGreater("0.9.9", "1.0.0") {
		t.Fatal("expected older")
	}
}

func TestSignature(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	m := Manifest{Version: "1.2.3", Asset: "https://example/gateway.exe", SHA256: "abcdef"}
	msg := []byte(m.Version + "\n" + m.SHA256 + "\n" + m.Asset)
	m.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, msg))
	if err := verifyManifest(m, base64.StdEncoding.EncodeToString(pub)); err != nil {
		t.Fatal(err)
	}
	m.SHA256 = "ffffff"
	if err := verifyManifest(m, base64.StdEncoding.EncodeToString(pub)); err == nil {
		t.Fatal("expected signature failure")
	}
}
