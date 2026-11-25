package main

import (
	"bytes"
	"encoding/base64"
	"os"
	"strings"
	"testing"
)

// testdata/identity.rsa2048 is the RSA-2048 signature of a build instance's identity document, as its metadata
// service gave it (the instance is terminated).
func TestAnIdentityDocumentSignedByAWSIsTrusted(t *testing.T) {
	v, err := NewVerifier("243891825325")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("testdata/identity.rsa2048")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := v.Verify(raw)
	if err != nil {
		t.Fatalf("a document EC2 signed: %v", err)
	}
	if !strings.HasPrefix(doc.InstanceID, "i-") || doc.Region != "us-east-1" || doc.PendingTime.IsZero() {
		t.Fatalf("document %+v", doc)
	}
	other, _ := NewVerifier("060795898020")
	if _, err := other.Verify(raw); err == nil {
		t.Fatal("a document of another account was trusted")
	}
}

func TestAChangedDocumentIsRefused(t *testing.T) {
	v, _ := NewVerifier("243891825325")
	raw, _ := os.ReadFile("testdata/identity.rsa2048")
	der, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(string(raw)), ""))
	if err != nil {
		t.Fatal(err)
	}
	i := bytes.Index(der, []byte(`"instanceId" : "i-`))
	if i < 0 {
		t.Fatal("no instance id in the signed content")
	}
	forged := bytes.Clone(der)
	forged[i+len(`"instanceId" : "i-`)] ^= 1
	if _, err := v.Verify([]byte(base64.StdEncoding.EncodeToString(forged))); err == nil {
		t.Fatal("a document with another instance id under the same signature was trusted")
	}
	if _, err := v.Verify([]byte("not a signature")); err == nil {
		t.Fatal("garbage was trusted")
	}
}
