package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
)

type manifestEntry struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
}
type indexOrManifest struct {
	MediaType string          `json:"mediaType"`
	Manifests []manifestEntry `json:"manifests,omitempty"`
	Config    *manifestEntry  `json:"config,omitempty"`
	Layers    []manifestEntry `json:"layers,omitempty"`
}

type staged struct {
	digest      string
	data        []byte
	contentType string
}

var client = &http.Client{}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// fetchByRef fetches a manifest/blob by tag or digest and verifies the digest.
func fetchManifest(base, repo, ref string) ([]byte, string, error) {
	url := fmt.Sprintf("%s/v2/%s/manifests/%s", base, repo, ref)
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, "", fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	got := sha256Hex(body)
	if len(ref) >= 7 && ref[:7] == "sha256:" && got != ref {
		return nil, "", fmt.Errorf("digest mismatch for %s: computed %s", ref, got)
	}
	return body, resp.Header.Get("Content-Type"), nil
}

func fetchBlob(base, repo, digest string) ([]byte, error) {
	url := fmt.Sprintf("%s/v2/%s/blobs/%s", base, repo, digest)
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	got := sha256Hex(body)
	if got != digest {
		return nil, fmt.Errorf("digest mismatch for %s: computed %s (corrupt or tampered)", digest, got)
	}
	return body, nil
}

// walk fetches and verifies the whole descriptor closure from srcBase,
// staging everything in memory. Nothing is written anywhere until the
// entire closure is verified.
func walk(srcBase, repo, rootRef string) ([]staged, error) {
	var order []staged
	seen := map[string]bool{}

	var visit func(ref string) error
	visit = func(ref string) error {
		if seen[ref] {
			return nil
		}
		body, ct, err := fetchManifest(srcBase, repo, ref)
		if err != nil {
			return err
		}
		digest := ref
		if len(ref) < 7 || ref[:7] != "sha256:" {
			digest = sha256Hex(body) // tag ref: digest computed from content
		}
		var parsed indexOrManifest
		_ = json.Unmarshal(body, &parsed)

		for _, m := range parsed.Manifests { // index -> platform manifests
			if err := visit(m.Digest); err != nil {
				return err
			}
		}
		if parsed.Config != nil {
			if err := fetchBlobStaged(srcBase, repo, parsed.Config.Digest, &order, seen); err != nil {
				return err
			}
		}
		for _, l := range parsed.Layers {
			if err := fetchBlobStaged(srcBase, repo, l.Digest, &order, seen); err != nil {
				return err
			}
		}
		seen[ref] = true
		order = append(order, staged{digest: digest, data: body, contentType: ct})
		return nil
	}

	if err := visit(rootRef); err != nil {
		return nil, err
	}
	return order, nil
}

func fetchBlobStaged(base, repo, digest string, order *[]staged, seen map[string]bool) error {
	if seen[digest] {
		return nil
	}
	data, err := fetchBlob(base, repo, digest)
	if err != nil {
		return err
	}
	seen[digest] = true
	*order = append(*order, staged{digest: digest, data: data, contentType: "application/octet-stream"})
	return nil
}

func pushBlob(base, repo string, s staged) error {
	// start upload
	startURL := fmt.Sprintf("%s/v2/%s/blobs/uploads/", base, repo)
	resp, err := client.Post(startURL, "", nil)
	if err != nil {
		return err
	}
	loc := resp.Header.Get("Location")
	resp.Body.Close()
	// finalize with PUT including digest
	putURL := fmt.Sprintf("%s?digest=%s", loc, s.digest)
	if bytes.Contains([]byte(loc), []byte("?")) {
		putURL = fmt.Sprintf("%s&digest=%s", loc, s.digest)
	}
	req, _ := http.NewRequest("PUT", putURL, bytes.NewReader(s.data))
	req.Header.Set("Content-Type", "application/octet-stream")
	resp2, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 201 {
		b, _ := io.ReadAll(resp2.Body)
		return fmt.Errorf("push blob %s: status %d: %s", s.digest, resp2.StatusCode, string(b))
	}
	return nil
}

func pushManifest(base, repo, ref string, s staged) error {
	url := fmt.Sprintf("%s/v2/%s/manifests/%s", base, repo, ref)
	req, _ := http.NewRequest("PUT", url, bytes.NewReader(s.data))
	req.Header.Set("Content-Type", s.contentType)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("push manifest %s: status %d: %s", ref, resp.StatusCode, string(b))
	}
	return nil
}

func isManifestType(ct string) bool {
	return ct == "application/vnd.oci.image.index.v1+json" ||
		ct == "application/vnd.docker.distribution.manifest.list.v2+json" ||
		ct == "application/vnd.oci.image.manifest.v1+json" ||
		ct == "application/vnd.docker.distribution.manifest.v2+json"
}

func main() {
	src := flag.String("src", "http://localhost:5001", "source peer base URL")
	dst := flag.String("dst", "http://localhost:5003", "destination (local) base URL")
	repo := flag.String("repo", "myapp", "repository name")
	tag := flag.String("tag", "latest", "tag to transfer")
	flag.Parse()

	fmt.Printf("Transferring %s/%s:%s  from %s  to %s\n", *repo, *tag, *tag, *src, *dst)
	fmt.Println("Walking descriptor graph, fetching + verifying every node...")

	staged, err := walk(*src, *repo, *tag)
	if err != nil {
		fmt.Printf("[FAIL] transfer aborted: %v\n", err)
		fmt.Println("[PASS] nothing was published to destination (fail-closed)")
		os.Exit(1)
	}
	fmt.Printf("[PASS] full closure verified: %d nodes\n", len(staged))

	fmt.Println("Publishing verified closure to destination (blobs first, root manifest last)...")
	for _, s := range staged {
		var perr error
		if isManifestType(s.contentType) {
			// publish by digest first; we'll tag the root at the very end
			perr = pushManifest(*dst, *repo, s.digest, s)
		} else {
			perr = pushBlob(*dst, *repo, s)
		}
		if perr != nil {
			fmt.Printf("[FAIL] publish failed partway: %v\n", perr)
			os.Exit(1)
		}
	}
	// root is the LAST staged element (post-order); tag it now, last of all.
	root := staged[len(staged)-1]
	if err := pushManifest(*dst, *repo, *tag, root); err != nil {
		fmt.Printf("[FAIL] tagging root failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("[PASS] root published and tagged '%s' — LAST, only after full closure verified\n", *tag)
}
