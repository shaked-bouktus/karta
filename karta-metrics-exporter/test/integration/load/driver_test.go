// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func TestWriteKubeconfigPrivateAndUsable(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "load-test-placeholder"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	key := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	original := &rest.Config{Host: "https://example.test", TLSClientConfig: rest.TLSClientConfig{CAData: cert, CertData: cert, KeyData: key}}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config")
	if err := writeKubeconfig(path, original); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("credential file permissions: %v", info.Mode().Perm())
	}
	restored, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		t.Fatal("written kubeconfig cannot be loaded")
	}
	if restored.Host != original.Host || !bytes.Equal(restored.CAData, cert) || !bytes.Equal(restored.CertData, cert) || !bytes.Equal(restored.KeyData, key) {
		t.Fatal("kubeconfig did not preserve connection credentials")
	}
	if restored.Insecure {
		t.Fatal("kubeconfig weakened TLS verification")
	}
	tlsConfig, err := rest.TLSConfigFor(restored)
	if err != nil || tlsConfig == nil || tlsConfig.RootCAs == nil || tlsConfig.GetClientCertificate == nil {
		t.Fatal("written kubeconfig cannot construct authenticated TLS transport")
	}
	clientCertificate, err := tlsConfig.GetClientCertificate(&tls.CertificateRequestInfo{})
	if err != nil || clientCertificate == nil || len(clientCertificate.Certificate) == 0 || clientCertificate.PrivateKey == nil {
		t.Fatal("written kubeconfig cannot supply a client certificate and key")
	}
}

func TestWriteJSONDoesNotOverwriteAndUsesPrivateMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	value := map[string]any{"outcome": "passed", "count": 2}
	if err := writeJSON(path, value); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(original, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["outcome"] != "passed" || decoded["count"] != float64(2) {
		t.Fatalf("unexpected report: %v", decoded)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("report permissions: %v", info.Mode().Perm())
	}
	if err := writeJSON(path, map[string]string{"outcome": "replaced"}); !errors.Is(err, os.ErrExist) {
		t.Fatalf("overwrite error: %v", err)
	}
	current, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(current, original) {
		t.Fatal("existing report changed")
	}
}

func TestWriteJSONDoesNotFollowExistingSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	original := []byte("preserve this file")
	if err := os.WriteFile(target, original, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "report.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(link, map[string]string{"new": "value"}); !errors.Is(err, os.ErrExist) {
		t.Fatalf("symlink overwrite error: %v", err)
	}
	current, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(current, original) {
		t.Fatal("symlink target changed")
	}
}

func TestWriteJSONReportsEncodingFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invalid.json")
	if err := writeJSON(path, make(chan int)); err == nil {
		t.Fatal("unsupported JSON value accepted")
	}
}
