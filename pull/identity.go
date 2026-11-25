package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/smallstep/pkcs7"
)

// AWS's certificates for the RSA-2048 signature of the instance identity document, one per region, from
// https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/regions-certs.html.
var (
	//go:embed certs/us-east-1-rsa2048.pem
	certUSEast1 []byte
	//go:embed certs/us-east-2-rsa2048.pem
	certUSEast2 []byte
)

// An InstanceDocument is what EC2 signs about an instance. The listener trusts the instance id in it, never one a
// request merely names.
type InstanceDocument struct {
	InstanceID   string    `json:"instanceId"`
	AccountID    string    `json:"accountId"`
	Region       string    `json:"region"`
	InstanceType string    `json:"instanceType"`
	PendingTime  time.Time `json:"pendingTime"`
}

type Verifier struct {
	account string
	certs   map[string]*x509.Certificate
}

func NewVerifier(account string) (*Verifier, error) {
	v := &Verifier{account: account, certs: map[string]*x509.Certificate{}}
	for region, raw := range map[string][]byte{"us-east-1": certUSEast1, "us-east-2": certUSEast2} {
		block, _ := pem.Decode(raw)
		if block == nil {
			return nil, fmt.Errorf("%s: no certificate", region)
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", region, err)
		}
		v.certs[region] = c
	}
	return v, nil
}

// Verify checks the instance metadata service's signature of the identity document
// (/latest/dynamic/instance-identity/rsa2048: base64 PKCS7 with the document attached) against AWS's certificate for
// the region the document names, and returns the document if the signature holds and the instance is in this account.
func (v *Verifier) Verify(body []byte) (InstanceDocument, error) {
	var doc InstanceDocument
	der, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(string(body)), ""))
	if err != nil {
		return doc, fmt.Errorf("signature: %w", err)
	}
	p7, err := pkcs7.Parse(der)
	if err != nil {
		return doc, fmt.Errorf("signature: %w", err)
	}
	if err := json.Unmarshal(p7.Content, &doc); err != nil {
		return doc, fmt.Errorf("document: %w", err)
	}
	cert := v.certs[doc.Region]
	if cert == nil {
		return doc, fmt.Errorf("document of region %q", doc.Region)
	}
	p7.Certificates = []*x509.Certificate{cert}
	if err := p7.Verify(); err != nil {
		return doc, fmt.Errorf("signature: %w", err)
	}
	if doc.AccountID != v.account {
		return doc, fmt.Errorf("instance of account %s", doc.AccountID)
	}
	if !strings.HasPrefix(doc.InstanceID, "i-") {
		return doc, errors.New("document names no instance")
	}
	return doc, nil
}

// Identity is the listener's TLS key. Machines pin its public key (curl --pinnedpubkey), so it must outlive the
// listener's instance: it is kept in an SSM SecureString, made by the first listener that finds none.
type Identity struct {
	Cert tls.Certificate
	Pin  string // sha256//<base64 of the SubjectPublicKeyInfo's SHA-256>
}

func LoadIdentity(ctx context.Context, client *ssm.Client, name string) (Identity, error) {
	for try := 0; try < 2; try++ {
		out, err := client.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String(name), WithDecryption: aws.Bool(true)})
		var missing *ssmtypes.ParameterNotFound
		switch {
		case err == nil:
			var kp struct{ Cert, Key string }
			if err := json.Unmarshal([]byte(aws.ToString(out.Parameter.Value)), &kp); err != nil {
				return Identity{}, fmt.Errorf("%s: %w", name, err)
			}
			return identityOf([]byte(kp.Cert), []byte(kp.Key))
		case !errors.As(err, &missing):
			return Identity{}, err
		}
		certPEM, keyPEM, err := newIdentity()
		if err != nil {
			return Identity{}, err
		}
		value, _ := json.Marshal(struct{ Cert, Key string }{string(certPEM), string(keyPEM)})
		_, err = client.PutParameter(ctx, &ssm.PutParameterInput{Name: aws.String(name), Type: ssmtypes.ParameterTypeSecureString,
			Value: aws.String(string(value)), Overwrite: aws.Bool(false),
			Tags: []ssmtypes.Tag{{Key: aws.String("purpose"), Value: aws.String("superci-pull")}}})
		var exists *ssmtypes.ParameterAlreadyExists
		if err != nil && !errors.As(err, &exists) {
			return Identity{}, err
		}
	}
	return Identity{}, errors.New("tls identity: not found after making it")
}

func newIdentity() (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "superci-pull"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(10, 0, 0), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), nil
}

func identityOf(certPEM, keyPEM []byte) (Identity, error) {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return Identity{}, err
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return Identity{}, err
	}
	sum := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	return Identity{Cert: cert, Pin: "sha256//" + base64.StdEncoding.EncodeToString(sum[:])}, nil
}
