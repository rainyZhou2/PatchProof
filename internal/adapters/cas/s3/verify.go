package s3

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
)

// verifyingReader checks the streamed bytes against a content address without
// buffering the blob. A mismatch is reported from Read on the call that reaches
// EOF, and Close fails if the caller did not consume the blob completely.
type verifyingReader struct {
	source         io.ReadCloser
	digest         hash.Hash
	expectedDigest string
	expectedSize   int64
	read           int64
	verified       bool
	failure        error
	closed         bool
}

func newVerifyingReader(source io.ReadCloser, expectedDigest string, expectedSize int64) io.ReadCloser {
	return &verifyingReader{
		source:         source,
		digest:         sha256.New(),
		expectedDigest: expectedDigest,
		expectedSize:   expectedSize,
	}
}

func (v *verifyingReader) Read(p []byte) (int, error) {
	if v.failure != nil {
		return 0, v.failure
	}
	if v.closed {
		return 0, errors.New("s3 cas: read from closed blob reader")
	}
	n, err := v.source.Read(p)
	if n > 0 {
		v.read += int64(n)
		if v.read > v.expectedSize {
			return n, v.fail(fmt.Errorf("%w: content is larger than the declared %d bytes", ErrIntegrity, v.expectedSize))
		}
		if _, hashErr := v.digest.Write(p[:n]); hashErr != nil {
			return n, v.fail(fmt.Errorf("%w: hash content: %w", ErrIntegrity, hashErr))
		}
		if v.read == v.expectedSize {
			if failure := v.verifyDigest(); failure != nil {
				return n, failure
			}
		}
	}
	if err != nil {
		if !errors.Is(err, io.EOF) {
			return n, err
		}
		if v.read != v.expectedSize {
			return n, v.fail(fmt.Errorf("%w: read %d bytes, expected %d", ErrIntegrity, v.read, v.expectedSize))
		}
		if !v.verified {
			if failure := v.verifyDigest(); failure != nil {
				return n, failure
			}
		}
		return n, io.EOF
	}
	return n, nil
}

func (v *verifyingReader) verifyDigest() error {
	actual := hex.EncodeToString(v.digest.Sum(nil))
	if actual != v.expectedDigest {
		return v.fail(fmt.Errorf("%w: digest %s, expected %s", ErrIntegrity, actual, v.expectedDigest))
	}
	v.verified = true
	return nil
}

func (v *verifyingReader) Close() error {
	if v.closed {
		return nil
	}
	v.closed = true
	closeErr := v.source.Close()
	if v.failure != nil {
		return errors.Join(closeErr, v.failure)
	}
	if !v.verified {
		return errors.Join(closeErr, v.fail(fmt.Errorf("%w: blob reader closed before the full content was read", ErrIntegrity)))
	}
	return closeErr
}

func (v *verifyingReader) fail(err error) error {
	if v.failure == nil {
		v.failure = err
	}
	return v.failure
}
