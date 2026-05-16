package content

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/crane"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// ErrInvalidDigest indicates that an identifier shaped like a digest
// (e.g. "sha256:...") does not match the expected format. Rejecting these
// prevents path-traversal when the digest is used as a filename component.
var ErrInvalidDigest = errors.New("invalid artifact digest")

// sha256DigestRe matches a well-formed sha256 digest: 64 lowercase hex chars.
var sha256DigestRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// validateDigest returns nil if digest is a well-formed sha256 digest,
// or a wrapped ErrInvalidDigest otherwise.
func validateDigest(digest string) error {
	if !sha256DigestRe.MatchString(digest) {
		return fmt.Errorf("%w: %q", ErrInvalidDigest, digest)
	}
	return nil
}

// ErrStoreCorrupted indicates that the local artifact store is in an
// inconsistent or partially missing state (e.g. missing tar, refs or metadata).
// Callers may safely attempt to re-fetch the artifact from the remote source.
var ErrStoreCorrupted = errors.New("local artifact store corrupted")

// Store manages the local content store for artifacts
type Store struct {
	baseDir string
}

// ArtifactMetadata contains metadata about stored artifacts
type ArtifactMetadata struct {
	Digest      string            `json:"digest"`
	Reference   string            `json:"reference"`
	Size        int64             `json:"size"`
	StoredAt    time.Time         `json:"stored_at"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type Opt func(*Store)

func WithBaseDir(baseDir string) Opt {
	return func(s *Store) {
		s.baseDir = baseDir
	}
}

// NewStore creates a new content store
func NewStore(opts ...Opt) (*Store, error) {
	store := &Store{}

	for _, opt := range opts {
		opt(store)
	}

	if store.baseDir == "" {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("getting home directory: %w", err)
		}

		store.baseDir = filepath.Join(homeDir, ".cagent", "store")
	}

	if err := os.MkdirAll(store.baseDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating store directory: %w", err)
	}

	return store, nil
}

// StoreArtifact stores an artifact with the given reference and returns its digest
func (s *Store) StoreArtifact(img v1.Image, reference string) (string, error) {
	digest, err := img.Digest()
	if err != nil {
		return "", fmt.Errorf("calculating digest: %w", err)
	}

	digestStr := digest.String()

	// Validate the digest before using it in filesystem paths (defense-in-depth).
	if err := validateDigest(digestStr); err != nil {
		return "", err
	}

	tarPath := filepath.Join(s.baseDir, digestStr+".tar")

	if err := crane.Save(img, reference, tarPath); err != nil {
		return "", fmt.Errorf("saving image to tar: %w", err)
	}

	stat, err := os.Stat(tarPath)
	if err != nil {
		return "", fmt.Errorf("getting file stats: %w", err)
	}

	manifest, err := img.Manifest()
	if err != nil {
		return "", fmt.Errorf("getting manifest: %w", err)
	}

	// Create metadata
	metadata := ArtifactMetadata{
		Digest:      digestStr,
		Reference:   reference,
		Size:        stat.Size(),
		StoredAt:    time.Now(),
		Annotations: manifest.Annotations,
	}

	if err := s.saveMetadata(digestStr, &metadata); err != nil {
		return "", fmt.Errorf("saving metadata: %w", err)
	}

	if err := s.createReferenceLink(reference, digestStr); err != nil {
		return "", fmt.Errorf("creating reference link: %w", err)
	}

	return digestStr, nil
}

// GetArtifactImage loads an artifact by digest or reference and returns it as a v1.Image
func (s *Store) GetArtifactImage(identifier string) (v1.Image, error) {
	// Resolve the identifier (reference or digest) to a content digest.
	// Any failure here means the local store is incomplete or inconsistent.
	digest, err := s.resolveIdentifier(identifier)
	if err != nil {
		return nil, err
	}

	// Artifacts are stored locally as tarballs named by their digest.
	artifactPath := filepath.Join(s.baseDir, digest+".tar")

	// If the tarball is missing, the local store is considered corrupted.
	// Callers can safely attempt to re-fetch the artifact from the remote source.
	if _, err := os.Stat(artifactPath); err != nil {
		if os.IsNotExist(err) {
			return nil, ErrStoreCorrupted
		}
		return nil, fmt.Errorf("checking artifact file: %w", err)
	}

	// Load the OCI image from the local tarball.
	// Any failure at this stage indicates a partially written or corrupted artifact.
	img, err := tarball.ImageFromPath(artifactPath, nil)
	if err != nil {
		return nil, ErrStoreCorrupted
	}

	return img, nil
}

// GetArtifactPath returns the file path for an artifact by digest or reference
func (s *Store) GetArtifactPath(identifier string) (string, error) {
	digest, err := s.resolveIdentifier(identifier)
	if err != nil {
		return "", err
	}

	artifactPath := filepath.Join(s.baseDir, digest+".tar")

	if _, err := os.Stat(artifactPath); err != nil {
		if os.IsNotExist(err) {
			return "", ErrStoreCorrupted
		}
		return "", err
	}

	return artifactPath, nil
}

// GetArtifactMetadata returns metadata for an artifact by digest or reference
func (s *Store) GetArtifactMetadata(identifier string) (*ArtifactMetadata, error) {
	digest, err := s.resolveIdentifier(identifier)
	if err != nil {
		return nil, err
	}

	return s.loadMetadata(digest)
}

func (s *Store) GetArtifact(identifier string) (string, error) {
	// Load the artifact image from the local store.
	// Any error here is propagated so callers can decide whether to re-fetch.
	img, err := s.GetArtifactImage(identifier)
	if err != nil {
		return "", err
	}

	// Extract layers from the OCI image.
	// A failure indicates an invalid or partially written image.
	layers, err := img.Layers()
	if err != nil {
		return "", ErrStoreCorrupted
	}

	// An artifact without layers is considered invalid.
	// This should never happen for a correctly stored agent.
	if len(layers) == 0 {
		return "", ErrStoreCorrupted
	}

	// Agents are expected to be stored in the first layer.
	// If decompression fails, the local store is considered corrupted.
	layer := layers[0]
	rc, err := layer.Uncompressed()
	if err != nil {
		return "", ErrStoreCorrupted
	}
	defer rc.Close()

	// Read the full layer content into memory.
	// Any I/O error here means the artifact cannot be trusted.
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, rc); err != nil {
		return "", ErrStoreCorrupted
	}

	return buf.String(), nil
}

// ListArtifacts returns a list of all stored artifacts
func (s *Store) ListArtifacts() ([]ArtifactMetadata, error) {
	files, err := os.ReadDir(s.baseDir)
	if err != nil {
		return nil, fmt.Errorf("reading store directory: %w", err)
	}

	var artifacts []ArtifactMetadata
	for _, file := range files {
		if !file.IsDir() && strings.HasSuffix(file.Name(), ".tar") {
			digest := strings.TrimSuffix(file.Name(), ".tar")
			metadata, err := s.loadMetadata(digest)
			if err != nil {
				continue
			}
			artifacts = append(artifacts, *metadata)
		}
	}

	return artifacts, nil
}

// DeleteArtifact removes an artifact from the store
func (s *Store) DeleteArtifact(identifier string) error {
	digest, err := s.resolveIdentifier(identifier)
	if err != nil {
		return err
	}

	tarPath := filepath.Join(s.baseDir, digest+".tar")
	if err := os.Remove(tarPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing tar file: %w", err)
	}

	metadataPath := filepath.Join(s.baseDir, digest+".json")
	if err := os.Remove(metadataPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing metadata file: %w", err)
	}

	refsDir := filepath.Join(s.baseDir, "refs")
	if _, err := os.Stat(refsDir); err == nil {
		if err := s.removeReferenceLinks(digest); err != nil {
			return fmt.Errorf("removing reference links: %w", err)
		}
	}

	return nil
}

// resolveIdentifier resolves a user-provided identifier (digest or reference)
// into a concrete content digest stored in the local artifact store.
//
// The returned digest is always strictly validated ("sha256:" + 64 hex chars)
// so it can be safely used as a filename component without enabling path
// traversal.
func (s *Store) resolveIdentifier(identifier string) (string, error) {
	// Bare digest, e.g. "sha256:abc123...".
	if strings.HasPrefix(identifier, "sha256:") {
		if err := validateDigest(identifier); err != nil {
			return "", err
		}
		return identifier, nil
	}

	// Digest reference, e.g. "repo@sha256:abc123...".
	if i := strings.LastIndex(identifier, "@sha256:"); i >= 0 {
		digest := identifier[i+1:]
		if err := validateDigest(digest); err != nil {
			return "", err
		}
		return digest, nil
	}

	// Tagged or tag-less reference. Default to ":latest" per OCI semantics.
	if !strings.Contains(identifier, ":") {
		identifier += ":latest"
	}
	return s.resolveReference(identifier)
}

// resolveReference resolves an OCI reference (e.g. repo:tag)
// to a concrete digest using the local refs index.
func (s *Store) resolveReference(reference string) (string, error) {
	refsDir := filepath.Join(s.baseDir, "refs")

	// References are mapped to digests using a stable hash of the reference string.
	// This avoids filesystem issues with slashes, colons, etc.
	refHash := sha256.Sum256([]byte(reference))
	refFile := filepath.Join(refsDir, hex.EncodeToString(refHash[:]))

	// Read the stored digest for this reference.
	// If the file is missing, the local store is considered corrupted.
	data, err := os.ReadFile(refFile)
	if err != nil {
		if os.IsNotExist(err) {
			// This is the exact failure mode reported in the issue:
			// the tar/metadata may exist, but the reference index is missing.
			return "", ErrStoreCorrupted
		}
		return "", fmt.Errorf("reading reference file: %w", err)
	}

	// The file content is expected to be the digest string. Refs files are
	// generated by us, but we validate defense-in-depth in case the refs
	// directory is ever tampered with.
	digest := strings.TrimSpace(string(data))
	if err := validateDigest(digest); err != nil {
		return "", fmt.Errorf("ref %q: %w", reference, err)
	}
	return digest, nil
}

// createReferenceLink creates a link from reference to digest
func (s *Store) createReferenceLink(reference, digest string) error {
	refsDir := filepath.Join(s.baseDir, "refs")
	if err := os.MkdirAll(refsDir, 0o700); err != nil {
		return fmt.Errorf("creating refs directory: %w", err)
	}

	refHash := sha256.Sum256([]byte(reference))
	refFile := filepath.Join(refsDir, hex.EncodeToString(refHash[:]))

	return os.WriteFile(refFile, []byte(digest), 0o600)
}

// removeReferenceLinks removes all reference links pointing to the given digest
func (s *Store) removeReferenceLinks(digest string) error {
	refsDir := filepath.Join(s.baseDir, "refs")
	files, err := os.ReadDir(refsDir)
	if err != nil {
		return err
	}

	for _, file := range files {
		if file.IsDir() {
			continue
		}

		refFile := filepath.Join(refsDir, file.Name())
		data, err := os.ReadFile(refFile)
		if err != nil {
			continue
		}

		if strings.TrimSpace(string(data)) == digest {
			os.Remove(refFile)
		}
	}

	return nil
}

// saveMetadata saves metadata for an artifact
func (s *Store) saveMetadata(digest string, metadata *ArtifactMetadata) error {
	metadataPath := filepath.Join(s.baseDir, digest+".json")
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling metadata: %w", err)
	}

	return os.WriteFile(metadataPath, data, 0o600)
}

// loadMetadata loads metadata for an artifact
func (s *Store) loadMetadata(digest string) (*ArtifactMetadata, error) {
	metadataPath := filepath.Join(s.baseDir, digest+".json")
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrStoreCorrupted
		}
		return nil, fmt.Errorf("reading metadata file: %w", err)
	}

	var metadata ArtifactMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return nil, fmt.Errorf("unmarshaling metadata: %w", err)
	}

	return &metadata, nil
}
