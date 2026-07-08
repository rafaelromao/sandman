package batch

import (
	"bytes"
	"sync"
	"testing"
)

func TestLinePrefixWriterConcurrent(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	lp := NewLinePrefixWriter("test", &buf)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				line := []byte("goroutine-%d-line-%d\n")
				lp.Write(line)
			}
		}(i)
	}
	wg.Wait()

	if err := lp.Flush(); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}

	if buf.Len() == 0 {
		t.Fatal("expected output, got none")
	}
}
