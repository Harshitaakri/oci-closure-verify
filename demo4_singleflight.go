package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"

	"golang.org/x/sync/singleflight"
)

var (
	group        singleflight.Group
	realFetches  int32 // counts actual HTTP GETs that hit the network
)

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// fetchBlobDeduped wraps the real fetch in singleflight, keyed by digest.
func fetchBlobDeduped(base, repo, digest string) ([]byte, error) {
	v, err, shared := group.Do(digest, func() (interface{}, error) {
		atomic.AddInt32(&realFetches, 1) // only increments on an actual network call
		url := fmt.Sprintf("%s/v2/%s/blobs/%s", base, repo, digest)
		resp, err := http.Get(url)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		got := sha256Hex(body)
		if got != digest {
			return nil, fmt.Errorf("digest mismatch")
		}
		return body, nil
	})
	if err != nil {
		return nil, err
	}
	if shared {
		fmt.Println("  (this caller got a SHARED result — no extra network call)")
	}
	return v.([]byte), nil
}

func main() {
	base := "http://localhost:5002" // peerC
	repo := "myapp"
	digest := "sha256:0b4812acd9ecd32b96a2d8c30f9f91632a42a56f0bfceed3322402dbfb6d9083"

	var wg sync.WaitGroup
	results := make([]error, 2)

	fmt.Println("Firing 2 simultaneous requests for the SAME digest...")
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, err := fetchBlobDeduped(base, repo, digest)
			results[idx] = err
			fmt.Printf("caller %d finished, err=%v\n", idx, err)
		}(i)
	}
	wg.Wait()

	fmt.Printf("\n[RESULT] logical requests: 2, actual network fetches: %d\n", realFetches)
	if realFetches == 1 {
		fmt.Println("[PASS] singleflight deduped the concurrent identical requests")
	} else {
		fmt.Println("[FAIL] more than one network fetch occurred — dedup did not happen")
	}
}
