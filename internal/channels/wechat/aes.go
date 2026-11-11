package wechat

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"regexp"

	ilink "github.com/openilink/openilink-sdk-go"
)

var hexKeyRe = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)

// ParseInboundAESKey resolves an inbound image AES key to its 16 raw bytes.
// Images carry the key as hex (image_item.aeskey, preferred) OR base64
// (image_item.media.aes_key). Two base64 encodings are seen in the wild: the raw 16 bytes, or
// base64(hex-string-of-16-bytes). The parser tolerates either encoding in either field.
//
// The SDK's ParseAESKey only handles base64, so a hex image_item.aeskey (the preferred field)
// would fail there; this is the v1 parser, ported as-is.
func ParseInboundAESKey(hexKey, base64Key string) ([]byte, error) {
	key := hexKey
	field := "image_item.aeskey"
	if key == "" {
		key = base64Key
		field = "image_item.media.aes_key"
	}
	if key == "" {
		return nil, fmt.Errorf("image has no aes key")
	}
	return keyFromString(key, field)
}

func keyFromString(key, field string) ([]byte, error) {
	if hexKeyRe.MatchString(key) {
		return hex.DecodeString(key)
	}
	decoded, err := base64.StdEncoding.DecodeString(key)
	if err == nil {
		if len(decoded) == 16 {
			return decoded, nil
		}
		if len(decoded) == 32 && hexKeyRe.MatchString(string(decoded)) {
			return hex.DecodeString(string(decoded))
		}
	}
	return nil, fmt.Errorf("%s must be a 32-char hex key or base64 (16 raw bytes / 32-char hex)", field)
}

// decryptAESECB decrypts AES-128-ECB ciphertext with PKCS#7 padding (delegated to the SDK).
func decryptAESECB(ciphertext, key []byte) ([]byte, error) {
	return ilink.DecryptAESECB(ciphertext, key)
}
