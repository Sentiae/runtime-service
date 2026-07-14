// Package oci implements a minimal OCI Distribution pull client and an
// ext4 materializer for the runtime-fleet image-boot path (CP3). It has no
// containerd/docker dependency — plain net/http against the platform registry
// (vcs OCI-on-CAS, D-016) — because the runtime host only needs to *pull* a
// compiled image and lay it down as a Firecracker rootfs, never build or push.
package oci

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Media types the client accepts / understands.
const (
	mediaTypeOCIManifest    = "application/vnd.oci.image.manifest.v1+json"
	mediaTypeDockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
	mediaTypeOCIIndex       = "application/vnd.oci.image.index.v1+json"
	mediaTypeDockerList     = "application/vnd.docker.distribution.manifest.list.v2+json"

	manifestAccept = mediaTypeOCIManifest + ", " + mediaTypeDockerManifest + ", " +
		mediaTypeOCIIndex + ", " + mediaTypeDockerList
)

// Config configures the registry pull client.
type Config struct {
	// Host is the registry host:port, reached over plain HTTP (homelab).
	Host string
	// Username is the Basic-auth username. Defaults to "registry-client".
	Username string
	// Password is the Basic-auth password (the runtime service key).
	Password string
}

// Client is a plain-HTTP OCI Distribution pull client.
type Client struct {
	cfg  Config
	http *http.Client
}

