// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package v2

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	goerrors "errors"
	"io"
	"math/big"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp/ecdh"
	"github.com/ProtonMail/go-crypto/openpgp/ecdsa"
	"github.com/ProtonMail/go-crypto/openpgp/ed25519"
	"github.com/ProtonMail/go-crypto/openpgp/ed448"
	"github.com/ProtonMail/go-crypto/openpgp/eddsa"
	"github.com/ProtonMail/go-crypto/openpgp/errors"
	"github.com/ProtonMail/go-crypto/openpgp/internal/algorithm"
	"github.com/ProtonMail/go-crypto/openpgp/internal/ecc"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/ProtonMail/go-crypto/openpgp/x25519"
	"github.com/ProtonMail/go-crypto/openpgp/x448"
)

type userIdData struct {
	name, comment, email string
}

type keyProperties struct {
	primaryKey      *packet.PrivateKey
	creationTime    time.Time
	keyLifetimeSecs uint32
	hash            crypto.Hash
	cipher          packet.CipherFunction
	aead            *packet.AEADConfig
	compression     packet.CompressionAlgo
}

// NewEntityWithoutId returns an Entity that contains fresh keys for signing and
// encrypting pgp messages. The key is not associated with an identity.
// This is only allowed for v6 key generation. If v6 is not enabled,
// it will return an error.
// If config is nil, sensible defaults will be used.
func NewEntityWithoutId(config *packet.Config) (*Entity, error) {
	return newEntity(nil, config)
}

// NewEntity returns an Entity that contains fresh keys with a for signing and
// encrypting pgp messages. The key is associated with a
// single identity composed of the given full name, comment and email, any of
// which may be empty but must not contain any of "()<>\x00".
// If config is nil, sensible defaults will be used.
func NewEntity(name, comment, email string, config *packet.Config) (*Entity, error) {
	return newEntity(&userIdData{name, comment, email}, config)
}

func selectKeyProperties(creationTime time.Time, config *packet.Config, primary *packet.PrivateKey) *keyProperties {
	return &keyProperties{
		primaryKey:      primary,
		creationTime:    creationTime,
		keyLifetimeSecs: config.KeyLifetime(),
		hash:            config.Hash(),
		cipher:          config.Cipher(),
		aead:            config.AEAD(),
		compression:     config.Compression(),
	}
}

func newEntity(uid *userIdData, config *packet.Config) (*Entity, error) {
	if uid == nil && !config.V6() {
		return nil, errors.InvalidArgumentError("user id has to be set for non-v6 keys")
	}
	creationTime := config.Now()

	// Generate a primary signing key
	primaryPrivRaw, err := newSigner(config)
	if err != nil {
		return nil, err
	}
	primary := packet.NewSignerPrivateKey(creationTime, primaryPrivRaw)
	if config.V6() {
		if err := primary.UpgradeToV6(); err != nil {
			return nil, err
		}
	}

	keyProperties := selectKeyProperties(creationTime, config, primary)

	e := &Entity{
		PrimaryKey:       &primary.PublicKey,
		PrivateKey:       primary,
		Identities:       make(map[string]*Identity),
		Subkeys:          []Subkey{},
		DirectSignatures: []*packet.VerifiableSignature{},
	}

	if config.V6() {
		if err := e.AddDirectKeySignature(keyProperties, config); err != nil {
			return nil, err
		}
		keyProperties = nil
	}

	if uid != nil {
		err = e.addUserId(*uid, config, keyProperties)
		if err != nil {
			return nil, err
		}
	}

	// NOTE: No key expiry here, but we will not return this subkey in EncryptionKey()
	// if the primary/master key has expired.
	err = e.addEncryptionSubkey(config, creationTime, 0)
	if err != nil {
		return nil, err
	}

	return e, nil
}

// AddUserId adds a user-id packet to the given entity.
func (t *Entity) AddUserId(name, comment, email string, config *packet.Config) error {
	var keyProperties *keyProperties
	if !config.V6() {
		keyProperties = selectKeyProperties(config.Now(), config, t.PrivateKey)
	}
	return t.addUserId(userIdData{name, comment, email}, config, keyProperties)
}

