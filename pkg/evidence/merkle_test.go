package evidence

import (
	"fmt"
	"strings"
	"testing"
)

func makeLeaves(count int) []Leaf {
	leaves := make([]Leaf, 0, count)
	for i := 0; i < count; i++ {
		content := []byte(fmt.Sprintf("leaf-content-%02d", i))
		leaves = append(leaves, Leaf{
			Path:      fmt.Sprintf("dir/file-%02d.json", i),
			Digest:    DigestBytes(content),
			Size:      int64(len(content)),
			MediaType: "application/json",
		})
	}
	return leaves
}

func TestBuildManifestSortsLeavesAndComputesStableRoot(t *testing.T) {
	leaves := makeLeaves(5)
	reversed := make([]Leaf, len(leaves))
	for i := range leaves {
		reversed[i] = leaves[len(leaves)-1-i]
	}
	first, err := BuildManifest(leaves)
	if err != nil {
		t.Fatalf("build manifest: %v", err)
	}
	second, err := BuildManifest(reversed)
	if err != nil {
		t.Fatalf("build reversed manifest: %v", err)
	}
	if first.Root != second.Root {
		t.Fatalf("root depends on input order: %s vs %s", first.Root, second.Root)
	}
	for i := 1; i < len(first.Leaves); i++ {
		if first.Leaves[i-1].Path >= first.Leaves[i].Path {
			t.Fatalf("leaves not sorted: %s >= %s", first.Leaves[i-1].Path, first.Leaves[i].Path)
		}
	}
	if err := first.Verify(); err != nil {
		t.Fatalf("verify manifest: %v", err)
	}
	if leaf, ok := first.RevealPath(leaves[2].Path); !ok || leaf.Digest != leaves[2].Digest {
		t.Fatalf("reveal path = %+v, %v", leaf, ok)
	}
	if _, ok := first.RevealPath("missing"); ok {
		t.Fatal("revealed a missing path")
	}
}

func TestBuildManifestRejectsDuplicatesAndInvalidLeaves(t *testing.T) {
	if _, err := BuildManifest(nil); err == nil {
		t.Fatal("empty manifest accepted")
	}
	leaves := makeLeaves(2)
	duplicate := append([]Leaf(nil), leaves...)
	duplicate[1].Path = duplicate[0].Path
	if _, err := BuildManifest(duplicate); err == nil {
		t.Fatal("duplicate leaf path accepted")
	}
	tests := []struct {
		name string
		leaf Leaf
	}{
		{"empty path", Leaf{Path: "", Digest: DigestBytes([]byte("x"))}},
		{"empty digest", Leaf{Path: "a", Digest: ""}},
		{"short digest", Leaf{Path: "a", Digest: "sha256:abc"}},
		{"non-hex digest", Leaf{Path: "a", Digest: "sha256:" + strings.Repeat("z", 64)}},
		{"negative size", Leaf{Path: "a", Digest: DigestBytes([]byte("x")), Size: -1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := BuildManifest([]Leaf{tc.leaf}); err == nil {
				t.Fatalf("invalid leaf accepted: %+v", tc.leaf)
			}
		})
	}
}

func TestManifestVerifyDetectsTampering(t *testing.T) {
	manifest, err := BuildManifest(makeLeaves(4))
	if err != nil {
		t.Fatalf("build manifest: %v", err)
	}

	tamperedLeaf := manifest
	tamperedLeaf.Leaves = append([]Leaf(nil), manifest.Leaves...)
	tamperedLeaf.Leaves[0].Digest = DigestBytes([]byte("tampered"))
	if err := tamperedLeaf.Verify(); err == nil {
		t.Fatal("tampered leaf digest accepted")
	}

	tamperedPath := manifest
	tamperedPath.Leaves = append([]Leaf(nil), manifest.Leaves...)
	tamperedPath.Leaves[0].Path = "renamed.json"
	if err := tamperedPath.Verify(); err == nil {
		t.Fatal("tampered leaf path accepted")
	}

	tamperedRoot := manifest
	tamperedRoot.Root = DigestBytes([]byte("different root"))
	if err := tamperedRoot.Verify(); err == nil {
		t.Fatal("tampered root accepted")
	}

	duplicate := manifest
	duplicate.Leaves = append([]Leaf(nil), manifest.Leaves...)
	duplicate.Leaves[1].Path = duplicate.Leaves[0].Path
	if err := duplicate.Verify(); err == nil {
		t.Fatal("duplicate path accepted by Verify")
	}
}

