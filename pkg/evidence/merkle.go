package evidence

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
)

const ManifestSchemaV1 = "patchproof.manifest/v1"

type Leaf struct {
	Path      string `json:"path"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	MediaType string `json:"media_type,omitempty"`
}

type Manifest struct {
	SchemaVersion string `json:"schema_version"`
	Root          string `json:"root"`
	Leaves        []Leaf `json:"leaves"`
}

type ProofNode struct {
	Hash string `json:"hash"`
	Left bool   `json:"left"`
}

func BuildManifest(leaves []Leaf) (Manifest, error) {
	manifest := Manifest{SchemaVersion: ManifestSchemaV1, Leaves: append([]Leaf(nil), leaves...)}
	if err := manifest.ValidateStructure(); err != nil {
		return Manifest{}, err
	}
	sort.Slice(manifest.Leaves, func(i, j int) bool { return manifest.Leaves[i].Path < manifest.Leaves[j].Path })
	root, err := rootOf(manifest.Leaves)
	if err != nil {
		return Manifest{}, err
	}
	manifest.Root = root
	return manifest, nil
}

func (m Manifest) ValidateStructure() error {
	if m.SchemaVersion != ManifestSchemaV1 {
		return fmt.Errorf("manifest: schema_version must be %s", ManifestSchemaV1)
	}
	if len(m.Leaves) == 0 {
		return fmt.Errorf("manifest: must contain at least one leaf")
	}
	seen := make(map[string]struct{}, len(m.Leaves))
	for i, leaf := range m.Leaves {
		if leaf.Path == "" {
			return fmt.Errorf("manifest: leaf %d has an empty path", i)
		}
		if _, dup := seen[leaf.Path]; dup {
			return fmt.Errorf("manifest: duplicate leaf path %q", leaf.Path)
		}
		seen[leaf.Path] = struct{}{}
		if err := validateDigest(leaf.Digest); err != nil {
			return fmt.Errorf("manifest: leaf %q: %w", leaf.Path, err)
		}
		if leaf.Size < 0 {
			return fmt.Errorf("manifest: leaf %q has a negative size", leaf.Path)
		}
	}
	return nil
}

func (m Manifest) Verify() error {
	if err := m.ValidateStructure(); err != nil {
		return err
	}
	if err := validateDigest(m.Root); err != nil {
		return fmt.Errorf("manifest: root: %w", err)
	}
	sorted := append([]Leaf(nil), m.Leaves...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	root, err := rootOf(sorted)
	if err != nil {
		return err
	}
	if root != m.Root {
		return fmt.Errorf("manifest: root mismatch")
	}
	return nil
}

func (m Manifest) Bytes() ([]byte, error) {
	return Canonicalize(m)
}

func (m Manifest) RevealPath(path string) (Leaf, bool) {
	for _, leaf := range m.Leaves {
		if leaf.Path == path {
			return leaf, true
		}
	}
	return Leaf{}, false
}

func Prove(m Manifest, path string) ([]ProofNode, error) {
	if err := m.Verify(); err != nil {
		return nil, err
	}
	sorted := append([]Leaf(nil), m.Leaves...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	index := -1
	for i, leaf := range sorted {
		if leaf.Path == path {
			index = i
			break
		}
	}
	if index < 0 {
		return nil, fmt.Errorf("manifest: path %q not found", path)
	}
	level := make([][]byte, len(sorted))
	for i, leaf := range sorted {
		hash, err := leafHash(leaf)
		if err != nil {
			return nil, err
		}
		level[i] = hash
	}
	var proof []ProofNode
	position := index
	for len(level) > 1 {
		if position%2 == 0 {
			if position+1 < len(level) {
				proof = append(proof, ProofNode{Hash: "sha256:" + hex.EncodeToString(level[position+1]), Left: false})
			}
		} else {
			proof = append(proof, ProofNode{Hash: "sha256:" + hex.EncodeToString(level[position-1]), Left: true})
		}
		next := make([][]byte, 0, (len(level)+1)/2)
		for i := 0; i < len(level); i += 2 {
			if i+1 == len(level) {
				next = append(next, level[i])
				continue
			}
			next = append(next, nodeHash(level[i], level[i+1]))
		}
		level = next
		position /= 2
	}
	return proof, nil
}

func VerifyProof(leaf Leaf, proof []ProofNode, root string) error {
	hash, err := leafHash(leaf)
	if err != nil {
		return err
	}
	current := hash
	for _, node := range proof {
		sibling, err := hex.DecodeString(trimSHA256(node.Hash))
		if err != nil || len(sibling) != sha256.Size {
			return fmt.Errorf("merkle proof: invalid sibling hash")
		}
		if node.Left {
			current = nodeHash(sibling, current)
		} else {
			current = nodeHash(current, sibling)
		}
	}
	if "sha256:"+hex.EncodeToString(current) != root {
		return fmt.Errorf("merkle proof: root mismatch")
	}
	return nil
}

func rootOf(leaves []Leaf) (string, error) {
	level := make([][]byte, len(leaves))
	for i, leaf := range leaves {
		hash, err := leafHash(leaf)
		if err != nil {
			return "", err
		}
		level[i] = hash
	}
	for len(level) > 1 {
		next := make([][]byte, 0, (len(level)+1)/2)
		for i := 0; i < len(level); i += 2 {
			if i+1 == len(level) {
				next = append(next, level[i])
				continue
			}
			next = append(next, nodeHash(level[i], level[i+1]))
		}
		level = next
	}
	return "sha256:" + hex.EncodeToString(level[0]), nil
}

func leafHash(leaf Leaf) ([]byte, error) {
	if err := validateDigest(leaf.Digest); err != nil {
		return nil, err
	}
	if leaf.Path == "" {
		return nil, fmt.Errorf("merkle: empty leaf path")
	}
	payload, err := Canonicalize(leaf)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	h.Write([]byte{0x00})
	h.Write(payload)
	return h.Sum(nil), nil
}

func nodeHash(left, right []byte) []byte {
	h := sha256.New()
	h.Write([]byte{0x01})
	h.Write(left)
	h.Write(right)
	return h.Sum(nil)
}

func validateDigest(digest string) error {
	raw := trimSHA256(digest)
	if len(raw) != sha256.Size*2 {
		return fmt.Errorf("digest must be sha256 hex")
	}
	if _, err := hex.DecodeString(raw); err != nil {
		return fmt.Errorf("digest must be hexadecimal")
	}
	return nil
}

func trimSHA256(digest string) string {
	if len(digest) > 7 && digest[:7] == "sha256:" {
		return digest[7:]
	}
	return digest
}

func sha256Sum(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
