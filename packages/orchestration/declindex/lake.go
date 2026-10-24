package declindex

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const (
	leanHashSeed  uint64 = 11
	leanHashMix   uint64 = 0xc6a4a7935bd1e995
	leanHashShift        = 47
	lakeNilHash   uint64 = 1723
)

// LakeTextHash returns Lake.Hash.ofText for src. Lake normalizes CRLF before hashing text, then mixes Lean's
// string hash with Hash.nil. The result is the lowercase hexadecimal value stored in a build trace.
func LakeTextHash(src []byte) string {
	if bytes.Contains(src, []byte("\r\n")) {
		src = bytes.ReplaceAll(src, []byte("\r\n"), []byte("\n"))
	}
	return hexUint64(leanUint64MixHash(lakeNilHash, leanStringHash(src)))
}

func hexUint64(v uint64) string {
	var raw [8]byte
	var encoded [16]byte
	binary.BigEndian.PutUint64(raw[:], v)
	hex.Encode(encoded[:], raw[:])
	return string(encoded[:])
}

func leanStringHash(src []byte) uint64 {
	h := leanHashSeed ^ uint64(len(src))*leanHashMix
	i := 0
	for ; i+8 <= len(src); i += 8 {
		k := binary.LittleEndian.Uint64(src[i:])
		k *= leanHashMix
		k ^= k >> leanHashShift
		k *= leanHashMix
		h ^= k
		h *= leanHashMix
	}
	switch len(src) - i {
	case 7:
		h ^= uint64(src[i+6]) << 48
		fallthrough
	case 6:
		h ^= uint64(src[i+5]) << 40
		fallthrough
	case 5:
		h ^= uint64(src[i+4]) << 32
		fallthrough
	case 4:
		h ^= uint64(src[i+3]) << 24
		fallthrough
	case 3:
		h ^= uint64(src[i+2]) << 16
		fallthrough
	case 2:
		h ^= uint64(src[i+1]) << 8
		fallthrough
	case 1:
		h ^= uint64(src[i])
		h *= leanHashMix
	}
	h ^= h >> leanHashShift
	h *= leanHashMix
	h ^= h >> leanHashShift
	return h
}

func leanUint64MixHash(h, k uint64) uint64 {
	k *= leanHashMix
	k ^= k >> leanHashShift
	k ^= leanHashMix
	h ^= k
	return h * leanHashMix
}

// FreshnessState classifies the candidate build output for a module.
type FreshnessState uint8

const (
	// FreshnessNotBuilt means the candidate build produced no output for the module.
	FreshnessNotBuilt FreshnessState = iota
	// FreshnessFresh means Lake's trace proves that the reference file is for the candidate source.
	FreshnessFresh
	// FreshnessStale means the candidate build output cannot be trusted for the candidate source.
	FreshnessStale
)

// Freshness is Fresh's result. Reason is set when State is FreshnessStale.
type Freshness struct {
	State  FreshnessState
	Reason string
}

const lakeTraceSchemaVersion = "2025-09-10"

type lakeTraceMetadata struct {
	SchemaVersion string            `json:"schemaVersion"`
	Inputs        []json.RawMessage `json:"inputs"`
	Outputs       *struct {
		ILean string `json:"i"`
	} `json:"outputs"`
	Synthetic *bool `json:"synthetic"`
}