// AddDirectKeySignature adds a fresh direct key signature with the selected key-properties.
func (t *Entity) AddDirectKeySignature(selectedKeyProperties *keyProperties, config *packet.Config) error {
	selfSignature := createSignaturePacket(&t.PrivateKey.PublicKey, packet.SigTypeDirectSignature, config)
	err := writeKeyProperties(selfSignature, selectedKeyProperties)
	if err != nil {
		return err
	}
	err = selfSignature.SignDirectKeyBinding(&t.PrivateKey.PublicKey, t.PrivateKey, config)
	if err != nil {
		return err
	}
	t.DirectSignatures = append(t.DirectSignatures, packet.NewVerifiableSig(selfSignature))
	return nil
}

func writeKeyProperties(selfSignature *packet.Signature, selectedKeyProperties *keyProperties) error {
	advertiseAead := selectedKeyProperties.aead != nil

	selfSignature.CreationTime = selectedKeyProperties.creationTime
	selfSignature.KeyLifetimeSecs = &selectedKeyProperties.keyLifetimeSecs
	selfSignature.FlagsValid = true
	selfSignature.FlagSign = true
	selfSignature.FlagCertify = true
	selfSignature.SEIPDv1 = true // true by default, see 5.8 vs. 5.14
	selfSignature.SEIPDv2 = advertiseAead

	// Set the PreferredHash for the SelfSignature from the packet.Config.
	// If it is not the must-implement algorithm from rfc4880bis, append that.
	hash, ok := algorithm.HashToHashId(selectedKeyProperties.hash)
	if !ok {
		return errors.UnsupportedError("unsupported preferred hash function")
	}

	selfSignature.PreferredHash = []uint8{}
	// Ensure that for signing algorithms with higher security level an
	// appropriate a matching hash function is available.
	acceptableHashes := acceptableHashesToWrite(&selectedKeyProperties.primaryKey.PublicKey)
	var match bool
	for _, acceptableHash := range acceptableHashes {
		if acceptableHash == hash {
			match = true
			break
		}
	}
	if !match && len(acceptableHashes) > 0 {
		selfSignature.PreferredHash = []uint8{acceptableHashes[0]}
	}

	selfSignature.PreferredHash = append(selfSignature.PreferredHash, hash)
	if selectedKeyProperties.hash != crypto.SHA256 {
		selfSignature.PreferredHash = append(selfSignature.PreferredHash, hashToHashId(crypto.SHA256))
	}

	// Likewise for DefaultCipher.
	selfSignature.PreferredSymmetric = []uint8{uint8(selectedKeyProperties.cipher)}
	if selectedKeyProperties.cipher != packet.CipherAES128 {
		selfSignature.PreferredSymmetric = append(selfSignature.PreferredSymmetric, uint8(packet.CipherAES128))
	}

	// We set CompressionNone as the preferred compression algorithm because
	// of compression side channel attacks, then append the configured
	// DefaultCompressionAlgo if any is set (to signal support for cases
	// where the application knows that using compression is safe).
	selfSignature.PreferredCompression = []uint8{uint8(packet.CompressionNone)}
	if selectedKeyProperties.compression != packet.CompressionNone {
		selfSignature.PreferredCompression = append(selfSignature.PreferredCompression, uint8(selectedKeyProperties.compression))
	}

	if advertiseAead {
		// Get the preferred AEAD mode from the packet.Config.
		// If it is not the must-implement algorithm from rfc9580, append that.
		modes := []uint8{uint8(selectedKeyProperties.aead.Mode())}
		if selectedKeyProperties.aead.Mode() != packet.AEADModeOCB {
			modes = append(modes, uint8(packet.AEADModeOCB))
		}

		// For preferred (AES256, GCM), we'll generate (AES256, GCM), (AES256, OCB), (AES128, GCM), (AES128, OCB)
		for _, cipher := range selfSignature.PreferredSymmetric {
			for _, mode := range modes {
				selfSignature.PreferredCipherSuites = append(selfSignature.PreferredCipherSuites, [2]uint8{cipher, mode})
			}
		}
	}

	return nil
}