// descriptor is an OCI content descriptor (manifest/config/layer entry).
type descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Platform    *platformSpec     `json:"platform,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type platformSpec struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
}

// imageManifest is a single-platform image manifest.
type imageManifest struct {
	MediaType string       `json:"mediaType"`
	Config    descriptor   `json:"config"`
	Layers    []descriptor `json:"layers"`
}

// imageIndex is a multi-platform manifest list / index.
type imageIndex struct {
	MediaType string       `json:"mediaType"`
	Manifests []descriptor `json:"manifests"`
}

// ImageConfig holds the runtime-relevant fields parsed out of the OCI image
// config blob.
type ImageConfig struct {
	Entrypoint []string
	Cmd        []string
	Env        []string
	WorkingDir string
}

// configBlob is the on-disk shape of an OCI image config blob.
type configBlob struct {
	Config struct {
		Entrypoint []string `json:"Entrypoint"`
		Cmd        []string `json:"Cmd"`
		Env        []string `json:"Env"`
		WorkingDir string   `json:"WorkingDir"`
	} `json:"config"`
}

// NewClient constructs a pull client.
func NewClient(cfg Config) *Client {
	if cfg.Username == "" {
		cfg.Username = "registry-client"
	}
	return &Client{
		cfg:  cfg,
		http: &http.Client{Timeout: 60 * time.Second},
	}
}

// withPassword returns a shallow clone of the client that presents pw as the
// Basic-auth password (D-124: a per-deployment registry pull token overriding the
// shared service key). The clone SHARES the underlying *http.Client (connection
// pool) and keeps the configured username; only the credential differs, so a
// concurrent pull with a different token cannot race on shared auth state. Passing
// an empty pw is never done by the caller (it uses the base client instead).
func (c *Client) withPassword(pw string) *Client {
	cfg := c.cfg
	cfg.Password = pw
	return &Client{cfg: cfg, http: c.http}
}

// resolvedManifest is the fully-resolved single-platform manifest for an image.
type resolvedManifest struct {
	config ImageConfig
	layers []descriptor
}

// FetchManifest fetches the manifest for repo@digest, following an index/list
// to the linux/amd64 entry when necessary, and returns the parsed image config
// plus the ordered layer descriptors.
func (c *Client) FetchManifest(ctx context.Context, repo, digest string) (resolvedManifest, error) {
	raw, mediaType, err := c.getManifest(ctx, repo, digest)
	if err != nil {
		return resolvedManifest{}, err
	}

	// If we got an index/list, pick the linux/amd64 entry and re-fetch.
	if isIndex(mediaType, raw) {
		var idx imageIndex
		if err := json.Unmarshal(raw, &idx); err != nil {
			return resolvedManifest{}, fmt.Errorf("decode image index: %w", err)
		}
		sel, err := selectAMD64(idx.Manifests)
		if err != nil {
			return resolvedManifest{}, err
		}
		raw, mediaType, err = c.getManifest(ctx, repo, sel.Digest)
		if err != nil {
			return resolvedManifest{}, err
		}
		if isIndex(mediaType, raw) {
			return resolvedManifest{}, fmt.Errorf("nested image index for %s@%s", repo, sel.Digest)
		}
	}

	var man imageManifest
	if err := json.Unmarshal(raw, &man); err != nil {
		return resolvedManifest{}, fmt.Errorf("decode image manifest: %w", err)
	}
	if man.Config.Digest == "" {
		return resolvedManifest{}, fmt.Errorf("manifest %s@%s has no config", repo, digest)
	}

	cfg, err := c.fetchConfig(ctx, repo, man.Config.Digest)
	if err != nil {
		return resolvedManifest{}, err
	}
	return resolvedManifest{config: cfg, layers: man.Layers}, nil
}

// FetchBlob streams a blob (layer or config) for repo@digest. The caller closes.
func (c *Client) FetchBlob(ctx context.Context, repo, digest string) (io.ReadCloser, error) {
	url := fmt.Sprintf("http://%s/v2/%s/blobs/%s", c.cfg.Host, repo, digest)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create blob request: %w", err)
	}
	c.auth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch blob %s: %w", digest, err)
	}
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("fetch blob %s: registry returned %d: %s", digest, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return resp.Body, nil
}

// getManifest fetches a raw manifest and its Content-Type.
func (c *Client) getManifest(ctx context.Context, repo, digest string) ([]byte, string, error) {
	url := fmt.Sprintf("http://%s/v2/%s/manifests/%s", c.cfg.Host, repo, digest)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", fmt.Errorf("create manifest request: %w", err)
	}
	req.Header.Set("Accept", manifestAccept)
	c.auth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("fetch manifest %s@%s: %w", repo, digest, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("read manifest %s@%s: %w", repo, digest, err)
	}
	if resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("fetch manifest %s@%s: registry returned %d: %s", repo, digest, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, resp.Header.Get("Content-Type"), nil
}

// fetchConfig fetches + parses the image config blob.
func (c *Client) fetchConfig(ctx context.Context, repo, digest string) (ImageConfig, error) {
	rc, err := c.FetchBlob(ctx, repo, digest)
	if err != nil {
		return ImageConfig{}, err
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		return ImageConfig{}, fmt.Errorf("read config blob: %w", err)
	}
	var cb configBlob
	if err := json.Unmarshal(body, &cb); err != nil {
		return ImageConfig{}, fmt.Errorf("decode config blob: %w", err)
	}
	return ImageConfig{
		Entrypoint: cb.Config.Entrypoint,
		Cmd:        cb.Config.Cmd,
		Env:        cb.Config.Env,
		WorkingDir: cb.Config.WorkingDir,
	}, nil
}

func (c *Client) auth(req *http.Request) {
	if c.cfg.Password != "" || c.cfg.Username != "" {
		req.SetBasicAuth(c.cfg.Username, c.cfg.Password)
	}
}

// isIndex reports whether the manifest is a multi-platform index/list. It
// checks the transport Content-Type first, then falls back to the mediaType
// embedded in the document (registries are inconsistent about the header).
func isIndex(contentType string, raw []byte) bool {
	ct := strings.ToLower(contentType)
	if strings.Contains(ct, "image.index") || strings.Contains(ct, "manifest.list") {
		return true
	}
	var probe struct {
		MediaType string            `json:"mediaType"`
		Manifests []json.RawMessage `json:"manifests"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return false
	}
	if probe.MediaType == mediaTypeOCIIndex || probe.MediaType == mediaTypeDockerList {
		return true
	}
	// A document with a "manifests" array and no "config" is an index.
	if len(probe.Manifests) > 0 {
		var hasConfig struct {
			Config json.RawMessage `json:"config"`
		}
		_ = json.Unmarshal(raw, &hasConfig)
		if len(hasConfig.Config) == 0 {
			return true
		}
	}
	return false
}

// selectAMD64 picks the linux/amd64 manifest from an index.
func selectAMD64(manifests []descriptor) (descriptor, error) {
	for _, m := range manifests {
		if m.Platform != nil && m.Platform.OS == "linux" && m.Platform.Architecture == "amd64" {
			return m, nil
		}
	}
	return descriptor{}, fmt.Errorf("image index has no linux/amd64 manifest")
}
