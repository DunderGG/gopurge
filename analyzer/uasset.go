// uasset.go implements a structured parser for the Unreal Engine binary package
// format used by .uasset and .umap files.
//
// Every package file begins with a Package File Summary whose header contains a
// Name Map — a flat, uncompressed array of every string identifier used by the
// package (asset paths, class names, property names, etc.). Because this table
// is stored before any compressed or encrypted payload section, reading /Game/...
// paths from it eliminates the false matches that arise when those byte sequences
// happen to appear inside compressed data.
//
// Parsing strategy:
//
//  1. Validate the 4-byte magic number (0x9E2A83C1).
//  2. Walk the version-dependent fields of the Package File Summary header to
//     locate NameCount and NameOffset.
//  3. Seek directly to NameOffset and read NameCount FNameEntry records.
//  4. Return every entry whose value begins with /Game/.
//
// Supported file versions: UE4 (LegacyFileVersion −4 through −7) and UE5
// (LegacyFileVersion −8). Any file that does not start with the expected magic
// number, or whose header cannot be fully parsed, returns a non-nil error so
// that the caller can fall back to a raw binary scan.
package analyzer

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"unicode/utf16"
)

// uassetMagic is the 4-byte little-endian signature that every .uasset and
// .umap file begins with (0x9E2A83C1).
const uassetMagic uint32 = 0x9E2A83C1

// verUE4NameHashesSerialized is the FileVersionUE4 value at or above which
// each Name Map entry is followed by 4 bytes of hash data:
// NonCasePreservingHash (uint16) + CasePreservingHash (uint16). Introduced in
// Unreal Engine 4.26 (object version 504).
const verUE4NameHashesSerialized int32 = 504

// verUE5PackageSavedHash is the FileVersionUE5 value at or above which the
// PackageFileSummary includes a 20-byte SavedHash (FIoHash) and the
// SectionSixOffset/TotalHeaderSize field immediately after FileVersionLicenseeUE,
// before the CustomVersionContainer. Introduced in Unreal Engine 5.6
// (EObjectVersionUE5::PACKAGE_SAVED_HASH = 1016, FileVersionUE5 = 1017).
const verUE5PackageSavedHash int32 = 1016

