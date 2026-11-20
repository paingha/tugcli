package cli

import (
	"bufio"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// tugd calls daemonUpstream from concurrent proxy requests, so what it
// reaches (the store and registry, loaded on first use, and the encrypted
// file store) must be safe to share. The race detector checks this.
func TestServicesAreSafeForConcurrentUse(t *testing.T) {
	isolate(t)
	a := &app{stdin: strings.NewReader(""), in: bufio.NewReader(strings.NewReader("")), stdout: io.Discard, stderr: io.Discard, now: time.Now}
	if err := a.setup(false); err != nil {
		t.Fatal(err)
	}
	cat, err := a.loadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, broker, err := a.services(cat)
			if err != nil {
				t.Error(err)
				return
			}
			key := fmt.Sprintf("test/%d", i)
			if err := broker.Store.Set(key, "v"); err != nil {
				t.Error(err)
				return
			}
			if v, err := broker.Store.Get(key); err != nil || v != "v" {
				t.Errorf("Get(%s) = %q, %v", key, v, err)
			}
		}()
	}
	wg.Wait()
}
