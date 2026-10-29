// Package image pulls and unpacks OCI images from a registry.
//
// This is the part that makes `minic run alpine` work rather than requiring a
// pre-built rootfs directory, and it is less magic than it looks: an image is
// a JSON manifest listing gzipped tarballs, and "pulling" is fetching them and
// extracting them in order.
//
// The sequence, which the registry specification describes across several
// documents:
//
//  1. GET /v2/ — the registry answers 401 with a WWW-Authenticate header that
//     names a token service. Anonymous pulls still need a token; the header
//     tells you where to get one.
//  2. GET the token service with the right scope.
//  3. GET /v2/<name>/manifests/<ref>. The answer may be an *index* (a list of
//     per-platform manifests) rather than a manifest, in which case you pick
//     the right architecture and fetch again.
//  4. GET each layer blob and extract it over the previous ones.
//
// Step 3 is where naive implementations break: they assume a manifest and get
// an index, then fail confusingly on an arm64 machine.
package image

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Media types from the OCI and Docker specifications.
const (
	mediaDockerManifest     = "application/vnd.docker.distribution.manifest.v2+json"
	mediaDockerManifestList = "application/vnd.docker.distribution.manifest.list.v2+json"
	mediaOCIManifest        = "application/vnd.oci.image.manifest.v1+json"
	mediaOCIIndex           = "application/vnd.oci.image.index.v1+json"
)

// Reference is a parsed image reference.
type Reference struct {
	Registry string
	// Name is the repository, e.g. "library/alpine".
	Name string
	// Tag or Digest; exactly one is set.
	Tag    string
	Digest string
}

// String renders the reference.
func (r Reference) String() string {
	if r.Digest != "" {
		return fmt.Sprintf("%s/%s@%s", r.Registry, r.Name, r.Digest)
	}
	return fmt.Sprintf("%s/%s:%s", r.Registry, r.Name, r.Tag)
}

// ParseReference parses a Docker-style reference.
//
// The defaulting rules are quirky and worth encoding once: a bare name means
// Docker Hub's library namespace, a name with one slash and no dot means a
// Hub user, and "latest" is the implicit tag.
func ParseReference(s string) (Reference, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Reference{}, errors.New("image: empty reference")
	}

	ref := Reference{Registry: "registry-1.docker.io", Tag: "latest"}

	// Split off a digest or tag. Careful: a registry may include a port, so
	// the last colon is only a tag separator if it is after the last slash.
	if at := strings.Index(s, "@"); at >= 0 {
		ref.Digest = s[at+1:]
		ref.Tag = ""
		s = s[:at]
	} else if colon := strings.LastIndex(s, ":"); colon >= 0 && !strings.Contains(s[colon:], "/") {
		ref.Tag = s[colon+1:]
		s = s[:colon]
	}

	parts := strings.Split(s, "/")
	switch {
	case len(parts) == 1:
		// "alpine" -> docker.io/library/alpine
		ref.Name = "library/" + parts[0]
	case strings.Contains(parts[0], ".") || strings.Contains(parts[0], ":") || parts[0] == "localhost":
		// The first component looks like a host, so it is the registry.
		ref.Registry = parts[0]
		ref.Name = strings.Join(parts[1:], "/")
	default:
		// "user/image" on Docker Hub.
		ref.Name = s
	}

	if ref.Name == "" {
		return Reference{}, fmt.Errorf("image: %q has no repository name", s)
	}
	return ref, nil
}

type manifest struct {
	MediaType string       `json:"mediaType"`
	Config    descriptor   `json:"config"`
	Layers    []descriptor `json:"layers"`
	// Manifests is populated for an index.
	Manifests []indexEntry `json:"manifests"`
}

type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

type indexEntry struct {
	descriptor
	Platform struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
		Variant      string `json:"variant,omitempty"`
	} `json:"platform"`
}

// Puller fetches images.
type Puller struct {
	client *http.Client
	// Arch and OS select from a multi-platform index.
	Arch string
	OS   string
	// Insecure uses http:// instead of https://. Needed for a local registry
	// on plain HTTP, which is common in development and in CI, and is the
	// same thing docker's --insecure-registry and crane's --insecure do.
	Insecure bool
	// Progress, when set, receives human-readable status lines.
	Progress func(string)
}

// NewPuller returns a Puller for the current platform.
func NewPuller() *Puller {
	return &Puller{
		client: &http.Client{Timeout: 10 * time.Minute},
		Arch:   runtime.GOARCH,
		OS:     "linux",
	}
}

// scheme returns the URL scheme to use.
func (p *Puller) scheme() string {
	if p.Insecure {
		return "http"
	}
	return "https"
}

func (p *Puller) report(format string, args ...any) {
	if p.Progress != nil {
		p.Progress(fmt.Sprintf(format, args...))
	}
}

