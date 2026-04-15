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

func isInternalRegistry(host string) bool {
	return strings.HasPrefix(host, "registry:") || strings.HasPrefix(host, "localhost:")
}