// parseUAssetImports reads the Package File Summary of a .uasset or .umap
// binary, locates the Name Map, and returns every /Game/... string found in it.
//
// The Name Map is a flat, uncompressed array of all identifiers referenced by
// the package. Filtering it for the /Game/ prefix yields the set of asset
// package paths this file depends on, without touching potentially-compressed
// payload bytes.
//
// A non-nil error means the data is not a recognisable UAsset binary or its
// header could not be parsed; the caller should fall back to a raw scan.
func parseUAssetImports(data []byte) ([]string, error) {
	reader := bytes.NewReader(data)

	// ── 1. Validate magic number ──────────────────────────────────────────
	// The first 4 bytes of every .uasset/.umap file are a fixed magic number that identifies the format. 
	// If this check fails, we can skip all further parsing and fall back to a raw scan.
	var magic uint32
	if err := binary.Read(reader, binary.LittleEndian, &magic); err != nil {
		return nil, fmt.Errorf("read magic: %w", err)
	}
	if magic != uassetMagic {
		return nil, fmt.Errorf("unrecognised magic 0x%08X", magic)
	}

	// ── 2. Parse version fields ───────────────────────────────────────────
	// The layout of the Package File Summary header is version-dependent, 
	// but the version fields always appear in the same order, so we can walk them sequentially to locate the Name Map.
	var legacyFileVersion, fileVersionUE4 int32
	if err := binary.Read(reader, binary.LittleEndian, &legacyFileVersion); err != nil {
		return nil, fmt.Errorf("read LegacyFileVersion: %w", err)
	}

	// LegacyUE3Version is present in all files except the rare −4 variant.
	if legacyFileVersion != -4 {
		if _, err := reader.Seek(4, io.SeekCurrent); err != nil {
			return nil, fmt.Errorf("seek past LegacyUE3Version: %w", err)
		}
	}

	// FileVersionUE4 is present in all UE4 and UE5 files, but not in the legacy UE3 ones. 
	// If LegacyFileVersion is positive, it's a UE3 file and we should skip this field; otherwise, 
	// it's a UE4/UE5 file and we need to read it to determine if Name Map entries are followed by hashes.
	if err := binary.Read(reader, binary.LittleEndian, &fileVersionUE4); err != nil {
		return nil, fmt.Errorf("read FileVersionUE4: %w", err)
	}

	// FileVersionUE5 is present only in UE5 files (LegacyFileVersion ≤ −8).
	// We read the actual value (instead of just seeking past it) because it
	// determines where SectionSixOffset / SavedHash appear in the header.
	var fileVersionUE5 int32
	if legacyFileVersion <= -8 {
		if err := binary.Read(reader, binary.LittleEndian, &fileVersionUE5); err != nil {
			return nil, fmt.Errorf("read FileVersionUE5: %w", err)
		}
	}

	// Skip FileVersionLicenseeUE (4 bytes).
	if _, err := reader.Seek(4, io.SeekCurrent); err != nil {
		return nil, fmt.Errorf("seek past FileVersionLicenseeUE: %w", err)
	}

	// In UE5.6+ (ObjectVersionUE5 ≥ PACKAGE_SAVED_HASH = 1016) the summary
	// stores a 20-byte FIoHash (SavedHash) and a 4-byte SectionSixOffset here,
	// before the CustomVersionContainer. In earlier versions those two fields
	// appear after the CustomVersionContainer instead.
	if fileVersionUE5 >= verUE5PackageSavedHash {
		if _, err := reader.Seek(24, io.SeekCurrent); err != nil {
			return nil, fmt.Errorf("seek past SavedHash and SectionSixOffset: %w", err)
		}
	}

	// ── 3. Skip CustomVersions array ─────────────────────────────────────
	// Present whenever LegacyFileVersion ≤ −2. 
	// Entry size depends on format:
	//   −2         : FEnumCustomVersion  — int32 Tag + int32 Version   = 8 bytes
	//   −3, −4, −5 : FGuidCustomVersion  — FGuid(16) + int32(4) + FString(var)
	//   ≤ −6       : Optimized           — FGuid(16) + int32 Version   = 20 bytes
	if legacyFileVersion <= -2 {
		var count int32
		if err := binary.Read(reader, binary.LittleEndian, &count); err != nil {
			return nil, fmt.Errorf("read CustomVersions count: %w", err)
		}
		if count < 0 {
			return nil, fmt.Errorf("negative CustomVersions count: %d", count)
		}

		switch {
		case legacyFileVersion <= -6:
			// Optimized: 20 bytes per entry.
			if int64(count)*20 > int64(len(data)) {
				return nil, fmt.Errorf("implausible CustomVersions count: %d", count)
			}
			if _, err := reader.Seek(int64(count)*20, io.SeekCurrent); err != nil {
				return nil, fmt.Errorf("seek past CustomVersions: %w", err)
			}
		case legacyFileVersion == -2:
			// FEnumCustomVersion: 8 bytes per entry.
			if int64(count)*8 > int64(len(data)) {
				return nil, fmt.Errorf("implausible CustomVersions count: %d", count)
			}
			if _, err := reader.Seek(int64(count)*8, io.SeekCurrent); err != nil {
				return nil, fmt.Errorf("seek past CustomVersions: %w", err)
			}
		default:
			// FGuidCustomVersion (−3, −4, −5): variable size; read entry-by-entry.
			for i := int32(0); i < count; i++ {
				if _, err := reader.Seek(20, io.SeekCurrent); err != nil {
					return nil, fmt.Errorf("seek past FGuidCustomVersion[%d]: %w", i, err)
				}
				if err := skipFString(reader); err != nil {
					return nil, fmt.Errorf("skip CustomVersions FriendlyName[%d]: %w", i, err)
				}
			}
		}
	}

	// ── 4. Skip fixed fields that precede NameCount ───────────────────────
	// SectionSixOffset/TotalHeaderSize (int32) follows here for UE5.5 and
	// earlier. For UE5.6+ it was already consumed before the CustomVersions.
	if fileVersionUE5 < verUE5PackageSavedHash {
		if _, err := reader.Seek(4, io.SeekCurrent); err != nil {
			return nil, fmt.Errorf("seek past TotalHeaderSize: %w", err)
		}
	}
	// FolderName (FString)
	if err := skipFString(reader); err != nil {
		return nil, fmt.Errorf("skip FolderName: %w", err)
	}
	// PackageFlags (uint32)
	if _, err := reader.Seek(4, io.SeekCurrent); err != nil {
		return nil, fmt.Errorf("seek past PackageFlags: %w", err)
	}

	// ── 5. Read Name Map location ─────────────────────────────────────────
	var nameCount, nameOffset int32
	if err := binary.Read(reader, binary.LittleEndian, &nameCount); err != nil {
		return nil, fmt.Errorf("read NameCount: %w", err)
	}
	if err := binary.Read(reader, binary.LittleEndian, &nameOffset); err != nil {
		return nil, fmt.Errorf("read NameOffset: %w", err)
	}

	// Sanity check the Name Map location to avoid seeking to an absurd location if the header is corrupted.
	if nameCount <= 0 || nameOffset <= 0 || int64(nameOffset) >= int64(len(data)) {
		return nil, fmt.Errorf("invalid Name Map location (count=%d, offset=%d)", nameCount, nameOffset)
	}

	// ── 6. Seek to the Name Map and read it ───────────────────────────────
	if _, err := reader.Seek(int64(nameOffset), io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek to Name Map at offset %d: %w", nameOffset, err)
	}
	hasHashes := fileVersionUE4 >= verUE4NameHashesSerialized
	names, err := readNameMap(reader, nameCount, hasHashes)
	if err != nil {
		return nil, fmt.Errorf("read Name Map: %w", err)
	}

	// ── 7. Filter for /Game/... package paths ─────────────────────────────
	var gamePaths []string
	for _, name := range names {
		if strings.HasPrefix(name, softObjectPathPrefix) {
			gamePaths = append(gamePaths, name)
		}
	}
	return gamePaths, nil
}