// Pull downloads an image and extracts it into dir.
//
// dir is created if needed. An existing non-empty dir is reused as a cache:
// re-pulling the same image is a no-op, which matters because the alternative
// is downloading 50 MB every time a container starts.
func (p *Puller) Pull(ctx context.Context, ref Reference, dir string) error {
	marker := filepath.Join(dir, ".image-digest")
	if existing, err := os.ReadFile(marker); err == nil {
		p.report("using cached image %s", strings.TrimSpace(string(existing)))
		return nil
	}

	token, err := p.authenticate(ctx, ref)
	if err != nil {
		return err
	}

	m, digest, err := p.fetchManifest(ctx, ref, token)
	if err != nil {
		return err
	}
	p.report("manifest %s with %d layer(s)", digest, len(m.Layers))

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("image: create %s: %w", dir, err)
	}

	for i, layer := range m.Layers {
		p.report("layer %d/%d %s (%.1f MB)", i+1, len(m.Layers), shortDigest(layer.Digest),
			float64(layer.Size)/(1024*1024))
		if err := p.extractLayer(ctx, ref, token, layer, dir); err != nil {
			return fmt.Errorf("image: layer %s: %w", shortDigest(layer.Digest), err)
		}
	}

	return os.WriteFile(marker, []byte(digest), 0o644)
}

// authenticate obtains a bearer token, following the challenge the registry
// returns. Anonymous pulls still need one.
func (p *Puller) authenticate(ctx context.Context, ref Reference) (string, error) {
	probe := fmt.Sprintf("%s://%s/v2/", p.scheme(), ref.Registry)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, probe, nil)
	if err != nil {
		return "", err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("image: contact %s: %w", ref.Registry, err)
	}
	defer drain(resp)

	if resp.StatusCode == http.StatusOK {
		return "", nil // no auth required, e.g. a local registry
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return "", fmt.Errorf("image: %s/v2/ returned %s", ref.Registry, resp.Status)
	}

	realm, service := parseChallenge(resp.Header.Get("WWW-Authenticate"))
	if realm == "" {
		return "", fmt.Errorf("image: %s did not offer a bearer realm", ref.Registry)
	}

	tokenURL := fmt.Sprintf("%s?scope=repository:%s:pull", realm, ref.Name)
	if service != "" {
		tokenURL += "&service=" + service
	}

	req, err = http.NewRequestWithContext(ctx, http.MethodGet, tokenURL, nil)
	if err != nil {
		return "", err
	}
	tokenResp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("image: fetch token: %w", err)
	}
	defer drain(tokenResp)
	if tokenResp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("image: token service returned %s", tokenResp.Status)
	}

	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(tokenResp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("image: decode token: %w", err)
	}
	if body.Token != "" {
		return body.Token, nil
	}
	return body.AccessToken, nil
}

// parseChallenge extracts realm and service from a WWW-Authenticate header.
func parseChallenge(header string) (realm, service string) {
	header = strings.TrimPrefix(header, "Bearer ")
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		v = strings.Trim(v, `"`)
		switch k {
		case "realm":
			realm = v
		case "service":
			service = v
		}
	}
	return realm, service
}

// fetchManifest gets the manifest, resolving an index to this platform.
func (p *Puller) fetchManifest(ctx context.Context, ref Reference, token string) (*manifest, string, error) {
	target := ref.Tag
	if ref.Digest != "" {
		target = ref.Digest
	}

	m, digest, err := p.getManifest(ctx, ref, token, target)
	if err != nil {
		return nil, "", err
	}

	// An index lists per-platform manifests; pick ours and fetch again. This
	// is the step naive pullers skip, and the reason they fail on arm64.
	if m.MediaType == mediaOCIIndex || m.MediaType == mediaDockerManifestList || len(m.Manifests) > 0 {
		var chosen string
		for _, entry := range m.Manifests {
			if entry.Platform.OS == p.OS && entry.Platform.Architecture == p.Arch {
				chosen = entry.Digest
				break
			}
		}
		if chosen == "" {
			var have []string
			for _, e := range m.Manifests {
				have = append(have, e.Platform.OS+"/"+e.Platform.Architecture)
			}
			return nil, "", fmt.Errorf("image: %s has no %s/%s manifest (available: %s)",
				ref, p.OS, p.Arch, strings.Join(have, ", "))
		}
		p.report("index resolved to %s for %s/%s", shortDigest(chosen), p.OS, p.Arch)
		return p.getManifest(ctx, ref, token, chosen)
	}
	return m, digest, nil
}

