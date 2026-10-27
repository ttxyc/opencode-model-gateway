package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
)

type manifest struct {
	Version   string `json:"version"`
	Asset     string `json:"asset"`
	SHA256    string `json:"sha256"`
	Signature string `json:"signature"`
	Notes     string `json:"notes,omitempty"`
}

func main() {
	if len(os.Args) < 3 {
		usage()
	}
	switch os.Args[1] {
	case "gen":
		gen(os.Args[2])
	case "sign":
		if len(os.Args) < 7 {
			usage()
		}
		sign(os.Args[2], os.Args[3], os.Args[4], os.Args[5], os.Args[6])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "keytool gen <private-key-file>")
	fmt.Fprintln(os.Stderr, "keytool sign <private-key-file> <version> <sha256> <asset-url> <out-manifest>")
	os.Exit(2)
}

func gen(path string) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(priv.Seed())+"\n"), 0o600); err != nil {
		panic(err)
	}
	fmt.Println(base64.StdEncoding.EncodeToString(pub))
}

func sign(keyPath, version, sha, asset, out string) {
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		panic(err)
	}
	seed, err := base64.StdEncoding.DecodeString(string(trimSpace(raw)))
	if err != nil {
		panic(err)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	msg := []byte(version + "\n" + lower(sha) + "\n" + asset)
	sig := ed25519.Sign(priv, msg)
	m := manifest{Version: version, Asset: asset, SHA256: lower(sha), Signature: base64.StdEncoding.EncodeToString(sig)}
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(out, append(b, '\n'), 0o644); err != nil {
		panic(err)
	}
}

func trimSpace(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r' || b[len(b)-1] == ' ' || b[len(b)-1] == '\t') {
		b = b[:len(b)-1]
	}
	return b
}
func lower(s string) string {
	out := []byte(s)
	for i := range out {
		if out[i] >= 'A' && out[i] <= 'F' {
			out[i] += 'a' - 'A'
		}
	}
	return string(out)
}
