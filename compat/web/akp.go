package web

// akp.go: the key encoding the post-quantum algorithms share.
//
// ML-KEM (FIPS 203) and ML-DSA (FIPS 204) are both "algorithm key pair" keys,
// and their DER is the same shape: an SPKI is the algorithm OID plus the public
// key in a BIT STRING, and a PKCS#8 is the OID plus the generation seed in a
// [0] choice inside the private-key OCTET STRING. crypto/x509 marshals neither
// yet, so both are hand-encoded here — they are small and fully specified, and
// the OID is the only thing that distinguishes one algorithm's key from
// another's, which is what lets an import refuse to reinterpret a key as the
// wrong parameter set.

import (
	"encoding/asn1"
	"fmt"
)

type pkixAlgorithm struct {
	Algorithm asn1.ObjectIdentifier
}

type spkiDoc struct {
	Algorithm pkixAlgorithm
	PublicKey asn1.BitString
}

type pkcs8Doc struct {
	Version    int
	Algorithm  pkixAlgorithm
	PrivateKey []byte // an OCTET STRING holding the [0] seed choice
}

// akpSPKI encodes a public key as SubjectPublicKeyInfo.
func akpSPKI(oid asn1.ObjectIdentifier, pub []byte) ([]byte, error) {
	return asn1.Marshal(spkiDoc{
		Algorithm: pkixAlgorithm{oid},
		PublicKey: asn1.BitString{Bytes: pub, BitLength: len(pub) * 8},
	})
}

// The tag the private-key OCTET STRING wraps its payload in. The two families
// differ only here: an AKP seed is the [0] alternative of a CHOICE, while an
// RFC 8410 curve key is a CurvePrivateKey, which is a nested OCTET STRING.
const (
	akpSeedTag   = 0x80
	curveSeedTag = 0x04
)

// akpPKCS8 encodes a seed as a PrivateKeyInfo.
func akpPKCS8(oid asn1.ObjectIdentifier, seed []byte) ([]byte, error) {
	return derPKCS8(oid, akpSeedTag, seed)
}

func derPKCS8(oid asn1.ObjectIdentifier, tag byte, seed []byte) ([]byte, error) {
	inner := append([]byte{tag, byte(len(seed))}, seed...)
	return asn1.Marshal(pkcs8Doc{Version: 0, Algorithm: pkixAlgorithm{oid}, PrivateKey: inner})
}

// akpParseSPKI reads back what akpSPKI wrote, reporting the OID so the caller
// can decide whether it names the parameter set that was asked for.
func akpParseSPKI(der []byte) (asn1.ObjectIdentifier, []byte, error) {
	var doc spkiDoc
	if _, err := asn1.Unmarshal(der, &doc); err != nil {
		return nil, nil, err
	}
	return doc.Algorithm.Algorithm, doc.PublicKey.Bytes, nil
}

// akpParsePKCS8 reads back what akpPKCS8 wrote. Only the seed alternative is
// accepted: the expanded-key and both-halves alternatives are legal DER but
// carry material this layer has no way to import.
func akpParsePKCS8(der []byte) (asn1.ObjectIdentifier, []byte, error) {
	return derParsePKCS8(der, akpSeedTag)
}

func derParsePKCS8(der []byte, tag byte) (asn1.ObjectIdentifier, []byte, error) {
	var doc pkcs8Doc
	if _, err := asn1.Unmarshal(der, &doc); err != nil {
		return nil, nil, err
	}
	b := doc.PrivateKey
	if len(b) < 2 || b[0] != tag || int(b[1]) != len(b)-2 {
		return nil, nil, fmt.Errorf("private key is not a seed")
	}
	return doc.Algorithm.Algorithm, b[2:], nil
}
