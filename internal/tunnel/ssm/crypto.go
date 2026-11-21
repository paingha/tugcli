package ssm

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
)

// KMS encryption of session data, as the session-manager-plugin and the
// agent do it. When the Session Manager preferences name a KMS key, the
// agent asks for it in the handshake. The client calls GenerateDataKey for
// 64 bytes with the session and target as encryption context and hands the
// encrypted key back; the agent decrypts it. The agent encrypts with the
// first 32 bytes and the client with the last 32, each with AES-256-GCM and
// a 12-byte nonce in front of the sealed data. Only stream data (payload
// type Output, and StdErr and ExitCode from the agent) is encrypted; the
// agent proves the keys with an encrypted challenge that the client
// decrypts and encrypts again.

// KMSAPI is the part of the KMS client that KMS-encrypted sessions use.
type KMSAPI interface {
	GenerateDataKey(ctx context.Context, in *kms.GenerateDataKeyInput, opts ...func(*kms.Options)) (*kms.GenerateDataKeyOutput, error)
}

var _ KMSAPI = (*kms.Client)(nil)

const (
	// dataKeySize is the size of the data key: one 32-byte AES-256 key for
	// each direction.
	dataKeySize = 64
	nonceSize   = 12

	contextSessionID = "aws:ssm:SessionId"
	contextTargetID  = "aws:ssm:TargetId"
)

// kmsEncryptionRequest is the parameter of the KMSEncryption action. Newer
// agents add a random challenge for the encryption context; the plugin
// does not acknowledge it, and neither does tug.
type kmsEncryptionRequest struct {
	KMSKeyID string `json:"KMSKeyId"`
}

// kmsEncryptionResponse is the result of the KMSEncryption action.
type kmsEncryptionResponse struct {
	KMSCipherTextKey  []byte `json:"KMSCipherTextKey"`
	KMSCipherTextHash []byte `json:"KMSCipherTextHash"`
}

// encryptionChallenge is the payload of the agent's challenge and of the
// client's answer.
type encryptionChallenge struct {
	Challenge []byte `json:"Challenge"`
}

// sessionCipher encrypts the client's stream data and decrypts the agent's.
type sessionCipher struct {
	seal, open cipher.AEAD
	prefix     [4]byte
	counter    atomic.Uint64
}

// newSessionCipher splits a 64-byte data key into the agent's half, which
// the client decrypts with, and the client's half.
func newSessionCipher(key []byte) (*sessionCipher, error) {
	if len(key) != dataKeySize {
		return nil, fmt.Errorf("the data key has %d bytes, not %d", len(key), dataKeySize)
	}
	open, err := newGCM(key[:dataKeySize/2])
	if err != nil {
		return nil, err
	}
	seal, err := newGCM(key[dataKeySize/2:])
	if err != nil {
		return nil, err
	}
	s := &sessionCipher{seal: seal, open: open}
	// Nonces are a random prefix and a counter, so none repeats under the
	// key; the plugin uses random ones and the agent a counter.
	_, _ = rand.Read(s.prefix[:])
	return s, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("creating the AES cipher: %w", err)
	}
	return cipher.NewGCM(block)
}

// encrypt returns the nonce followed by plain sealed with the client's key.
func (s *sessionCipher) encrypt(plain []byte) []byte {
	nonce := make([]byte, nonceSize, nonceSize+len(plain)+s.seal.Overhead())
	copy(nonce, s.prefix[:])
	binary.BigEndian.PutUint64(nonce[4:], s.counter.Add(1))
	return s.seal.Seal(nonce, nonce, plain, nil)
}

// errDecrypt marks stream data that does not decrypt with the agent's key.
var errDecrypt = errors.New("stream data does not decrypt with the session's KMS data key")

// decrypt opens data the agent sealed with its key.
func (s *sessionCipher) decrypt(sealed []byte) ([]byte, error) {
	if len(sealed) < nonceSize+s.open.Overhead() {
		return nil, errDecrypt
	}
	plain, err := s.open.Open(nil, sealed[:nonceSize], sealed[nonceSize:], nil)
	if err != nil {
		return nil, errDecrypt
	}
	return plain, nil
}

// setUpEncryption asks KMS for the session's data key and starts
// encrypting. It returns the encrypted key for the agent.
func (c *conn) setUpEncryption(ctx context.Context, keyID string) ([]byte, error) {
	if c.kms == nil {
		return nil, errors.New("no KMS client is configured")
	}
	if keyID == "" {
		return nil, errors.New("the agent named no KMS key")
	}
	out, err := c.kms.GenerateDataKey(ctx, &kms.GenerateDataKeyInput{
		KeyId:         awsv2.String(keyID),
		NumberOfBytes: awsv2.Int32(dataKeySize),
		EncryptionContext: map[string]string{
			contextSessionID: c.sessionID,
			contextTargetID:  c.target,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("generating a data key with KMS key %s; check that you may use kms:GenerateDataKey on it: %w", keyID, err)
	}
	enc, err := newSessionCipher(out.Plaintext)
	clear(out.Plaintext)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.enc = enc
	c.mu.Unlock()
	return out.CiphertextBlob, nil
}

// answerChallenge decrypts the agent's challenge and sends it back
// encrypted with the client's key, which proves both keys.
func (c *conn) answerChallenge(m message) error {
	c.mu.Lock()
	enc := c.enc
	c.mu.Unlock()
	if enc == nil {
		return errors.New("the agent sent an encryption challenge without asking for KMS encryption")
	}
	var req encryptionChallenge
	if err := json.Unmarshal(m.Payload, &req); err != nil {
		return fmt.Errorf("reading the agent's encryption challenge: %w", err)
	}
	plain, err := enc.decrypt(req.Challenge)
	if err != nil {
		return fmt.Errorf("decrypting the agent's encryption challenge: %w", err)
	}
	if err := c.queueJSON(payloadEncChallengeResponse, encryptionChallenge{Challenge: enc.encrypt(plain)}); err != nil {
		return fmt.Errorf("answering the agent's encryption challenge: %w", err)
	}
	return nil
}
