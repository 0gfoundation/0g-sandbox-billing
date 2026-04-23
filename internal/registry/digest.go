package registry

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
)

// GetDigest fetches the content digest (e.g. "sha256:abc...") for imageRef.
//
// Registries whose host starts with "registry:" or "localhost:" are contacted
// over plain HTTP (matching the internal registry:6000 setup). All others use
// the default HTTPS transport.
//
// For sealed containers this must succeed before sandbox creation proceeds;
// callers should treat an error as a hard failure.
func GetDigest(ctx context.Context, imageRef string) (string, error) {
	opts := []crane.Option{
		crane.WithContext(ctx),
		crane.WithAuth(authn.Anonymous),
	}

	// name.Insecure is needed for ParseReference when the registry host
	// does not have a TLD (e.g. "registry:6000/...").
	ref, err := name.ParseReference(imageRef, name.Insecure)
	if err != nil {
		return "", fmt.Errorf("parse image ref %q: %w", imageRef, err)
	}

	if isInternalRegistry(ref.Context().RegistryStr()) {
		opts = append(opts, crane.Insecure)
	}

	digest, err := crane.Digest(imageRef, opts...)
	if err != nil {
		return "", fmt.Errorf("get digest for %q: %w", imageRef, err)
	}
	return digest, nil
}

// TagByDigest resolves imageRef to its content digest and creates a derived
// tag "<repo>:d-<first16hex-of-digest>" in the same registry pointing at the
// same manifest. Returns the full derived-tag reference.
//
// Snapshot registration uses this to give Daytona a stable tag-form imageName
// whose tag rotates whenever the source content changes: re-pushing the same
// base tag with new content yields a new digest, therefore a new derived tag,
// therefore a fresh Daytona-side cache key on the runner — avoiding stale
// wrapped-image cache hits.
//
// Digest-form refs ("<repo>@sha256:...") would be more natural but Daytona
// rejects them ("invalid reference format"), so we stay in tag-space.
func TagByDigest(ctx context.Context, imageRef string) (string, error) {
	digest, err := GetDigest(ctx, imageRef)
	if err != nil {
		return "", err
	}
	ref, err := name.ParseReference(imageRef, name.Insecure)
	if err != nil {
		return "", fmt.Errorf("parse image ref %q: %w", imageRef, err)
	}
	short := strings.TrimPrefix(digest, "sha256:")
	if len(short) < 16 {
		return "", fmt.Errorf("digest too short: %q", digest)
	}
	newTag := "d-" + short[:16]
	newRef := ref.Context().Name() + ":" + newTag

	opts := []crane.Option{crane.WithContext(ctx), crane.WithAuth(authn.Anonymous)}
	if isInternalRegistry(ref.Context().RegistryStr()) {
		opts = append(opts, crane.Insecure)
	}
	if err := crane.Tag(imageRef, newTag, opts...); err != nil {
		return "", fmt.Errorf("tag %q as %q: %w", imageRef, newRef, err)
	}
	return newRef, nil
}

func isInternalRegistry(host string) bool {
	return strings.HasPrefix(host, "registry:") || strings.HasPrefix(host, "localhost:")
}
