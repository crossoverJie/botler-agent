// Package images persists inbound (received) images under the target data subproject.
//
// The written path is validated by paths.SafePath (first-level allowlist) plus an
// image-extension whitelist, so an image can never escape DATA_ROOT or be written with a
// non-image extension. The inbound whitelist deliberately excludes bmp (v1 parity).
package images

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"

	"github.com/crossoverJie/botler-agent/internal/agentrt"
	"github.com/crossoverJie/botler-agent/internal/paths"
	"github.com/crossoverJie/botler-agent/internal/prompt"
)

// photosSubdir is where inbound originals land inside a project.
const photosSubdir = "photos"

var inboundExtOK = map[string]bool{"jpg": true, "png": true, "gif": true, "webp": true}

// SniffMime sniffs the image format from magic bytes, defaulting to JPEG when unrecognized.
func SniffMime(buf []byte) (mimeType, ext string) {
	switch {
	case len(buf) >= 8 && string(buf[:8]) == "\x89PNG\r\n\x1a\n":
		return "image/png", "png"
	case len(buf) >= 3 && buf[0] == 0xff && buf[1] == 0xd8 && buf[2] == 0xff:
		return "image/jpeg", "jpg"
	case len(buf) >= 4 && string(buf[:4]) == "GIF8":
		return "image/gif", "gif"
	case len(buf) >= 12 && string(buf[:4]) == "RIFF" && string(buf[8:12]) == "WEBP":
		return "image/webp", "webp"
	}
	return "image/jpeg", "jpg"
}

// PersistInboundImage writes the decoded bytes under DATA_ROOT/<project>/photos/<date>-<rand>.<ext>
// and returns the RELATIVE path (e.g. cook/photos/2026-08-23-ab12cd.jpg) for the agent to cite.
func PersistInboundImage(img agentrt.InboundImage, project string) (string, error) {
	_, ext := SniffMime(img.Data)
	if !inboundExtOK[ext] {
		ext = "jpg"
	}
	var raw [4]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	name := prompt.Today() + "-" + hex.EncodeToString(raw[:]) + "." + ext
	// filepath.Join is safe here: project comes from the routing decision over real subdir names.
	rel := filepath.Join(project, photosSubdir, name)
	abs, err := paths.SafePath(rel, paths.Options{})
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(abs, img.Data, 0o644); err != nil {
		return "", err
	}
	return rel, nil
}