func (t *Entity) addUserId(userIdData userIdData, config *packet.Config, selectedKeyProperties *keyProperties) error {
	uid := packet.NewUserId(userIdData.name, userIdData.comment, userIdData.email)
	if uid == nil {
		return errors.InvalidArgumentError("user id field contained invalid characters")
	}

	if _, ok := t.Identities[uid.Id]; ok {
		return errors.InvalidArgumentError("user id exist")
	}

	primary := t.PrivateKey
	isPrimaryId := len(t.Identities) == 0
	selfSignature := createSignaturePacket(&primary.PublicKey, packet.SigTypePositiveCert, config)
	if selectedKeyProperties != nil {
		err := writeKeyProperties(selfSignature, selectedKeyProperties)
		if err != nil {
			return err
		}
	}
	selfSignature.IsPrimaryId = &isPrimaryId

	// User ID binding signature
	err := selfSignature.SignUserId(uid.Id, &primary.PublicKey, primary, config)
	if err != nil {
		return err
	}
	t.Identities[uid.Id] = &Identity{
		Primary:            t,
		Name:               uid.Id,
		UserId:             uid,
		SelfCertifications: []*packet.VerifiableSignature{packet.NewVerifiableSig(selfSignature)},
	}
	return nil
}

// AddSigningSubkey adds a signing keypair as a subkey to the Entity.
// If config is nil, sensible defaults will be used.
func (e *Entity) AddSigningSubkey(config *packet.Config) error {
	creationTime := config.Now()
	keyLifetimeSecs := config.KeyLifetime()

	subPrivRaw, err := newSigner(config)
	if err != nil {
		return err
	}
	sub := packet.NewSignerPrivateKey(creationTime, subPrivRaw)
	sub.IsSubkey = true
	// Every subkey for a v6 primary key MUST be a v6 subkey.
	if e.PrimaryKey.Version == 6 {
		if err := sub.UpgradeToV6(); err != nil {
			return err
		}
	}

	subkey := Subkey{
		PublicKey:  &sub.PublicKey,
		PrivateKey: sub,
	}
	sig := createSignaturePacket(e.PrimaryKey, packet.SigTypeSubkeyBinding, config)
	sig.CreationTime = creationTime
	sig.KeyLifetimeSecs = &keyLifetimeSecs
	sig.FlagsValid = true
	sig.FlagSign = true
	sig.EmbeddedSignature = createSignaturePacket(subkey.PublicKey, packet.SigTypePrimaryKeyBinding, config)
	sig.EmbeddedSignature.CreationTime = creationTime

	err = sig.EmbeddedSignature.CrossSignKey(subkey.PublicKey, e.PrimaryKey, subkey.PrivateKey, config)
	if err != nil {
		return err
	}

	err = sig.SignKey(subkey.PublicKey, e.PrivateKey, config)
	if err != nil {
		return err
	}

	subkey.Bindings = []*packet.VerifiableSignature{packet.NewVerifiableSig(sig)}
	subkey.Primary = e

	e.Subkeys = append(e.Subkeys, subkey)
	return nil
}

// AddEncryptionSubkey adds an encryption keypair as a subkey to the Entity.
// If config is nil, sensible defaults will be used.
func (e *Entity) AddEncryptionSubkey(config *packet.Config) error {
	creationTime := config.Now()
	keyLifetimeSecs := config.KeyLifetime()
	return e.addEncryptionSubkey(config, creationTime, keyLifetimeSecs)
}

