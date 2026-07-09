package archivers

import (
	"sync"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/transport/client"
)

// TestInstallGitProtocols verifies concurrent registration does not race the
// go-git global protocol map (regression for the per-job InstallProtocol crash).
func TestInstallGitProtocols(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			installGitProtocols()
		}()
	}
	wg.Wait()

	if client.Protocols["https"] == nil || client.Protocols["http"] == nil {
		t.Fatal("expected http and https protocols to be registered")
	}
}