// Fresh reports whether the .ilean at root/libRel/rel came from blob, and for a fresh module gives the .ilean bytes
// whose hash it checked. A module with no candidate output is not built; malformed, missing, or mismatched Lake
// metadata is stale with its reason. The build directory's own source must be blob, and the .ilean must be the
// output its trace records. A build trace also binds that source by its hash; a fetch trace (an artifact-cache hit)
// binds the outputs only, and the source by age.
func Fresh(root, libRel, rel string, blob []byte) (Freshness, []byte, error) {
	base := filepath.Join(root, filepath.FromSlash(libRel), filepath.FromSlash(rel))
	ilean, err := fileExists(base + ".ilean")
	if err != nil {
		return Freshness{}, nil, err
	}
	olean, err := fileExists(base + ".olean")
	if err != nil {
		return Freshness{}, nil, err
	}
	trace, err := fileExists(base + ".trace")
	if err != nil {
		return Freshness{}, nil, err
	}
	if !ilean && !olean && !trace {
		return Freshness{State: FreshnessNotBuilt}, nil, nil
	}
	if !ilean {
		return staleFreshness("missing " + rel + ".ilean"), nil, nil
	}
	if !trace {
		return staleFreshness("missing " + rel + ".trace"), nil, nil
	}
	raw, err := os.ReadFile(base + ".trace")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return staleFreshness("missing " + rel + ".trace"), nil, nil
		}
		return Freshness{}, nil, err
	}
	var metadata lakeTraceMetadata
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return staleFreshness("invalid " + rel + ".trace"), nil, nil
	}
	if metadata.SchemaVersion != lakeTraceSchemaVersion {
		return staleFreshness("unknown Lake trace schema " + metadata.SchemaVersion), nil, nil
	}
	if metadata.Synthetic == nil {
		return staleFreshness("missing synthetic flag"), nil, nil
	}
	sourcePath := filepath.Join(root, filepath.FromSlash(rel)+".lean")
	source, err := os.ReadFile(sourcePath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return staleFreshness("missing built source " + rel + ".lean"), nil, nil
		}
		return Freshness{}, nil, err
	}
	if !bytes.Equal(source, blob) {
		return staleFreshness("built source " + rel + ".lean differs from the candidate blob"), nil, nil
	}
	if metadata.Outputs == nil || metadata.Outputs.ILean == "" {
		return staleFreshness("build trace records no .ilean output for " + rel), nil, nil
	}
	ileanData, err := os.ReadFile(base + ".ilean")
	if err != nil {
		return Freshness{}, nil, err
	}
	if metadata.Outputs.ILean != LakeTextHash(ileanData)+".ilean" {
		return staleFreshness(rel + ".ilean is not the output its build trace records"), nil, nil
	}
	if *metadata.Synthetic {
		traceInfo, err := os.Stat(base + ".trace")
		if err != nil {
			return Freshness{}, nil, err
		}
		sourceInfo, err := os.Stat(sourcePath)
		if err != nil {
			return Freshness{}, nil, err
		}
		if traceInfo.ModTime().Before(sourceInfo.ModTime()) {
			return staleFreshness("fetch trace is older than its source"), nil, nil
		}
		return Freshness{State: FreshnessFresh}, ileanData, nil
	}
	hash, ok := sourceTraceHash(metadata.Inputs, sourcePath, rel+".lean")
	if !ok {
		return staleFreshness("build trace has no source hash for " + rel + ".lean"), nil, nil
	}
	if want := LakeTextHash(blob); hash != want {
		return staleFreshness("build trace source hash differs from the candidate blob"), nil, nil
	}
	return Freshness{State: FreshnessFresh}, ileanData, nil
}

func staleFreshness(reason string) Freshness {
	return Freshness{State: FreshnessStale, Reason: reason}
}

func fileExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func sourceTraceHash(inputs []json.RawMessage, sourcePath, relPath string) (string, bool) {
	for _, input := range inputs {
		var pair []json.RawMessage
		if err := json.Unmarshal(input, &pair); err != nil || len(pair) != 2 {
			continue
		}
		var caption string
		if err := json.Unmarshal(pair[0], &caption); err != nil {
			continue
		}
		if sameSourceCaption(caption, sourcePath, relPath) {
			var hash string
			if err := json.Unmarshal(pair[1], &hash); err == nil {
				return hash, true
			}
			return "", false
		}
		var children []json.RawMessage
		if err := json.Unmarshal(pair[1], &children); err == nil {
			if hash, ok := sourceTraceHash(children, sourcePath, relPath); ok {
				return hash, true
			}
		}
	}
	return "", false
}

func sameSourceCaption(caption, sourcePath, relPath string) bool {
	if filepath.Clean(caption) == filepath.Clean(sourcePath) {
		return true
	}
	return !filepath.IsAbs(caption) && filepath.ToSlash(filepath.Clean(caption)) == strings.TrimPrefix(filepath.ToSlash(relPath), "./")
}
