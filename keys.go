package main

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"
)

func loadHostKeys(dir string) ([]ssh.Signer, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create host key directory: %w", err)
	}
	names := []string{"ssh_host_ed25519_key", "ssh_host_ecdsa_key", "ssh_host_rsa_key"}
	keys := make([]ssh.Signer, 0, len(names))
	for i, name := range names {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			var private any
			switch i {
			case 0:
				_, private, err = ed25519.GenerateKey(rand.Reader)
			case 1:
				private, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			case 2:
				private, err = rsa.GenerateKey(rand.Reader, 3072)
			}
			if err != nil {
				return nil, err
			}
			der, e := x509.MarshalPKCS8PrivateKey(private)
			if e != nil {
				return nil, e
			}
			data = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
			// Link publishes a complete key without overwriting a concurrent creator.
			f, e := os.CreateTemp(dir, ".hostkey-")
			if e != nil {
				return nil, e
			}
			tmp := f.Name()
			if _, e = f.Write(data); e == nil {
				e = f.Sync()
			}
			closeErr := f.Close()
			if e == nil {
				e = closeErr
			}
			if e == nil {
				e = os.Link(tmp, path)
			}
			os.Remove(tmp)
			if e != nil && !errors.Is(e, os.ErrExist) {
				return nil, fmt.Errorf("save host key: %w", e)
			}
			data, err = os.ReadFile(path)
		}
		if err != nil {
			return nil, fmt.Errorf("read host key %s: %w", name, err)
		}
		key, err := ssh.ParsePrivateKey(data)
		if err != nil {
			return nil, fmt.Errorf("invalid host key %s: %w", name, err)
		}
		keys = append(keys, key)
	}
	return keys, nil
}
