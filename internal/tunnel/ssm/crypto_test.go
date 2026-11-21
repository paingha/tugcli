package ssm

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/smithy-go"
)

func TestSessionCipher(t *testing.T) {
	key := make([]byte, dataKeySize)
	_, _ = rand.Read(key)
	enc, err := newSessionCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	agent, client := agentGCM(key[:32]), agentGCM(key[32:])

	// The client seals with the second half of the key.
	sealed := enc.encrypt([]byte("from the client"))
	if got, err := agentOpen(client, sealed); err != nil || string(got) != "from the client" {
		t.Errorf("opening the client's data with the second half = %q, %v", got, err)
	}
	if len(sealed) != nonceSize+len("from the client")+16 {
		t.Errorf("len(encrypt()) = %d, want the nonce, the data and the tag", len(sealed))
	}
	if again := enc.encrypt([]byte("from the client")); bytes.Equal(again[:nonceSize], sealed[:nonceSize]) {
		t.Error("encrypt() reused a nonce")
	}

	// It opens what the agent sealed with the first half.
	if got, err := enc.decrypt(agentSeal(agent, []byte("from the agent"))); err != nil || string(got) != "from the agent" {
		t.Errorf("decrypt(agent's data) = %q, %v", got, err)
	}
	tampered := agentSeal(agent, []byte("from the agent"))
	tampered[len(tampered)-1] ^= 1
	for name, in := range map[string][]byte{
		"tampered":             tampered,
		"short":                make([]byte, nonceSize),
		"the client's own":     enc.encrypt([]byte("x")),
		"sealed by a stranger": agentSeal(agentGCM(make([]byte, 32)), []byte("x")),
	} {
		if _, err := enc.decrypt(in); !errors.Is(err, errDecrypt) {
			t.Errorf("decrypt(%s) error = %v, want errDecrypt", name, err)
		}
	}
	if _, err := newSessionCipher(key[:32]); err == nil {
		t.Error("newSessionCipher(32 bytes) error = nil")
	}
}

func TestKMSEncryptedSession(t *testing.T) {
	keys := &fakeKMS{}
	agent := &fakeAgent{kmsKeyID: "alias/session-manager", kms: keys, greeting: []string{"sealed ", "greeting"}}
	api, opts := newFake(t, agent)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Dial(ctx, Config{Target: testTarget}, "10.0.0.1:443", append(opts, WithKMS(keys))...)
	if err != nil {
		agent.check(t)
		t.Fatalf("Dial() error = %v", err)
	}
	defer c.Close()

	greeting := make([]byte, len("sealed greeting"))
	if _, err := io.ReadFull(c, greeting); err != nil || string(greeting) != "sealed greeting" {
		t.Fatalf("reading the greeting = %q, %v", greeting, err)
	}
	data := make([]byte, 3*chunkSize+5)
	_, _ = rand.Read(data)
	if _, err := c.Write(data); err != nil {
		t.Fatal(err)
	}
	echo := make([]byte, len(data))
	if _, err := io.ReadFull(c, echo); err != nil || !bytes.Equal(echo, data) {
		t.Fatalf("reading the echo: %v, equal %v", err, bytes.Equal(echo, data))
	}

	s := agent.session(t, 0)
	keys.mu.Lock()
	calls := slices.Clone(keys.calls)
	keys.mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("GenerateDataKey calls = %d, want 1", len(calls))
	}
	in := calls[0]
	wantContext := map[string]string{contextSessionID: s.id, contextTargetID: testTarget}
	if awsv2.ToString(in.KeyId) != "alias/session-manager" || awsv2.ToInt32(in.NumberOfBytes) != 64 || !maps.Equal(in.EncryptionContext, wantContext) {
		t.Errorf("GenerateDataKey(KeyId %q, NumberOfBytes %d, EncryptionContext %v), want alias/session-manager, 64, %v",
			awsv2.ToString(in.KeyId), awsv2.ToInt32(in.NumberOfBytes), in.EncryptionContext, wantContext)
	}

	resp := s.handshakeResponse()
	if resp == nil || len(resp.ProcessedClientActions) != 2 {
		t.Fatalf("handshake response = %+v, want two actions", resp)
	}
	kmsAction := resp.ProcessedClientActions[1]
	var result kmsEncryptionResponse
	_ = json.Unmarshal(mustJSON(kmsAction.ActionResult), &result)
	if kmsAction.ActionType != actionKMSEncryption || kmsAction.ActionStatus != actionSuccess || string(result.KMSCipherTextKey) != "encrypted-data-key-1" {
		t.Errorf("KMS action = %+v, want success with the encrypted data key", kmsAction)
	}

	// Data travelled sealed: no payload holds the plain text.
	s.mu.Lock()
	sealed := slices.Clone(s.sealedInput)
	s.mu.Unlock()
	if len(sealed) != 4 {
		t.Fatalf("sealed data messages = %d, want 4", len(sealed))
	}
	for i, p := range sealed {
		chunk := data[i*chunkSize : min((i+1)*chunkSize, len(data))]
		if bytes.Contains(p, chunk) || len(p) != len(chunk)+nonceSize+16 {
			t.Errorf("data message %d is %d bytes for %d of data, or holds it in the clear", i, len(p), len(chunk))
		}
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close() error = %v", err)
	}
	if got := api.terminations(); !slices.Equal(got, []string{s.id}) {
		t.Errorf("TerminateSession calls = %v, want [%s]", got, s.id)
	}
	agent.check(t)
}

func TestKMSEncryptionFails(t *testing.T) {
	keys := &fakeKMS{err: &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "not allowed"}}
	agent := &fakeAgent{kmsKeyID: "alias/session-manager", kms: keys}
	api, opts := newFake(t, agent)
	_, err := Dial(context.Background(), Config{Target: testTarget}, "10.0.0.1:443", append(opts, WithKMS(keys))...)
	if !errors.Is(err, ErrKMSEncryption) || !strings.Contains(err.Error(), "kms:GenerateDataKey") || !strings.Contains(err.Error(), "AccessDeniedException") {
		t.Fatalf("Dial() error = %v, want ErrKMSEncryption naming kms:GenerateDataKey and the cause", err)
	}
	s := agent.session(t, 0)
	s.waitDone(t)
	resp := s.handshakeResponse()
	if resp == nil || len(resp.ProcessedClientActions) != 2 || resp.ProcessedClientActions[1].ActionStatus != actionFailed {
		t.Errorf("handshake response = %+v, want the KMS action failed", resp)
	}
	if got := api.terminations(); !slices.Equal(got, []string{s.id}) {
		t.Errorf("TerminateSession calls = %v, want [%s]", got, s.id)
	}
	agent.check(t)
}
