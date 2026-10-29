package image_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MahyarNemati/mini-container/internal/image"
)

func TestParseReferenceDefaults(t *testing.T) {
	// The defaulting rules are quirky and worth encoding once rather than
	// rediscovering: a bare name is Docker Hub's library namespace, one slash
	// without a dot is a Hub user, and "latest" is implicit.
	for _, tc := range []struct {
		in       string
		registry string
		name     string
		tag      string
		digest   string
	}{
		{"alpine", "registry-1.docker.io", "library/alpine", "latest", ""},
		{"alpine:3.20", "registry-1.docker.io", "library/alpine", "3.20", ""},
		{"user/image", "registry-1.docker.io", "user/image", "latest", ""},
		{"user/image:v1", "registry-1.docker.io", "user/image", "v1", ""},
		{"ghcr.io/user/image", "ghcr.io", "user/image", "latest", ""},
		{"ghcr.io/user/image:v2", "ghcr.io", "user/image", "v2", ""},
		{"localhost:5000/img", "localhost:5000", "img", "latest", ""},
		{"registry.example.com/a/b/c:tag", "registry.example.com", "a/b/c", "tag", ""},
		{"alpine@sha256:abc123", "registry-1.docker.io", "library/alpine", "", "sha256:abc123"},
	} {
		got, err := image.ParseReference(tc.in)
		if err != nil {
			t.Errorf("ParseReference(%q): %v", tc.in, err)
			continue
		}
		if got.Registry != tc.registry || got.Name != tc.name || got.Tag != tc.tag || got.Digest != tc.digest {
			t.Errorf("ParseReference(%q) = %+v, want registry=%q name=%q tag=%q digest=%q",
				tc.in, got, tc.registry, tc.name, tc.tag, tc.digest)
		}
	}
}

func TestParseReferenceHandlesAPortWithoutMistakingItForATag(t *testing.T) {
	// The last colon is only a tag separator when it comes after the last
	// slash; otherwise "localhost:5000/img" parses as tag "5000/img".
	ref, err := image.ParseReference("localhost:5000/img")
	if err != nil {
		t.Fatalf("ParseReference: %v", err)
	}
	if ref.Tag != "latest" || ref.Registry != "localhost:5000" {
		t.Errorf("got %+v", ref)
	}
}

func TestParseReferenceRejectsEmpty(t *testing.T) {
	for _, in := range []string{"", "   "} {
		if _, err := image.ParseReference(in); err == nil {
			t.Errorf("ParseReference(%q) should fail", in)
		}
	}
}

func TestReferenceString(t *testing.T) {
	ref, _ := image.ParseReference("ghcr.io/user/img:v1")
	if got := ref.String(); got != "ghcr.io/user/img:v1" {
		t.Errorf("String = %q", got)
	}
	ref, _ = image.ParseReference("alpine@sha256:deadbeef")
	if got := ref.String(); !strings.Contains(got, "@sha256:deadbeef") {
		t.Errorf("String = %q", got)
	}
}

// --- a fake registry, so the pull path is tested without the network ---

type layer struct {
	digest string
	body   []byte
}