func TestProveAndVerifyProofForEveryLeaf(t *testing.T) {
	for count := 1; count <= 17; count++ {
		t.Run(fmt.Sprintf("%d-leaves", count), func(t *testing.T) {
			leaves := makeLeaves(count)
			manifest, err := BuildManifest(leaves)
			if err != nil {
				t.Fatalf("build manifest: %v", err)
			}
			for _, leaf := range manifest.Leaves {
				proof, err := Prove(manifest, leaf.Path)
				if err != nil {
					t.Fatalf("prove %s: %v", leaf.Path, err)
				}
				if err := VerifyProof(leaf, proof, manifest.Root); err != nil {
					t.Fatalf("verify proof for %s (%d leaves): %v", leaf.Path, count, err)
				}
			}
		})
	}
}

func TestProveRejectsUnknownPath(t *testing.T) {
	manifest, err := BuildManifest(makeLeaves(3))
	if err != nil {
		t.Fatalf("build manifest: %v", err)
	}
	if _, err := Prove(manifest, "does/not/exist.json"); err == nil {
		t.Fatal("proof for unknown path accepted")
	}
}

func TestProveRequiresValidManifest(t *testing.T) {
	manifest, err := BuildManifest(makeLeaves(3))
	if err != nil {
		t.Fatalf("build manifest: %v", err)
	}
	manifest.Root = DigestBytes([]byte("wrong root"))
	if _, err := Prove(manifest, manifest.Leaves[0].Path); err == nil {
		t.Fatal("proof produced for an invalid manifest")
	}
}

func TestOddLevelPromotion(t *testing.T) {
	manifest, err := BuildManifest(makeLeaves(3))
	if err != nil {
		t.Fatalf("build manifest: %v", err)
	}
	proof, err := Prove(manifest, manifest.Leaves[2].Path)
	if err != nil {
		t.Fatalf("prove promoted leaf: %v", err)
	}
	if len(proof) != 1 {
		t.Fatalf("promoted leaf proof length = %d, want 1", len(proof))
	}
	if !proof[0].Left {
		t.Fatal("promoted right leaf sibling should be on the left")
	}
	if err := VerifyProof(manifest.Leaves[2], proof, manifest.Root); err != nil {
		t.Fatalf("verify promoted proof: %v", err)
	}

	// A four-leaf tree has no promotion, so the proof shape differs.
	four, err := BuildManifest(makeLeaves(4))
	if err != nil {
		t.Fatalf("build four-leaf manifest: %v", err)
	}
	fourProof, err := Prove(four, four.Leaves[2].Path)
	if err != nil {
		t.Fatalf("prove leaf 2 of four: %v", err)
	}
	if len(fourProof) != 2 {
		t.Fatalf("four-leaf proof length = %d, want 2", len(fourProof))
	}
}

func TestVerifyProofRejectsWrongRootAndLeaf(t *testing.T) {
	manifest, err := BuildManifest(makeLeaves(5))
	if err != nil {
		t.Fatalf("build manifest: %v", err)
	}
	leaf := manifest.Leaves[1]
	proof, err := Prove(manifest, leaf.Path)
	if err != nil {
		t.Fatalf("prove: %v", err)
	}

	if err := VerifyProof(leaf, proof, DigestBytes([]byte("other root"))); err == nil {
		t.Fatal("proof accepted with the wrong root")
	}

	wrongLeaf := leaf
	wrongLeaf.Digest = DigestBytes([]byte("other content"))
	if err := VerifyProof(wrongLeaf, proof, manifest.Root); err == nil {
		t.Fatal("proof accepted for the wrong leaf")
	}

	wrongPath := leaf
	wrongPath.Path = "other/path.json"
	if err := VerifyProof(wrongPath, proof, manifest.Root); err == nil {
		t.Fatal("proof accepted for a leaf with the wrong path")
	}
}

func TestVerifyProofRejectsMalformedSibling(t *testing.T) {
	leaf := makeLeaves(1)[0]
	for _, proof := range [][]ProofNode{
		{{Hash: "not-hex", Left: false}},
		{{Hash: "sha256:abc", Left: true}},
		{{Hash: DigestBytes([]byte("x")), Left: false}, {Hash: "zz", Left: true}},
	} {
		if err := VerifyProof(leaf, proof, DigestBytes([]byte("root"))); err == nil {
			t.Fatalf("malformed proof accepted: %+v", proof)
		}
	}
}

func TestManifestBytesAreCanonical(t *testing.T) {
	manifest, err := BuildManifest(makeLeaves(2))
	if err != nil {
		t.Fatalf("build manifest: %v", err)
	}
	data, err := manifest.Bytes()
	if err != nil {
		t.Fatalf("manifest bytes: %v", err)
	}
	canonical, err := Canonicalize(manifest)
	if err != nil {
		t.Fatalf("canonicalize manifest: %v", err)
	}
	if string(data) != string(canonical) {
		t.Fatal("manifest bytes are not canonical")
	}
}