// readNameMap reads nameCount consecutive FNameEntry records from reader and returns
// their decoded string values. When hasHashes is true, each string is followed
// by 4 bytes of hash data (NonCasePreservingHash uint16 + CasePreservingHash
// uint16) which are consumed and discarded.
func readNameMap(reader *bytes.Reader, nameCount int32, hasHashes bool) ([]string, error) {
	names := make([]string, 0, nameCount)
	for i := int32(0); i < nameCount; i++ {
		name, err := readFString(reader)
		if err != nil {
			return nil, fmt.Errorf("name entry %d: %w", i, err)
		}
		names = append(names, name)
		if hasHashes {
			// NonCasePreservingHash (uint16) + CasePreservingHash (uint16) = 4 bytes.
			if _, err := reader.Seek(4, io.SeekCurrent); err != nil {
				return nil, fmt.Errorf("name entry %d hash: %w", i, err)
			}
		}
	}
	return names, nil
}

// readFString reads a length-prefixed Unreal FString from reader.
//
//   - length > 0: ANSI/Latin-1 bytes, length includes the null terminator.
//   - length < 0: UTF-16 LE code units, |length| units including null terminator.
//   - length == 0: empty string, no bytes follow.
func readFString(reader *bytes.Reader) (string, error) {
	var length int32
	if err := binary.Read(reader, binary.LittleEndian, &length); err != nil {
		return "", fmt.Errorf("read FString length: %w", err)
	}
	switch {
		// 0 length means an empty string, and no bytes follow.
	case length == 0:
		return "", nil

	// Positive length means ANSI/Latin-1 bytes, including a null terminator. 
	// We read the specified number of bytes, convert to a string, and strip the null terminator if present.
	case length > 0:
		if length > 65535 {
			return "", fmt.Errorf("ANSI FString length %d exceeds sanity cap", length)
		}
		buf := make([]byte, length)
		if _, err := io.ReadFull(reader, buf); err != nil {
			return "", fmt.Errorf("read ANSI FString data: %w", err)
		}
		// Strip null terminator if present.
		if len(buf) > 0 && buf[len(buf)-1] == 0 {
			buf = buf[:len(buf)-1]
		}
		return string(buf), nil

	// Negative length means UTF-16 LE code units, including a null terminator. 
	// We read the specified number of uint16 code units, convert to a string, and strip the null terminator if present.
	default: 
		charCount := -length
		if charCount > 32767 {
			return "", fmt.Errorf("UTF-16 FString length %d exceeds sanity cap", charCount)
		}
		buf := make([]uint16, charCount)
		if err := binary.Read(reader, binary.LittleEndian, buf); err != nil {
			return "", fmt.Errorf("read UTF-16 FString data: %w", err)
		}
		// Strip null terminator if present.
		if len(buf) > 0 && buf[len(buf)-1] == 0 {
			buf = buf[:len(buf)-1]
		}
		return string(utf16.Decode(buf)), nil
	}
}

// skipFString reads and discards an FString from reader.
func skipFString(reader *bytes.Reader) error {
	_, err := readFString(reader)
	return err
}
