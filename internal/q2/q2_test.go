package q2

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Writes at once, as the sign-in's from several goroutines, each land whole: the file is always
// one of them, and no temporary file is left.
func TestWriteFileAtOnce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b, _ := json.Marshal(map[string]any{"token": strings.Repeat("x", 100+i*997), "n": i})
			for k := 0; k < 20; k++ {
				if err := WriteFile(path, b); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	b, err := os.ReadFile(path)
	var v struct {
		Token string
		N     int
	}
	if err != nil || json.Unmarshal(b, &v) != nil || len(v.Token) != 100+v.N*997 {
		t.Fatalf("%v: %.80q", err, b)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Fatalf("%d files left", len(ents))
	}
}