func (e *Entity) addEncryptionSubkey(config *packet.Config, creationTime time.Time, keyLifetimeSecs uint32) error {
	subPrivRaw, err := newDecrypter(config)
	if err != nil {
		return err
	}
	sub := packet.NewDecrypterPrivateKey(creationTime, subPrivRaw)
	sub.IsSubkey = true
	// Every subkey for a v6 primary key MUST be a v6 subkey.
	if e.PrimaryKey.Version == 6 {
		if err := sub.UpgradeToV6(); err != nil {
			return err
		}
	}

	subkey := Subkey{
		PublicKey:  &sub.PublicKey,
		PrivateKey: sub,
	}
	sig := createSignaturePacket(e.PrimaryKey, packet.SigTypeSubkeyBinding, config)
	sig.CreationTime = creationTime
	sig.KeyLifetimeSecs = &keyLifetimeSecs
	sig.FlagsValid = true
	sig.FlagEncryptStorage = true
	sig.FlagEncryptCommunications = true

	err = sig.SignKey(subkey.PublicKey, e.PrivateKey, config)
	if err != nil {
		return err
	}

	subkey.Bindings = []*packet.VerifiableSignature{packet.NewVerifiableSig(sig)}

	subkey.Primary = e
	e.Subkeys = append(e.Subkeys, subkey)
	return nil
}

// newSigner generates a signing key.
func newSigner(config *packet.Config) (signer interface{}, err error) {
	switch config.PublicKeyAlgorithm() {
	case packet.PubKeyAlgoRSA:
		bits := config.RSAModulusBits()
		if bits < 1024 {
			return nil, errors.InvalidArgumentError("bits must be >= 1024")
		}
		if config != nil && len(config.RSAPrimes) >= 2 {
			primes := config.RSAPrimes[0:2]
			config.RSAPrimes = config.RSAPrimes[2:]
			return generateRSAKeyWithPrimes(config.Random(), 2, bits, primes)
		}
		return rsa.GenerateKey(config.Random(), bits)
	case packet.PubKeyAlgoEdDSA:
		if config.V6() {
			// Implementations MUST NOT accept or generate v6 key material
			// using the deprecated OIDs.
			return nil, errors.InvalidArgumentError("EdDSALegacy cannot be used for v6 keys")
		}
		curve := ecc.FindEdDSAByGenName(string(config.CurveName()))
		if curve == nil {
			return nil, errors.InvalidArgumentError("unsupported curve")
		}

		priv, err := eddsa.GenerateKey(config.Random(), curve)
		if err != nil {
			return nil, err
		}
		return priv, nil
	case packet.PubKeyAlgoECDSA:
		curve := ecc.FindECDSAByGenName(string(config.CurveName()))
		if curve == nil {
			return nil, errors.InvalidArgumentError("unsupported curve")
		}

		priv, err := ecdsa.GenerateKey(config.Random(), curve)
		if err != nil {
			return nil, err
		}
		return priv, nil
	case packet.PubKeyAlgoEd25519:
		priv, err := ed25519.GenerateKey(config.Random())
		if err != nil {
			return nil, err
		}
		return priv, nil
	case packet.PubKeyAlgoEd448:
		priv, err := ed448.GenerateKey(config.Random())
		if err != nil {
			return nil, err
		}
		return priv, nil
	default:
		return nil, errors.InvalidArgumentError("unsupported public key algorithm")
	}
}

