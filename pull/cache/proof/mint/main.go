// mint stands in for GitHub's token service in the proof: `mint init DIR` makes a key and its JWKS; `mint token DIR
// REPO SCOPE...` signs a job's token ("refs/heads/main:rw", "refs/heads/main:r") and prints it.
package main

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"superci-pull-cache/internal/cache"
	"superci-pull-cache/internal/cachetest"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: mint init DIR | mint token DIR REPO SCOPE...")
		os.Exit(2)
	}
	dir := os.Args[2]
	switch os.Args[1] {
	case "init":
		s := cachetest.NewSigner("proof")
		der := x509.MarshalPKCS1PrivateKey(s.Key)
		must(os.WriteFile(filepath.Join(dir, "key.pem"), pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der}), 0o600))
		must(os.WriteFile(filepath.Join(dir, "jwks"), s.JWKS(), 0o644))
	case "token":
		raw, err := os.ReadFile(filepath.Join(dir, "key.pem"))
		must(err)
		blk, _ := pem.Decode(raw)
		k, err := x509.ParsePKCS1PrivateKey(blk.Bytes)
		must(err)
		s := &cachetest.Signer{Key: k, Kid: "proof"}
		fmt.Println(s.Mint(cache.GitHubIssuer, os.Args[3], cachetest.AC(os.Args[4:]...), time.Now().Add(6*time.Hour)))
	}
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