func gzipTar(t *testing.T, build func(*tar.Writer)) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	build(tw)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// fakeRegistry serves a single-platform manifest and its layers.
func fakeRegistry(t *testing.T, layers []layer, index bool) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()
	var srv *httptest.Server

	// No auth challenge: a 200 on /v2/ means anonymous access, which the
	// puller must handle as well as the token flow.
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	})

	descriptors := make([]map[string]any, 0, len(layers))
	for _, l := range layers {
		descriptors = append(descriptors, map[string]any{
			"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
			"digest":    l.digest,
			"size":      len(l.body),
		})
	}

	manifestBody, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config":        map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": "sha256:cfg", "size": 1},
		"layers":        descriptors,
	})
	manifestDigest := digestOf(manifestBody)

	indexBody, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.index.v1+json",
		"manifests": []map[string]any{
			{
				"mediaType": "application/vnd.oci.image.manifest.v1+json",
				"digest":    manifestDigest,
				"size":      len(manifestBody),
				"platform":  map[string]any{"os": "linux", "architecture": "amd64"},
			},
			{
				"mediaType": "application/vnd.oci.image.manifest.v1+json",
				"digest":    manifestDigest,
				"size":      len(manifestBody),
				"platform":  map[string]any{"os": "linux", "architecture": "arm64"},
			},
		},
	})

	mux.HandleFunc("/v2/library/test/manifests/", func(w http.ResponseWriter, r *http.Request) {
		ref := strings.TrimPrefix(r.URL.Path, "/v2/library/test/manifests/")
		if index && ref == "latest" {
			w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
			w.Header().Set("Docker-Content-Digest", digestOf(indexBody))
			_, _ = w.Write(indexBody)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", manifestDigest)
		_, _ = w.Write(manifestBody)
	})

	mux.HandleFunc("/v2/library/test/blobs/", func(w http.ResponseWriter, r *http.Request) {
		want := strings.TrimPrefix(r.URL.Path, "/v2/library/test/blobs/")
		for _, l := range layers {
			if l.digest == want {
				_, _ = w.Write(l.body)
				return
			}
		}
		http.NotFound(w, r)
	})

	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func pullFrom(t *testing.T, srv *httptest.Server, arch, dir string) error {
	t.Helper()
	ref, err := image.ParseReference(strings.TrimPrefix(srv.URL, "http://") + "/library/test")
	if err != nil {
		t.Fatalf("ParseReference: %v", err)
	}
	p := image.NewPuller()
	p.Arch = arch
	p.OS = "linux"
	// The fake registry serves plain HTTP, which is also how a local
	// development registry is usually run.
	p.Insecure = true
	return p.Pull(context.Background(), ref, dir)
}

func TestExtractionAppliesLayersInOrder(t *testing.T) {
	first := gzipTar(t, func(tw *tar.Writer) {
		writeTarFile(t, tw, "etc/hosts", "from layer 1")
		writeTarFile(t, tw, "bin/sh", "shell")
	})
	second := gzipTar(t, func(tw *tar.Writer) {
		writeTarFile(t, tw, "etc/hosts", "from layer 2")
	})

	srv := fakeRegistry(t, []layer{
		{digest: digestOf(first), body: first},
		{digest: digestOf(second), body: second},
	}, false)

	dir := t.TempDir()
	if err := pullFrom(t, srv, "amd64", dir); err != nil {
		t.Fatalf("Pull: %v", err)
	}

	// A later layer must win.
	got := readFile(t, filepath.Join(dir, "etc/hosts"))
	if got != "from layer 2" {
		t.Errorf("etc/hosts = %q, want the second layer's content", got)
	}
	if readFile(t, filepath.Join(dir, "bin/sh")) != "shell" {
		t.Error("a file only in the first layer was lost")
	}
}

func TestWhiteoutDeletesALowerLayerFile(t *testing.T) {
	// Docker's layer format represents a deletion as a .wh.<name> entry,
	// which an extractor must interpret rather than create as a file.
	first := gzipTar(t, func(tw *tar.Writer) {
		writeTarFile(t, tw, "etc/removeme", "doomed")
		writeTarFile(t, tw, "etc/keepme", "safe")
	})
	second := gzipTar(t, func(tw *tar.Writer) {
		writeTarFile(t, tw, "etc/.wh.removeme", "")
	})

	srv := fakeRegistry(t, []layer{
		{digest: digestOf(first), body: first},
		{digest: digestOf(second), body: second},
	}, false)

	dir := t.TempDir()
	if err := pullFrom(t, srv, "amd64", dir); err != nil {
		t.Fatalf("Pull: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "etc/removeme")); !os.IsNotExist(err) {
		t.Error("the whiteout did not delete the file")
	}
	if _, err := os.Stat(filepath.Join(dir, "etc/.wh.removeme")); !os.IsNotExist(err) {
		t.Error("the whiteout marker itself must not be extracted as a file")
	}
	if readFile(t, filepath.Join(dir, "etc/keepme")) != "safe" {
		t.Error("the whiteout removed the wrong file")
	}
}

func TestOpaqueWhiteoutClearsADirectory(t *testing.T) {
	first := gzipTar(t, func(tw *tar.Writer) {
		writeTarFile(t, tw, "var/a", "a")
		writeTarFile(t, tw, "var/b", "b")
	})
	second := gzipTar(t, func(tw *tar.Writer) {
		writeTarFile(t, tw, "var/.wh..wh..opq", "")
		writeTarFile(t, tw, "var/c", "c")
	})

	srv := fakeRegistry(t, []layer{
		{digest: digestOf(first), body: first},
		{digest: digestOf(second), body: second},
	}, false)

	dir := t.TempDir()
	if err := pullFrom(t, srv, "amd64", dir); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	for _, gone := range []string{"var/a", "var/b"} {
		if _, err := os.Stat(filepath.Join(dir, gone)); !os.IsNotExist(err) {
			t.Errorf("%s should have been cleared by the opaque whiteout", gone)
		}
	}
	if readFile(t, filepath.Join(dir, "var/c")) != "c" {
		t.Error("the new file in the opaque directory was lost")
	}
}

func TestPathTraversalInALayerIsRefused(t *testing.T) {
	// A layer containing ../../etc/passwd is a real attack, not a
	// hypothetical.
	evil := gzipTar(t, func(tw *tar.Writer) {
		writeTarFile(t, tw, "../../etc/passwd", "pwned")
	})
	srv := fakeRegistry(t, []layer{{digest: digestOf(evil), body: evil}}, false)

	dir := t.TempDir()
	err := pullFrom(t, srv, "amd64", dir)
	if err == nil {
		t.Fatal("a traversing tar entry must be refused")
	}
	if !strings.Contains(err.Error(), "escape") {
		t.Errorf("error should say the entry escapes the directory, got %v", err)
	}
}

func TestDigestMismatchIsDetected(t *testing.T) {
	// The digest is the only thing making this a content-addressed fetch
	// rather than blind trust in the network.
	body := gzipTar(t, func(tw *tar.Writer) { writeTarFile(t, tw, "a", "a") })
	srv := fakeRegistry(t, []layer{
		{digest: "sha256:" + strings.Repeat("00", 32), body: body},
	}, false)

	dir := t.TempDir()
	err := pullFrom(t, srv, "amd64", dir)
	if err == nil {
		t.Fatal("a layer whose content does not match its digest must be rejected")
	}
	if !strings.Contains(err.Error(), "digest mismatch") {
		t.Errorf("error = %v, want a digest mismatch", err)
	}
}

func TestIndexIsResolvedToThePlatform(t *testing.T) {
	// The step naive pullers skip, and the reason they fail on arm64.
	body := gzipTar(t, func(tw *tar.Writer) { writeTarFile(t, tw, "marker", "ok") })
	srv := fakeRegistry(t, []layer{{digest: digestOf(body), body: body}}, true)

	for _, arch := range []string{"amd64", "arm64"} {
		dir := t.TempDir()
		if err := pullFrom(t, srv, arch, dir); err != nil {
			t.Errorf("pull for %s: %v", arch, err)
			continue
		}
		if readFile(t, filepath.Join(dir, "marker")) != "ok" {
			t.Errorf("%s: layer was not extracted", arch)
		}
	}
}

func TestMissingPlatformIsAClearError(t *testing.T) {
	body := gzipTar(t, func(tw *tar.Writer) { writeTarFile(t, tw, "a", "a") })
	srv := fakeRegistry(t, []layer{{digest: digestOf(body), body: body}}, true)

	dir := t.TempDir()
	err := pullFrom(t, srv, "riscv64", dir)
	if err == nil {
		t.Fatal("an unavailable platform should be an error")
	}
	// The error should list what *is* available, or the user has to go
	// looking.
	if !strings.Contains(err.Error(), "available") {
		t.Errorf("error should list available platforms, got %v", err)
	}
}

func TestPullIsCachedBySecondCall(t *testing.T) {
	body := gzipTar(t, func(tw *tar.Writer) { writeTarFile(t, tw, "a", "a") })
	srv := fakeRegistry(t, []layer{{digest: digestOf(body), body: body}}, false)

	dir := t.TempDir()
	if err := pullFrom(t, srv, "amd64", dir); err != nil {
		t.Fatalf("first pull: %v", err)
	}
	// Re-pulling must be a no-op: the alternative is downloading the image
	// every time a container starts.
	srv.Close()
	if err := pullFrom(t, srv, "amd64", dir); err != nil {
		t.Errorf("second pull should have been served from cache, got %v", err)
	}
}

func writeTarFile(t *testing.T, tw *tar.Writer, name, body string) {
	t.Helper()
	if err := tw.WriteHeader(&tar.Header{
		Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}
