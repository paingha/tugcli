package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

func serveLines(t *testing.T, in string) map[string]json.RawMessage {
	t.Helper()
	var out, errb bytes.Buffer
	if code := Run(context.Background(), fake{}, []string{OpServe}, strings.NewReader(in), &out, &errb); code != ExitOK {
		t.Fatalf("serve exit %d: %s", code, errb.String())
	}
	got := map[string]json.RawMessage{}
	sc := bufio.NewScanner(&out)
	for sc.Scan() {
		var r ServeResponse
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("serve wrote a line that is not a response: %q", sc.Text())
		}
		got[r.ID] = r.Response
	}
	return got
}

func TestServeAnswersEachRequestByID(t *testing.T) {
	list := `{"apiVersion":"provider/v1alpha1","kind":"ListRequest","account":{"id":"a"},"secrets":{"api_token":"good"}}`
	in := `{"id":"1","op":"describe"}
{"id":"2","op":"list","request":` + list + `}
{"id":"3","op":"credential","request":{"kind":"CredentialRequest","clusterID":"c-1"}}
{"id":"4","op":"nope"}
not json
{"id":"5","op":"list"}
`
	got := serveLines(t, in)
	if d, err := DecodeDescribe(got["1"]); err != nil || d.Name != "fake" {
		t.Errorf("describe = %+v, %v", d, err)
	}
	if cl, err := DecodeList(got["2"]); err != nil || len(cl) != 1 || cl[0].ID != "c-1" {
		t.Errorf("list = %+v, %v", cl, err)
	}
	if c, err := DecodeCredential(got["3"]); err != nil || c.Token != "tok-c-1" {
		t.Errorf("credential = %+v, %v", c, err)
	}
	for _, id := range []string{"4", "", "5"} {
		if _, err := DecodeList(got[id]); CodeOf(err) != InvalidConfig {
			t.Errorf("response %q = %v, want InvalidConfig", id, err)
		}
	}
}

// slowLister blocks in List until its context ends, to check cancel.
type slowLister struct{ fake }

func (slowLister) List(ctx context.Context, _ ListRequest) ([]Cluster, error) {
	<-ctx.Done()
	return nil, Errorf(Unavailable, "cancelled")
}

func TestServeCancelsOneRequest(t *testing.T) {
	inR, inW := io.Pipe()
	var out syncBuffer
	done := make(chan int)
	go func() { done <- Run(context.Background(), slowLister{}, []string{OpServe}, inR, &out, io.Discard) }()
	write := func(s string) {
		t.Helper()
		if _, err := io.WriteString(inW, s+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"id":"slow","op":"list","request":{"account":{"id":"a"}}}`)
	write(`{"id":"quick","op":"describe"}`)
	write(`{"id":"slow","cancel":true}`)
	inW.Close()
	select {
	case code := <-done:
		if code != ExitOK {
			t.Fatalf("exit %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not exit after stdin closed and the slow request was cancelled")
	}
	got := out.String()
	if !strings.Contains(got, `"id":"quick"`) || !strings.Contains(got, `"id":"slow"`) || !strings.Contains(got, "Unavailable") {
		t.Fatalf("output:\n%s", got)
	}
}

// syncBuffer is a bytes.Buffer safe for the serve goroutines to share.
type syncBuffer struct {
	mu  chan struct{}
	buf bytes.Buffer
}

func (b *syncBuffer) lock() {
	if b.mu == nil {
		b.mu = make(chan struct{}, 1)
	}
	b.mu <- struct{}{}
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.lock()
	defer func() { <-b.mu }()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.lock()
	defer func() { <-b.mu }()
	return b.buf.String()
}

// lineReader hands out one line per Read and calls before ahead of each one,
// so a test can see what was written before serve asked for more.
type lineReader struct {
	lines  []string
	before func(next int)
	next   int
}

func (r *lineReader) Read(p []byte) (int, error) {
	r.before(r.next)
	if r.next == len(r.lines) {
		return 0, io.EOF
	}
	n := copy(p, r.lines[r.next]+"\n")
	r.next++
	return n, nil
}

// TestServeInOrderAnswersBeforeReading checks the loop WebAssembly plugins
// use: each request is answered before the next line is read, since a read
// there blocks the whole module.
func TestServeInOrderAnswersBeforeReading(t *testing.T) {
	var out bytes.Buffer
	in := &lineReader{lines: []string{
		`{"id":"1","op":"describe"}`,
		`{"id":"1","cancel":true}`,
		`{"id":"2","op":"nope"}`,
		`{"id":"3","op":"list","request":{"account":{"id":"a"},"secrets":{"api_token":"good"}}}`,
	}}
	answered := []int{0, 1, 1, 2, 3} // responses written before each read
	in.before = func(next int) {
		if got := strings.Count(out.String(), "\n"); got != answered[next] {
			t.Errorf("before reading line %d, %d responses were written, want %d", next+1, got, answered[next])
		}
	}
	s := &serveSession{p: fake{}, stdout: &out, stderr: io.Discard, cancels: map[string]context.CancelFunc{}}
	if code := s.inOrder(context.Background(), in); code != ExitOK {
		t.Fatalf("inOrder exit %d, want %d", code, ExitOK)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	for i, want := range []string{`"id":"1"`, `"id":"2"`, `"id":"3"`} {
		if i >= len(lines) || !strings.Contains(lines[i], want) {
			t.Fatalf("responses = %q, want %s at %d", lines, want, i)
		}
	}
}