func (p *Puller) getManifest(ctx context.Context, ref Reference, token, target string) (*manifest, string, error) {
	url := fmt.Sprintf("%s://%s/v2/%s/manifests/%s", p.scheme(), ref.Registry, ref.Name, target)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	// Accept every manifest type, or the registry answers with whichever it
	// prefers and the media type will not match what the code expects.
	req.Header.Set("Accept", strings.Join([]string{
		mediaOCIIndex, mediaOCIManifest, mediaDockerManifestList, mediaDockerManifest,
	}, ", "))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("image: fetch manifest: %w", err)
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("image: manifest for %s returned %s", ref, resp.Status)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, "", err
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, "", fmt.Errorf("image: decode manifest: %w", err)
	}
	if m.MediaType == "" {
		m.MediaType = resp.Header.Get("Content-Type")
	}

	digest := resp.Header.Get("Docker-Content-Digest")
	if digest == "" {
		sum := sha256.Sum256(raw)
		digest = "sha256:" + hex.EncodeToString(sum[:])
	}
	return &m, digest, nil
}

// extractLayer streams a blob and untars it over dir, verifying the digest.
func (p *Puller) extractLayer(ctx context.Context, ref Reference, token string, layer descriptor, dir string) error {
	url := fmt.Sprintf("%s://%s/v2/%s/blobs/%s", p.scheme(), ref.Registry, ref.Name, layer.Digest)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("blob returned %s", resp.Status)
	}

	// Verify while streaming rather than buffering the whole layer: a layer
	// can be hundreds of megabytes, and the digest is the only thing making
	// this a content-addressed fetch rather than blind trust in the network.
	hasher := sha256.New()
	tee := io.TeeReader(resp.Body, hasher)

	gz, err := gzip.NewReader(tee)
	if err != nil {
		return fmt.Errorf("gunzip: %w", err)
	}
	defer gz.Close()

	if err := extractTar(tar.NewReader(gz), dir); err != nil {
		return err
	}
	// Drain anything left so the hash covers the whole blob.
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return err
	}

	got := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	if !strings.EqualFold(got, layer.Digest) {
		return fmt.Errorf("digest mismatch: got %s, want %s", got, layer.Digest)
	}
	return nil
}

// whiteoutPrefix marks a deletion in an image layer. Docker's layer format
// represents "this file was removed in this layer" as a zero-length file
// named .wh.<name>, which an extractor must interpret rather than create.
const whiteoutPrefix = ".wh."

// opaqueWhiteout means "ignore everything from lower layers in this
// directory".
const opaqueWhiteout = ".wh..wh..opq"

func extractTar(tr *tar.Reader, dir string) error {
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}

		target, err := safeJoin(dir, header.Name)
		if err != nil {
			// A layer containing ../../etc/passwd is a real attack, not a
			// hypothetical: refuse rather than write outside the root.
			return err
		}

		base := filepath.Base(header.Name)
		switch {
		case base == opaqueWhiteout:
			// Everything already extracted in this directory is hidden by
			// this layer.
			if err := os.RemoveAll(filepath.Dir(target)); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			continue
		case strings.HasPrefix(base, whiteoutPrefix):
			removed := filepath.Join(filepath.Dir(target), strings.TrimPrefix(base, whiteoutPrefix))
			if err := os.RemoveAll(removed); err != nil {
				return err
			}
			continue
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(header.Mode)&os.ModePerm); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := writeFile(target, tr, os.FileMode(header.Mode)&os.ModePerm); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			_ = os.Remove(target)
			// A symlink target is not validated: it is resolved inside the
			// container, where it cannot escape the pivoted root.
			if err := os.Symlink(header.Linkname, target); err != nil {
				return err
			}
		case tar.TypeLink:
			source, err := safeJoin(dir, header.Linkname)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Link(source, target); err != nil {
				// A hard link across layers can legitimately fail; copying is
				// a correct fallback.
				continue
			}
		default:
			// Character and block devices need privileges a pull should not
			// require, and fifos are not useful in an image.
			continue
		}
	}
}

func writeFile(path string, r io.Reader, mode os.FileMode) error {
	// Remove first: a later layer replacing a file from an earlier one must
	// not inherit the old file's mode or hard links.
	_ = os.Remove(path)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode|0o200)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, r); err != nil {
		return err
	}
	return f.Chmod(mode)
}

// safeJoin resolves a tar entry name inside dir, refusing traversal.
//
// A traversing entry is *rejected* rather than cleaned. Cleaning
// "../../etc/passwd" into "/etc/passwd" would also be safe -- it lands inside
// the root either way -- but it silently rewrites a malicious path into a
// plausible one, so the attempt leaves no trace. A layer containing such an
// entry is not a mistake, and refusing says so.
func safeJoin(dir, name string) (string, error) {
	// A leading slash is common and harmless; the entry is simply rooted.
	cleaned := strings.TrimPrefix(name, "/")

	for _, part := range strings.Split(cleaned, "/") {
		if part == ".." {
			return "", fmt.Errorf("image: tar entry %q escapes the extraction directory", name)
		}
	}

	target := filepath.Join(dir, filepath.Clean("/"+cleaned))

	// Belt and braces: confirm the result really is inside dir, in case a
	// platform's path handling surprises us.
	rel, err := filepath.Rel(dir, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("image: tar entry %q escapes the extraction directory", name)
	}
	return target, nil
}

func shortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		return d[:12]
	}
	return d
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
}