// newDecrypter generates an encryption/decryption key.
func newDecrypter(config *packet.Config) (decrypter interface{}, err error) {
	switch config.PublicKeyAlgorithm() {
	case packet.PubKeyAlgoRSA:
		bits := config.RSAModulusBits()
		if bits < 1024 {
			return nil, errors.InvalidArgumentError("bits must be >= 1024")
		}
		if config != nil && len(config.RSAPrimes) >= 2 {
			primes := config.RSAPrimes[0:2]
			config.RSAPrimes = config.RSAPrimes[2:]
			return generateRSAKeyWithPrimes(config.Random(), 2, bits, primes)
		}
		return rsa.GenerateKey(config.Random(), bits)
	case packet.PubKeyAlgoEdDSA, packet.PubKeyAlgoECDSA:
		fallthrough // When passing EdDSA or ECDSA, we generate an ECDH subkey
	case packet.PubKeyAlgoECDH:
		if config.V6() &&
			(config.CurveName() == packet.Curve25519 ||
				config.CurveName() == packet.Curve448) {
			// Implementations MUST NOT accept or generate v6 key material
			// using the deprecated OIDs.
			return nil, errors.InvalidArgumentError("ECDH with Curve25519/448 legacy cannot be used for v6 keys")
		}
		var kdf = ecdh.KDF{
			Hash:   algorithm.SHA512,
			Cipher: algorithm.AES256,
		}
		curve := ecc.FindECDHByGenName(string(config.CurveName()))
		if curve == nil {
			return nil, errors.InvalidArgumentError("unsupported curve")
		}
		return ecdh.GenerateKey(config.Random(), curve, kdf)
	case packet.PubKeyAlgoEd25519, packet.PubKeyAlgoX25519: // When passing Ed25519, we generate an X25519 subkey
		return x25519.GenerateKey(config.Random())
	case packet.PubKeyAlgoEd448, packet.PubKeyAlgoX448: // When passing Ed448, we generate an x448 subkey
		return x448.GenerateKey(config.Random())
	default:
		return nil, errors.InvalidArgumentError("unsupported public key algorithm")
	}
}

var bigOne = big.NewInt(1)

// generateRSAKeyWithPrimes generates a multi-prime RSA keypair of the
// given bit size, using the given random source and prepopulated primes.
func generateRSAKeyWithPrimes(random io.Reader, nprimes int, bits int, prepopulatedPrimes []*big.Int) (*rsa.PrivateKey, error) {
	priv := new(rsa.PrivateKey)
	priv.E = 65537

	if nprimes < 2 {
		return nil, goerrors.New("generateRSAKeyWithPrimes: nprimes must be >= 2")
	}

	if bits < 1024 {
		return nil, goerrors.New("generateRSAKeyWithPrimes: bits must be >= 1024")
	}

	primes := make([]*big.Int, nprimes)

NextSetOfPrimes:
	for {
		todo := bits
		// crypto/rand should set the top two bits in each prime.
		// Thus each prime has the form
		//   p_i = 2^bitlen(p_i) × 0.11... (in base 2).
		// And the product is:
		//   P = 2^todo × α
		// where α is the product of nprimes numbers of the form 0.11...
		//
		// If α < 1/2 (which can happen for nprimes > 2), we need to
		// shift todo to compensate for lost bits: the mean value of 0.11...
		// is 7/8, so todo + shift - nprimes * log2(7/8) ~= bits - 1/2
		// will give good results.
		if nprimes >= 7 {
			todo += (nprimes - 2) / 5
		}
		for i := 0; i < nprimes; i++ {
			var err error
			if len(prepopulatedPrimes) == 0 {
				primes[i], err = rand.Prime(random, todo/(nprimes-i))
				if err != nil {
					return nil, err
				}
			} else {
				primes[i] = prepopulatedPrimes[0]
				prepopulatedPrimes = prepopulatedPrimes[1:]
			}

			todo -= primes[i].BitLen()
		}

		// Make sure that primes is pairwise unequal.
		for i, prime := range primes {
			for j := 0; j < i; j++ {
				if prime.Cmp(primes[j]) == 0 {
					continue NextSetOfPrimes
				}
			}
		}

		n := new(big.Int).Set(bigOne)
		totient := new(big.Int).Set(bigOne)
		pminus1 := new(big.Int)
		for _, prime := range primes {
			n.Mul(n, prime)
			pminus1.Sub(prime, bigOne)
			totient.Mul(totient, pminus1)
		}
		if n.BitLen() != bits {
			// This should never happen for nprimes == 2 because
			// crypto/rand should set the top two bits in each prime.
			// For nprimes > 2 we hope it does not happen often.
			continue NextSetOfPrimes
		}

		priv.D = new(big.Int)
		e := big.NewInt(int64(priv.E))
		ok := priv.D.ModInverse(e, totient)

		if ok != nil {
			priv.Primes = primes
			priv.N = n
			break
		}
	}

	priv.Precompute()
	return priv, nil
}
