package revision

// block.go: CAS block encoding — zstd compression of ORIGINAL file content
// (ACCEPTED RULING: the sha256 in paths and manifests addresses the
// ORIGINAL uncompressed bytes; the block body is the compressed form). The
// Core reader transparently accepts both forms; the Daemon always writes
// compressed.

import (
	"bytes"
	"errors"

	"github.com/klauspost/compress/zstd"
)

// zstdMagic is the frame header the Core decoder sniffs on.
var zstdMagic = []byte{0x28, 0xb5, 0x2f, 0xfd}

// EncodeBlock compresses content with zstd. The CAS path/digest still
// addresses the ORIGINAL bytes — call DigestHex(content) for the name, and
// upload EncodeBlock(content) as the body.
func EncodeBlock(content []byte) ([]byte, error) {
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		return nil, err
	}
	defer enc.Close()
	return enc.EncodeAll(content, nil), nil
}

// DecodeBlock reverses EncodeBlock; a non-zstd payload (injected by tests /
// older writers) is returned raw — mirroring the Core decoder exactly.
func DecodeBlock(block []byte) ([]byte, error) {
	if len(block) < 4 || !bytes.Equal(block[:4], zstdMagic) {
		return block, nil
	}
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	return dec.DecodeAll(block, nil)
}

// ErrDigestMismatch reports block-content corruption.
var ErrDigestMismatch = errors.New("revision: block content does not match its sha256")

// VerifyBlockDigest checks sha256(original) == shaHex for either wire form.
func VerifyBlockDigest(block []byte, shaHex string) error {
	content, err := DecodeBlock(block)
	if err != nil {
		return err
	}
	if DigestHex(content) != shaHex {
		return ErrDigestMismatch
	}
	return nil
}
