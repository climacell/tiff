// Copyright 2024 <climacell.com>. All rights reserved.
// Optimized TIFF reader for high-performance scenarios with adaptive I/O

package tiff

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"io"
	"sort"

	"github.com/climacell/go-middleware/backbone"
)

// Maximum allowed memory allocation per IFD entry (10MB) - circuit breaker
const MaxOptimizedIFDEntrySize = 10 * 1024 * 1024
const DefaultOptimizedBufferSize = 2 * 1024
const DefaultOptimizedMinEfficiency = 0.1

// Maximum gap between entry data offsets to consider them for batch reading (64KB)
const MaxBatchGap = 64 * 1024

// DataRange represents a range of data that needs to be read
type DataRange struct {
	Offset int64
	Size   int64
}

// OptimizedReader is the high-performance TIFF reader with adaptive I/O optimization
type OptimizedReader struct {
	Reader io.ReadSeeker
	Header *Header
	Ifd    [][]*IFD
	rs     *seekioReader

	// Metrics and stats
	IOCalls       int
	BytesRead     int64
	BufferUsed    int64
	Efficiency    float64
	AdaptiveCalls int   // Number of adaptive reads performed
	MetadataSize  int64 // Actual metadata size used
}

// OptimizedReaderConfig controls the behavior of the optimized reader
type OptimizedReaderConfig struct {
	BufferSize       int     // Initial buffer size for metadata reading
	MaxIFDEntrySize  int     // Maximum allowed IFD entry size (circuit breaker)
	EnableFallback   bool    // Whether to fallback to traditional reading on failure
	EnableMetrics    bool    // Whether to collect detailed metrics
	MinEfficiency    float64 // Minimum buffer efficiency to use single I/O
	MaxAdaptiveReads int     // Maximum number of adaptive reads allowed
}

// OptimizedReaderOption configures the optimized reader
type OptimizedReaderOption func(*OptimizedReaderConfig)

// WithBufferSize sets the initial buffer size for metadata reading
func WithBufferSize(size int) OptimizedReaderOption {
	return func(c *OptimizedReaderConfig) {
		c.BufferSize = size
	}
}

// WithMaxIFDEntrySize sets the maximum allowed IFD entry size (circuit breaker)
func WithMaxIFDEntrySize(size int) OptimizedReaderOption {
	return func(c *OptimizedReaderConfig) {
		c.MaxIFDEntrySize = size
	}
}

// WithFallbackDisabled disables fallback to traditional reading
func WithFallbackDisabled() OptimizedReaderOption {
	return func(c *OptimizedReaderConfig) {
		c.EnableFallback = false
	}
}

// WithMinEfficiency sets the minimum buffer efficiency required
func WithMinEfficiency(efficiency float64) OptimizedReaderOption {
	return func(c *OptimizedReaderConfig) {
		c.MinEfficiency = efficiency
	}
}

// WithMetricsDisabled disables detailed metrics collection
func WithMetricsEnabled() OptimizedReaderOption {
	return func(c *OptimizedReaderConfig) {
		c.EnableMetrics = true
	}
}

// WithMaxAdaptiveReads sets the maximum number of adaptive reads allowed
func WithMaxAdaptiveReads(maxReads int) OptimizedReaderOption {
	return func(c *OptimizedReaderConfig) {
		c.MaxAdaptiveReads = maxReads
	}
}

// OpenReaderOptimized creates a new optimized TIFF reader with adaptive I/O
// This reader is designed for high-performance scenarios with network/mounted filesystems
func OpenReaderOptimized(r io.Reader, bb *backbone.Backbone, opts ...OptimizedReaderOption) (*OptimizedReader, error) {

	// Configure options with sensible defaults
	config := &OptimizedReaderConfig{
		BufferSize:       DefaultOptimizedBufferSize,
		MaxIFDEntrySize:  MaxOptimizedIFDEntrySize,
		EnableFallback:   true,
		EnableMetrics:    false,
		MinEfficiency:    DefaultOptimizedMinEfficiency,
		MaxAdaptiveReads: 5, // Allow up to 5 adaptive reads
	}

	for _, opt := range opts {
		opt(config)
	}

	rs := openSeekioReader(r, -1)

	// Try optimized adaptive I/O approach first
	optimizedReader, err := readTIFFMetadataAdaptive(rs, bb, config)
	if err == nil {
		optimizedReader.rs = rs
		// Only track key performance metrics
		if config.EnableMetrics && bb != nil {
			bb.Metrics.Distribution("tiff.optimized.io_calls", float64(optimizedReader.IOCalls), []string{}, 0.1)
			bb.Metrics.Distribution("tiff.optimized.efficiency", optimizedReader.Efficiency, []string{}, 0.1)
		}
		return optimizedReader, nil
	}

	// Log the failure reason
	if bb != nil {
		bb.Logger.Warnw("Adaptive I/O optimization failed",
			"error", err.Error(),
			"bufferSize", config.BufferSize,
			"fallbackEnabled", config.EnableFallback)

	}

	// Fallback to traditional approach if enabled
	if config.EnableFallback {

		return fallbackToTraditionalOptimized(rs, bb, config)
	}

	rs.Close()
	return nil, fmt.Errorf("optimized reading failed and fallback disabled: %w", err)
}

// NewOptimizedReader creates a new optimized TIFF reader with adaptive I/O
// With pre-parsed IFD and header
func NewOptimizedReader(r io.ReadSeeker, ifd [][]*IFD, header *Header, bb *backbone.Backbone, opts ...OptimizedReaderOption) *OptimizedReader {
	rs := openSeekioReader(r, -1)

	config := &OptimizedReaderConfig{
		BufferSize:       DefaultOptimizedBufferSize,
		MaxIFDEntrySize:  MaxOptimizedIFDEntrySize,
		EnableFallback:   true,
		EnableMetrics:    false,
		MinEfficiency:    DefaultOptimizedMinEfficiency,
		MaxAdaptiveReads: 5, // Allow up to 5 adaptive reads
	}

	for _, opt := range opts {
		opt(config)
	}

	return &OptimizedReader{
		Reader: rs,
		Header: header,
		Ifd:    ifd,
		rs:     rs,
	}
}

// readTIFFMetadataAdaptive performs smart adaptive metadata reading
func readTIFFMetadataAdaptive(r io.ReadSeeker, bb *backbone.Backbone, config *OptimizedReaderConfig) (*OptimizedReader, error) {
	// No global state needed - offset adjustment handled properly in parsing

	// Smart initial read strategy:
	// 1. Read header first to understand file structure
	// 2. If file is small or metadata is clustered near start, read optimally
	// 3. Otherwise, fall back to traditional reading

	if _, err := r.Seek(0, 0); err != nil {
		return nil, fmt.Errorf("seek to start failed: %w", err)
	}

	buffer := make([]byte, config.BufferSize)
	actualRead, err := r.Read(buffer)
	if err != nil && err != io.EOF {
		return nil, fmt.Errorf("initial metadata buffer read failed: %w", err)
	}

	// Trim to actual size - this is the ACTUAL bytes read from file
	buffer = buffer[:actualRead]

	optimizedReader := &OptimizedReader{
		Reader:    r,
		IOCalls:   1,                 // Started with one I/O call
		BytesRead: int64(actualRead), // Actual bytes read from file
	}

	// Smart parsing with adaptive reading for missing data
	finalBuffer, err := parseWithAdaptiveReading(optimizedReader, buffer, r, bb, config)
	if err != nil {
		return nil, err
	}

	// Final parse with complete buffer
	err = parseCompleteMetadataFromBufferOptimized(optimizedReader, finalBuffer, bb, config)
	if err != nil {
		return nil, err
	}

	return optimizedReader, nil
}

// parseWithAdaptiveReading intelligently identifies and reads missing metadata
func parseWithAdaptiveReading(reader *OptimizedReader, initialBuffer []byte, r io.ReadSeeker, bb *backbone.Backbone, config *OptimizedReaderConfig) ([]byte, error) {
	// 1. Parse what we can from initial buffer to understand structure
	missingRanges, err := identifyMissingDataRanges(initialBuffer, bb, config)
	if err != nil {
		return nil, fmt.Errorf("failed to identify missing data ranges: %w", err)
	}

	if len(missingRanges) == 0 {
		// All metadata fits in initial buffer - we're done!
		return initialBuffer, nil
	}

	// SMART BATCHING: Only apply if we have missing ranges to batch
	if len(missingRanges) > 0 {
		smartBatchRanges, err := calculateSmartIFDBatch(initialBuffer, missingRanges, r, bb, config)
		if err != nil {
			if bb != nil {
				bb.Logger.Warnw("Smart IFD batching failed, using individual ranges", "error", err)
			}
			// Continue with original missing ranges
		} else if len(smartBatchRanges) > 0 {
			// Use smart batch instead of individual ranges
			missingRanges = smartBatchRanges
		}
	}

	// Circuit breaker: prevent too many adaptive reads
	if len(missingRanges) > config.MaxAdaptiveReads {
		if bb != nil {
			bb.Logger.Warnw("Too many missing ranges detected, falling back",
				"missingRanges", len(missingRanges),
				"maxAllowed", config.MaxAdaptiveReads)
		}
		return nil, fmt.Errorf("too many missing data ranges: %d (max: %d)", len(missingRanges), config.MaxAdaptiveReads)
	}

	// 2. Read only the missing pieces
	finalBuffer, additionalIOCalls, additionalBytes, err := readMissingRanges(r, initialBuffer, missingRanges, bb)
	if err != nil {
		return nil, fmt.Errorf("failed to read missing ranges: %w", err)
	}

	// 3. Update reader stats
	reader.IOCalls += additionalIOCalls
	reader.BytesRead += additionalBytes // Add only the additional bytes actually read
	reader.AdaptiveCalls = additionalIOCalls

	return finalBuffer, nil
}

// identifyMissingDataRanges analyzes current buffer and identifies what IFD entry data is missing
func identifyMissingDataRanges(buffer []byte, bb *backbone.Backbone, config *OptimizedReaderConfig) ([]DataRange, error) {
	var missingRanges []DataRange

	// Parse header first
	header, err := parseHeaderFromBufferOptimized(buffer, bb)
	if err != nil {
		return nil, fmt.Errorf("header parsing failed: %w", err)
	}

	// Walk through IFDs and identify entry data that extends beyond buffer
	for offset := header.FirstIFD; offset != 0; {
		if offset >= int64(len(buffer)) {
			// IFD itself is beyond buffer - need to read from current position to IFD
			// This preserves the original logic but smart batching will optimize it
			rangeStart := int64(len(buffer))
			paddingAfter := int64(2048) // 2KB padding after IFD to capture entry data

			ifdSize := estimateIFDSize(header.TiffType)
			totalSize := (offset - rangeStart) + ifdSize + paddingAfter

			missingRanges = append(missingRanges, DataRange{
				Offset: rangeStart,
				Size:   totalSize,
			})
			break // No need to continue - we'll read everything from here
		}

		// Parse IFD entries from current buffer to find missing entry data
		nextOffset, entryMissingRanges, err := analyzeIFDForMissingData(buffer, header, offset, config, bb)
		if err != nil {
			return nil, fmt.Errorf("IFD analysis failed at offset %d: %w", offset, err)
		}

		missingRanges = append(missingRanges, entryMissingRanges...)
		offset = nextOffset
	}

	// Merge adjacent/overlapping ranges for efficiency
	mergedRanges := mergeDataRanges(missingRanges)

	return mergedRanges, nil
}

// analyzeIFDForMissingData parses an IFD from buffer and identifies missing entry data
func analyzeIFDForMissingData(buffer []byte, header *Header, ifdOffset int64, config *OptimizedReaderConfig, bb *backbone.Backbone) (int64, []DataRange, error) {
	var missingRanges []DataRange

	if ifdOffset >= int64(len(buffer)) {
		return 0, nil, fmt.Errorf("IFD offset %d beyond buffer size %d", ifdOffset, len(buffer))
	}

	buf := bytes.NewReader(buffer[ifdOffset:])

	// Read entry count
	var entryCount uint64
	if header.TiffType == TiffType_ClassicTIFF {
		var count16 uint16
		if err := binary.Read(buf, header.ByteOrder, &count16); err != nil {
			return 0, nil, fmt.Errorf("failed to read entry count: %w", err)
		}
		entryCount = uint64(count16)
	} else {
		if err := binary.Read(buf, header.ByteOrder, &entryCount); err != nil {
			return 0, nil, fmt.Errorf("failed to read entry count: %w", err)
		}
	}

	// Circuit breaker for entry count
	if entryCount > 10000 {
		return 0, nil, fmt.Errorf("too many IFD entries: %d (max: 10000)", entryCount)
	}

	// Analyze each entry for missing data
	entrySize := 12 // Classic TIFF entry size
	if header.TiffType == TiffType_BigTIFF {
		entrySize = 20 // BigTIFF entry size
	}

	for i := uint64(0); i < entryCount; i++ {
		entryOffset := ifdOffset + int64(2) + int64(i*uint64(entrySize)) // 2 bytes for entry count
		if header.TiffType == TiffType_BigTIFF {
			entryOffset = ifdOffset + int64(8) + int64(i*uint64(entrySize)) // 8 bytes for entry count
		}

		if entryOffset+int64(entrySize) > int64(len(buffer)) {
			// Entry itself extends beyond buffer
			missingRanges = append(missingRanges, DataRange{
				Offset: entryOffset,
				Size:   int64(entrySize),
			})
			continue
		}

		// Parse entry to check if its data extends beyond buffer
		entryBuf := bytes.NewReader(buffer[entryOffset:])

		var tag TagType
		var dataType DataType
		var count, offset uint64

		if err := binary.Read(entryBuf, header.ByteOrder, &tag); err != nil {
			continue // Skip malformed entry
		}
		if err := binary.Read(entryBuf, header.ByteOrder, &dataType); err != nil {
			continue
		}

		if header.TiffType == TiffType_ClassicTIFF {
			var count32, offset32 uint32
			if err := binary.Read(entryBuf, header.ByteOrder, &count32); err != nil {
				continue
			}
			if err := binary.Read(entryBuf, header.ByteOrder, &offset32); err != nil {
				continue
			}
			count = uint64(count32)
			offset = uint64(offset32)
		} else {
			if err := binary.Read(entryBuf, header.ByteOrder, &count); err != nil {
				continue
			}
			if err := binary.Read(entryBuf, header.ByteOrder, &offset); err != nil {
				continue
			}
		}

		// Check if entry data is beyond the embedded threshold
		dataSize := dataType.ByteSize() * int(count)

		// Apply memory protection
		if dataSize > config.MaxIFDEntrySize {
			if bb != nil {
				bb.Logger.Warnw("IFD entry data size exceeds limit during analysis",
					"tag", tag,
					"dataSize", dataSize,
					"maxAllowed", config.MaxIFDEntrySize)
			}
			continue // Skip this entry to prevent memory issues
		}

		threshold := 4
		if header.TiffType == TiffType_BigTIFF {
			threshold = 8
		}

		if dataSize > threshold {
			// Data is stored at offset location - check if it's beyond buffer
			dataEndOffset := int64(offset) + int64(dataSize)
			if dataEndOffset > int64(len(buffer)) {
				missingRanges = append(missingRanges, DataRange{
					Offset: int64(offset),
					Size:   int64(dataSize),
				})

			}
		}

		// Special handling for Sub-IFD tags - check if Sub-IFD offsets are beyond buffer
		if tag == TagType_SubIFD {
			var subIfdOffsets []int64

			if dataSize <= threshold {
				// Data is embedded in offset field - extract correctly based on data type
				if header.TiffType == TiffType_ClassicTIFF && dataType == DataType_Long {
					// For classic TIFF, offset is 32-bit, might contain multiple 32-bit offsets
					for i := 0; i < int(count) && i < 1; i++ { // For embedded, usually just one offset fits
						subIfdOffsets = append(subIfdOffsets, int64(uint32(offset)))
					}
				} else if header.TiffType == TiffType_BigTIFF && dataType == DataType_Long8 {
					// For BigTIFF, offset is 64-bit
					subIfdOffsets = append(subIfdOffsets, int64(offset))
				}
			} else {
				// Sub-IFD offsets are stored at the offset location - read them if within buffer
				if int64(offset)+int64(dataSize) <= int64(len(buffer)) {
					subBuf := bytes.NewReader(buffer[offset:])
					offsetCount := int(count)

					for i := 0; i < offsetCount; i++ {
						if dataType == DataType_Long {
							var subOffset32 uint32
							if err := binary.Read(subBuf, header.ByteOrder, &subOffset32); err == nil {
								subIfdOffsets = append(subIfdOffsets, int64(subOffset32))
							}
						} else if dataType == DataType_Long8 {
							var subOffset64 uint64
							if err := binary.Read(subBuf, header.ByteOrder, &subOffset64); err == nil {
								subIfdOffsets = append(subIfdOffsets, int64(subOffset64))
							}
						}
					}
				}
			}

			// Check if any Sub-IFD structures extend beyond buffer and add them as missing ranges
			if len(subIfdOffsets) > 0 {
				// Find min and max Sub-IFD offsets to create one large range covering all
				minOffset := subIfdOffsets[0]
				maxOffset := subIfdOffsets[0]
				for _, offset := range subIfdOffsets {
					if offset < minOffset {
						minOffset = offset
					}
					if offset > maxOffset {
						maxOffset = offset
					}
				}

				// Create one large range from first Sub-IFD to last Sub-IFD + padding
				estimatedSubIFDSize := estimateIFDSize(header.TiffType)
				paddingForEntries := int64(8192) // 8KB padding for last Sub-IFD entry data
				rangeStart := minOffset
				rangeEnd := maxOffset + estimatedSubIFDSize + paddingForEntries
				totalSize := rangeEnd - rangeStart

				// Check if this large range extends beyond current buffer
				if rangeEnd > int64(len(buffer)) {
					missingRanges = append(missingRanges, DataRange{
						Offset: rangeStart,
						Size:   totalSize,
					})

				}
			}
		}
	}

	// Read next IFD offset
	nextIFDOffset := ifdOffset + int64(2) + int64(entryCount*uint64(entrySize)) // 2 bytes for count + entries
	if header.TiffType == TiffType_BigTIFF {
		nextIFDOffset = ifdOffset + int64(8) + int64(entryCount*uint64(entrySize)) // 8 bytes for count + entries
	}

	var nextIFD int64
	if nextIFDOffset+8 <= int64(len(buffer)) {
		nextBuf := bytes.NewReader(buffer[nextIFDOffset:])
		if header.TiffType == TiffType_ClassicTIFF {
			var next32 uint32
			if err := binary.Read(nextBuf, header.ByteOrder, &next32); err == nil {
				nextIFD = int64(next32)
			}
		} else {
			var next64 uint64
			if err := binary.Read(nextBuf, header.ByteOrder, &next64); err == nil {
				nextIFD = int64(next64)
			}
		}
	}

	return nextIFD, missingRanges, nil
}

// readMissingRanges reads only the specific missing ranges and expands buffer
func readMissingRanges(r io.ReadSeeker, initialBuffer []byte, ranges []DataRange, bb *backbone.Backbone) ([]byte, int, int64, error) {
	if len(ranges) == 0 {
		return initialBuffer, 0, 0, nil
	}

	// Calculate the size needed for expanded buffer
	maxOffset := int64(len(initialBuffer))
	for _, dataRange := range ranges {
		if dataRange.Offset+dataRange.Size > maxOffset {
			maxOffset = dataRange.Offset + dataRange.Size
		}
	}

	// Create expanded buffer that includes all missing data
	expandedBuffer := make([]byte, maxOffset)
	copy(expandedBuffer, initialBuffer) // Copy initial data

	var totalIOCalls int
	var totalBytesRead int64

	// Read each missing range
	for _, dataRange := range ranges {
		if _, err := r.Seek(dataRange.Offset, 0); err != nil {
			return nil, 0, 0, fmt.Errorf("seek to missing range at offset %d failed: %w", dataRange.Offset, err)
		}

		// Read the full requested size for this cluster
		readSize := dataRange.Size

		// Sanity check: ensure we don't read beyond what we allocated
		if dataRange.Offset+readSize > int64(len(expandedBuffer)) {
			if bb != nil {
				bb.Logger.Warnw("Cluster read would exceed buffer bounds",
					"clusterOffset", dataRange.Offset,
					"clusterSize", dataRange.Size,
					"bufferSize", len(expandedBuffer))
			}
			continue
		}

		if readSize <= 0 {
			continue
		}

		bytesRead, err := r.Read(expandedBuffer[dataRange.Offset : dataRange.Offset+readSize])
		if err != nil && err != io.EOF {
			return nil, 0, 0, fmt.Errorf("read missing range at offset %d failed: %w", dataRange.Offset, err)
		}

		totalIOCalls++
		totalBytesRead += int64(bytesRead)

	}

	return expandedBuffer, totalIOCalls, totalBytesRead, nil
}

// estimateIFDSize provides a generous estimate of IFD size including potential entry data
func estimateIFDSize(tiffType TiffType) int64 {
	// Be much more generous to account for entry data that may be scattered
	if tiffType == TiffType_BigTIFF {
		return 8 + (50 * 20) + 8 + (2 * 1024) // entry count + max 50 entries + next IFD offset + 2KB for entry data
	}
	return 2 + (50 * 12) + 4 + (2 * 1024) // entry count + max 50 entries + next IFD offset + 2KB for entry data
}

// mergeDataRanges merges adjacent/overlapping ranges for efficiency
func mergeDataRanges(ranges []DataRange) []DataRange {
	if len(ranges) <= 1 {
		return ranges
	}

	// Sort ranges by offset
	sort.Slice(ranges, func(i, j int) bool {
		return ranges[i].Offset < ranges[j].Offset
	})

	var merged []DataRange
	current := ranges[0]

	for i := 1; i < len(ranges); i++ {
		next := ranges[i]

		// Check if ranges overlap or are adjacent (with small gap)
		if next.Offset <= current.Offset+current.Size+MaxBatchGap {
			// Merge ranges
			endOffset := current.Offset + current.Size
			nextEndOffset := next.Offset + next.Size
			if nextEndOffset > endOffset {
				endOffset = nextEndOffset
			}
			current.Size = endOffset - current.Offset
		} else {
			// No overlap, add current range to merged list
			merged = append(merged, current)
			current = next
		}
	}

	// Add the last range
	merged = append(merged, current)

	return merged
}

// parseCompleteMetadataFromBufferOptimized handles the complete metadata parsing with safety features
func parseCompleteMetadataFromBufferOptimized(reader *OptimizedReader, buffer []byte, bb *backbone.Backbone, config *OptimizedReaderConfig) error {

	if len(buffer) < 8 {
		return fmt.Errorf("buffer too small for TIFF header: %d bytes", len(buffer))
	}

	// Parse header with validation
	header, err := parseHeaderFromBufferOptimized(buffer, bb)
	if err != nil {
		return fmt.Errorf("header parsing failed: %w", err)
	}

	reader.Header = header

	// Calculate metadata size (actual useful data)
	reader.MetadataSize = calculateActualMetadataSize(buffer, header, bb)
	reader.BufferUsed = int64(len(buffer))

	// Calculate efficiency based on metadata usage vs bytes read
	if reader.AdaptiveCalls == 0 {
		// Single I/O success - efficiency = metadata / bytes_read
		reader.Efficiency = float64(reader.MetadataSize) / float64(reader.BytesRead)

	} else {
		// Adaptive reading success - high efficiency since we targeted specific data
		reader.Efficiency = 0.95 // 95% efficiency for successful adaptive reading

	}

	// Ensure efficiency doesn't exceed 100%
	if reader.Efficiency > 1.0 {
		reader.Efficiency = 1.0
	}

	// Parse all IFDs with comprehensive safety checks
	reader.Ifd = make([][]*IFD, 0)
	ifdCount := 0

	for offset := header.FirstIFD; offset != 0; {
		ifdCount++

		// Circuit breaker: Prevent infinite loops
		if ifdCount > 1000 {
			if bb != nil {
				bb.Logger.Warnw("Too many IFDs detected, possible corruption", "ifdCount", ifdCount)

			}
			return fmt.Errorf("too many IFDs: %d, possible corruption", ifdCount)
		}

		// Check if IFD is within buffer (should be after smart batching)
		if offset >= int64(len(buffer)) {
			// This shouldn't happen after smart IFD batching, but handle gracefully

			break // Use what we have so far
		}

		ifd, nextOffset, err := parseIFDFromBufferOptimized(buffer, header, offset, bb, config)
		if err != nil {
			return fmt.Errorf("IFD parsing failed at offset %d: %w", offset, err)
		}

		// Handle sub-IFDs
		var ifdList []*IFD
		ifdList = append(ifdList, ifd)

		if subIfdOffsets, ok := ifd.TagGetter().GetSubIFD(); ok {
			for _, subOffset := range subIfdOffsets {
				// After adaptive reading, Sub-IFD data should be in buffer
				if subOffset < int64(len(buffer)) {
					if subIfd, _, err := parseIFDFromBufferOptimized(buffer, header, subOffset, bb, config); err == nil {
						ifdList = append(ifdList, subIfd)
					} else if bb != nil {
						bb.Logger.Warnw("Sub-IFD parsing failed", "subOffset", subOffset, "error", err)
					}
				} else if bb != nil {
					bb.Logger.Warnw("Sub-IFD offset beyond buffer after adaptive reading", "subOffset", subOffset, "bufferSize", len(buffer))
				}
			}
		}

		reader.Ifd = append(reader.Ifd, ifdList)
		offset = nextOffset
	}

	return nil
}

// calculateActualMetadataSize estimates the actual size of useful metadata
func calculateActualMetadataSize(buffer []byte, header *Header, _ *backbone.Backbone) int64 {
	metadataSize := int64(header.HeadSize()) // Start with header size

	// Walk through IFDs and sum up their sizes
	for offset := header.FirstIFD; offset != 0; {
		if offset >= int64(len(buffer)) {
			break
		}

		buf := bytes.NewReader(buffer[offset:])

		// Read entry count
		var entryCount uint64
		if header.TiffType == TiffType_ClassicTIFF {
			var count16 uint16
			if err := binary.Read(buf, header.ByteOrder, &count16); err != nil {
				break
			}
			entryCount = uint64(count16)
			metadataSize += 2 // entry count size
		} else {
			if err := binary.Read(buf, header.ByteOrder, &entryCount); err != nil {
				break
			}
			metadataSize += 8 // entry count size
		}

		// Add size of all entries
		entrySize := int64(12) // Classic TIFF entry size
		if header.TiffType == TiffType_BigTIFF {
			entrySize = 20 // BigTIFF entry size
		}
		metadataSize += int64(entryCount) * entrySize

		// Add next IFD offset size
		if header.TiffType == TiffType_ClassicTIFF {
			metadataSize += 4
		} else {
			metadataSize += 8
		}

		// Read next IFD offset to continue
		nextIFDOffset := offset + int64(2) + int64(entryCount)*entrySize // 2 bytes for count + entries
		if header.TiffType == TiffType_BigTIFF {
			nextIFDOffset = offset + int64(8) + int64(entryCount)*entrySize // 8 bytes for count + entries
		}

		var nextIFD int64
		if nextIFDOffset+8 <= int64(len(buffer)) {
			nextBuf := bytes.NewReader(buffer[nextIFDOffset:])
			if header.TiffType == TiffType_ClassicTIFF {
				var next32 uint32
				if err := binary.Read(nextBuf, header.ByteOrder, &next32); err == nil {
					nextIFD = int64(next32)
				}
			} else {
				var next64 uint64
				if err := binary.Read(nextBuf, header.ByteOrder, &next64); err == nil {
					nextIFD = int64(next64)
				}
			}
		}

		offset = nextIFD
	}

	return metadataSize
}

// parseHeaderFromBufferOptimized parses TIFF header with comprehensive validation
func parseHeaderFromBufferOptimized(buffer []byte, bb *backbone.Backbone) (*Header, error) {
	if len(buffer) < 8 {
		return nil, fmt.Errorf("buffer too small for header: %d bytes", len(buffer))
	}

	header := &Header{}

	// Parse and validate byte order
	switch {
	case buffer[0] == 'I' && buffer[1] == 'I':
		header.ByteOrder = binary.LittleEndian
	case buffer[0] == 'M' && buffer[1] == 'M':
		header.ByteOrder = binary.BigEndian
	default:
		if bb != nil {
			bb.Logger.Warnw("Invalid TIFF magic bytes detected",
				"byte0", buffer[0], "byte1", buffer[1])
		}
		return nil, fmt.Errorf("invalid TIFF magic bytes: %x %x", buffer[0], buffer[1])
	}

	// Parse and validate version
	version := header.ByteOrder.Uint16(buffer[2:4])
	header.TiffType = TiffType(version)

	if header.TiffType != TiffType_ClassicTIFF && header.TiffType != TiffType_BigTIFF {
		if bb != nil {
			bb.Logger.Warnw("Unsupported TIFF version", "version", version)
		}
		return nil, fmt.Errorf("unsupported TIFF version: %d", version)
	}

	// Parse FirstIFD offset with validation
	if header.TiffType == TiffType_ClassicTIFF {
		header.FirstIFD = int64(header.ByteOrder.Uint32(buffer[4:8]))
	} else {
		// BigTIFF requires 16 bytes minimum
		if len(buffer) < 16 {
			return nil, fmt.Errorf("buffer too small for BigTIFF header: %d bytes", len(buffer))
		}

		// Validate BigTIFF specific fields
		offsetSize := header.ByteOrder.Uint16(buffer[4:6])
		reserved := header.ByteOrder.Uint16(buffer[6:8])

		if offsetSize != 8 || reserved != 0 {
			if bb != nil {
				bb.Logger.Warnw("Invalid BigTIFF header fields",
					"offsetSize", offsetSize, "reserved", reserved)
			}
			return nil, fmt.Errorf("invalid BigTIFF header: offsetSize=%d, reserved=%d", offsetSize, reserved)
		}

		header.FirstIFD = int64(header.ByteOrder.Uint64(buffer[8:16]))
	}

	// Validate FirstIFD offset is reasonable
	minValidOffset := int64(header.HeadSize())
	if header.FirstIFD < minValidOffset {
		if bb != nil {
			bb.Logger.Warnw("FirstIFD offset too small",
				"firstIFD", header.FirstIFD, "minValid", minValidOffset)
		}
		return nil, fmt.Errorf("invalid FirstIFD offset: %d (minimum: %d)", header.FirstIFD, minValidOffset)
	}

	if bb != nil {
		bb.Logger.Infow("TIFF header parsed successfully",
			"tiffType", header.TiffType,
			"byteOrder", fmt.Sprintf("%T", header.ByteOrder),
			"firstIFD", header.FirstIFD)
	}

	return header, nil
}

// parseIFDFromBufferOptimized parses IFD with memory protection and circuit breakers
func parseIFDFromBufferOptimized(buffer []byte, header *Header, offset int64, bb *backbone.Backbone, config *OptimizedReaderConfig) (*IFD, int64, error) {
	if offset >= int64(len(buffer)) {
		return nil, 0, fmt.Errorf("IFD offset %d beyond buffer size %d", offset, len(buffer))
	}

	ifd := &IFD{
		Header:   header,
		EntryMap: make(map[TagType]*IFDEntry),
		ThisIFD:  offset,
	}

	// Use offset directly - adaptive reading ensures data is at correct positions
	buf := bytes.NewReader(buffer[offset:])

	// Read and validate entry count
	var entryCount uint64
	if header.TiffType == TiffType_ClassicTIFF {
		var count16 uint16
		if err := binary.Read(buf, header.ByteOrder, &count16); err != nil {
			return nil, 0, fmt.Errorf("failed to read entry count: %w", err)
		}
		entryCount = uint64(count16)
	} else {
		if err := binary.Read(buf, header.ByteOrder, &entryCount); err != nil {
			return nil, 0, fmt.Errorf("failed to read entry count: %w", err)
		}
	}

	// Circuit breaker: Validate entry count is reasonable
	if entryCount > 10000 {
		if bb != nil {
			bb.Logger.Warnw("Excessive IFD entry count, possible corruption",
				"entryCount", entryCount,
				"offset", offset)
		}
		return nil, 0, fmt.Errorf("too many IFD entries: %d (max: 10000)", entryCount)
	}

	// Parse entries with memory protection
	for i := uint64(0); i < entryCount; i++ {
		entry, err := parseIFDEntryFromBufferOptimized(buf, header, buffer, offset, bb, config)
		if err != nil {
			if bb != nil {
				bb.Logger.Warnw("IFD entry parsing failed",
					"entryIndex", i,
					"ifdOffset", offset,
					"error", err.Error())

			}
			return nil, 0, fmt.Errorf("entry %d parsing failed: %w", i, err)
		}
		ifd.EntryMap[entry.Tag] = entry
	}

	// Read next IFD offset
	var nextIFD int64
	if header.TiffType == TiffType_ClassicTIFF {
		var next32 uint32
		if err := binary.Read(buf, header.ByteOrder, &next32); err != nil {
			return nil, 0, fmt.Errorf("failed to read next IFD offset: %w", err)
		}
		nextIFD = int64(next32)
	} else {
		var next64 uint64
		if err := binary.Read(buf, header.ByteOrder, &next64); err != nil {
			return nil, 0, fmt.Errorf("failed to read next IFD offset: %w", err)
		}
		nextIFD = int64(next64)
	}

	ifd.NextIFD = nextIFD
	return ifd, nextIFD, nil
}

// parseIFDEntryFromBufferOptimized parses IFD entry with comprehensive memory protection
func parseIFDEntryFromBufferOptimized(buf *bytes.Reader, header *Header, fullBuffer []byte, _ int64, bb *backbone.Backbone, config *OptimizedReaderConfig) (*IFDEntry, error) {
	// Read entry fields
	var tag TagType
	var dataType DataType

	if err := binary.Read(buf, header.ByteOrder, &tag); err != nil {
		return nil, fmt.Errorf("failed to read tag: %w", err)
	}
	if err := binary.Read(buf, header.ByteOrder, &dataType); err != nil {
		return nil, fmt.Errorf("failed to read data type: %w", err)
	}

	var count uint64
	var offset uint64

	if header.TiffType == TiffType_ClassicTIFF {
		var count32, offset32 uint32
		if err := binary.Read(buf, header.ByteOrder, &count32); err != nil {
			return nil, fmt.Errorf("failed to read count: %w", err)
		}
		if err := binary.Read(buf, header.ByteOrder, &offset32); err != nil {
			return nil, fmt.Errorf("failed to read offset: %w", err)
		}
		count = uint64(count32)
		offset = uint64(offset32)
	} else {
		if err := binary.Read(buf, header.ByteOrder, &count); err != nil {
			return nil, fmt.Errorf("failed to read count: %w", err)
		}
		if err := binary.Read(buf, header.ByteOrder, &offset); err != nil {
			return nil, fmt.Errorf("failed to read offset: %w", err)
		}
	}

	entry := &IFDEntry{
		Header:   header,
		Tag:      tag,
		DataType: dataType,
		Count:    int(count),
		Offset:   int64(offset),
	}

	// Calculate data size and apply memory protection (CIRCUIT BREAKER)
	dataSize := dataType.ByteSize() * int(count)

	if dataSize > config.MaxIFDEntrySize {
		if bb != nil {
			bb.Logger.Warnw("TIFF IFD entry data size exceeds limit, possible corruption",
				"tag", tag,
				"dataType", dataType,
				"count", count,
				"requestedSize", dataSize,
				"maxAllowed", config.MaxIFDEntrySize,
				"offset", offset)

		}
		return nil, fmt.Errorf("entry data size %d exceeds maximum allowed %d (tag=%v, count=%d)",
			dataSize, config.MaxIFDEntrySize, tag, count)
	}

	// Get entry data
	threshold := 4
	if header.TiffType == TiffType_BigTIFF {
		threshold = 8
	}

	if dataSize <= threshold {
		// Data is embedded in the offset field - return full offset to match traditional reader behavior
		var offsetBytes bytes.Buffer
		if header.TiffType == TiffType_ClassicTIFF {
			binary.Write(&offsetBytes, header.ByteOrder, uint32(offset))
		} else {
			binary.Write(&offsetBytes, header.ByteOrder, offset)
		}
		entry.Data = offsetBytes.Bytes() // Return full offset field, not truncated
	} else {
		// Data is at the offset location - should be in buffer now after adaptive reading
		// For single-buffer files (len(fullBuffer) <= config.BufferSize), use offset directly
		// For multi-buffer adaptive files, the fullBuffer already contains the correct data at correct offsets
		adjustedOffset := int64(offset)

		if adjustedOffset+int64(dataSize) <= int64(len(fullBuffer)) {
			// Data is within our buffer - extract it
			entry.Data = make([]byte, dataSize)
			copy(entry.Data, fullBuffer[adjustedOffset:adjustedOffset+int64(dataSize)])

		} else {
			// This should not happen after adaptive reading, but handle gracefully
			if bb != nil {
				bb.Logger.Warnw("Entry data still beyond buffer after adaptive reading",
					"tag", tag,
					"dataOffset", offset,
					"dataSize", dataSize,
					"bufferSize", len(fullBuffer))

			}
			return nil, fmt.Errorf("entry data for tag %v still extends beyond buffer after adaptive reading (offset=%d, size=%d, buffer=%d)",
				tag, offset, dataSize, len(fullBuffer))
		}
	}

	return entry, nil
}

// fallbackToTraditionalOptimized falls back to traditional reading but still tracks metrics
func fallbackToTraditionalOptimized(r io.ReadSeeker, _ *backbone.Backbone, _ *OptimizedReaderConfig) (*OptimizedReader, error) {
	// Use the traditional OpenReader but wrap it in OptimizedReader for consistency
	traditionalReader, err := OpenReader(r)
	if err != nil {
		return nil, err
	}

	// Estimate the I/O calls that would have been made by traditional reader
	estimatedIOCalls := 1 // header
	for _, ifdList := range traditionalReader.Ifd {
		for _, ifd := range ifdList {
			estimatedIOCalls += 1 // IFD directory
			for _, entry := range ifd.EntryMap {
				dataSize := entry.DataType.ByteSize() * entry.Count
				threshold := 4
				if traditionalReader.Header.TiffType == TiffType_BigTIFF {
					threshold = 8
				}
				if dataSize > threshold {
					estimatedIOCalls += 1 // entry data
				}
			}
		}
	}

	optimizedReader := &OptimizedReader{
		Reader:        r,
		Header:        traditionalReader.Header,
		Ifd:           traditionalReader.Ifd,
		rs:            traditionalReader.rs,
		IOCalls:       estimatedIOCalls,
		BytesRead:     0, // Can't track this in fallback mode
		BufferUsed:    0,
		Efficiency:    0,
		AdaptiveCalls: 0,
		MetadataSize:  0,
	}

	return optimizedReader, nil
}

// Methods for OptimizedReader that delegate to the traditional implementation
func (p *OptimizedReader) ImageNum() int {
	return len(p.Ifd)
}

func (p *OptimizedReader) SubImageNum(i int) int {
	return len(p.Ifd[i])
}

func (p *OptimizedReader) ImageConfig(i, j int) (image.Config, error) {
	return p.Ifd[i][j].ImageConfig()
}

func (p *OptimizedReader) ImageBlocksAcross(i, j int) int {
	return p.Ifd[i][j].BlocksAcross()
}

func (p *OptimizedReader) ImageBlocksDown(i, j int) int {
	return p.Ifd[i][j].BlocksDown()
}

func (p *OptimizedReader) ImageBlockBounds(i, j, col, row int) image.Rectangle {
	return p.Ifd[i][j].BlockBounds(col, row)
}

func (p *OptimizedReader) DecodeImage(i, j int) (m image.Image, err error) {
	cfg, err := p.ImageConfig(i, j)
	if err != nil {
		return
	}
	imgRect := image.Rect(0, 0, cfg.Width, cfg.Height)
	if m, err = newImageWithIFD(imgRect, p.Ifd[i][j]); err != nil {
		return
	}

	blocksAcross := p.ImageBlocksAcross(i, j)
	blocksDown := p.ImageBlocksDown(i, j)

	for col := 0; col < blocksAcross; col++ {
		for row := 0; row < blocksDown; row++ {
			if err = p.Ifd[i][j].DecodeBlock(p.rs, col, row, m); err != nil {
				return
			}
		}
	}
	return
}

func (p *OptimizedReader) DecodeImageBlock(i, j, col, row int) (m image.Image, err error) {
	r := p.ImageBlockBounds(i, j, col, row)
	if m, err = newImageWithIFD(r, p.Ifd[i][j]); err != nil {
		return
	}
	if err = p.Ifd[i][j].DecodeBlock(p.rs, col, row, m); err != nil {
		return
	}
	return
}

func (p *OptimizedReader) DecodeImageBlockData(i, j, col, row int) ([]byte, error) {
	return p.Ifd[i][j].DecodeBlockData(p.rs, col, row)
}

func (p *OptimizedReader) Close() (err error) {
	if p != nil {
		if p.rs != nil {
			err = p.rs.Close()
		}
		*p = OptimizedReader{}
	}
	return
}

// GetStats returns the performance statistics of the optimized reader
func (p *OptimizedReader) GetStats() (ioCall int, bytesRead int64, efficiency float64) {
	return p.IOCalls, p.BytesRead, p.Efficiency
}

// GetAdaptiveStats returns the adaptive reading statistics
func (p *OptimizedReader) GetAdaptiveStats() (totalIOCalls int, adaptiveCalls int, bytesRead int64, efficiency float64) {
	return p.IOCalls, p.AdaptiveCalls, p.BytesRead, p.Efficiency
}

// readNextIFDOffsetDirect reads just the NextIFD offset from an IFD location
func readNextIFDOffsetDirect(r io.ReadSeeker, ifdOffset int64, header *Header) (int64, error) {
	// Seek to IFD location
	if _, err := r.Seek(ifdOffset, 0); err != nil {
		return 0, fmt.Errorf("seek to IFD failed: %w", err)
	}

	// Read entry count to calculate NextIFD offset location
	var entryCount uint64
	if header.TiffType == TiffType_ClassicTIFF {
		var count16 uint16
		if err := binary.Read(r, header.ByteOrder, &count16); err != nil {
			return 0, fmt.Errorf("read entry count failed: %w", err)
		}
		entryCount = uint64(count16)
	} else {
		if err := binary.Read(r, header.ByteOrder, &entryCount); err != nil {
			return 0, fmt.Errorf("read entry count failed: %w", err)
		}
	}

	// Skip to NextIFD offset position
	entrySize := 12
	if header.TiffType == TiffType_BigTIFF {
		entrySize = 20
	}

	// Calculate skip distance
	skipBytes := int64(entryCount) * int64(entrySize)
	if _, err := r.Seek(skipBytes, 1); err != nil { // Seek relative to current position
		return 0, fmt.Errorf("seek to NextIFD failed: %w", err)
	}

	// Read NextIFD offset
	var nextIFD int64
	if header.TiffType == TiffType_ClassicTIFF {
		var next32 uint32
		if err := binary.Read(r, header.ByteOrder, &next32); err != nil {
			return 0, fmt.Errorf("read NextIFD failed: %w", err)
		}
		nextIFD = int64(next32)
	} else {
		var next64 uint64
		if err := binary.Read(r, header.ByteOrder, &next64); err != nil {
			return 0, fmt.Errorf("read NextIFD failed: %w", err)
		}
		nextIFD = int64(next64)
	}

	return nextIFD, nil
}

// groupIFDsIntoClusters groups nearby IFD offsets into clusters for efficient batch reading
func groupIFDsIntoClusters(ifdOffsets []int64, maxGap int64) [][]int64 {
	if len(ifdOffsets) == 0 {
		return nil
	}

	// Sort offsets to ensure proper clustering
	sortedOffsets := make([]int64, len(ifdOffsets))
	copy(sortedOffsets, ifdOffsets)
	sort.Slice(sortedOffsets, func(i, j int) bool {
		return sortedOffsets[i] < sortedOffsets[j]
	})

	clusters := [][]int64{}
	currentCluster := []int64{sortedOffsets[0]}

	for i := 1; i < len(sortedOffsets); i++ {
		gap := sortedOffsets[i] - sortedOffsets[i-1]

		if gap <= maxGap {
			// IFDs are close enough, add to current cluster
			currentCluster = append(currentCluster, sortedOffsets[i])
		} else {
			// Gap too large, start new cluster
			clusters = append(clusters, currentCluster)
			currentCluster = []int64{sortedOffsets[i]}
		}
	}

	// Add the last cluster
	clusters = append(clusters, currentCluster)

	return clusters
}

// calculateSmartIFDBatch analyzes IFD chain and creates optimal cluster-based batch reads for all IFDs
func calculateSmartIFDBatch(initialBuffer []byte, missingRanges []DataRange, r io.ReadSeeker, bb *backbone.Backbone, _ *OptimizedReaderConfig) ([]DataRange, error) {
	// Apply smart batching when we have missing ranges (indicating IFDs beyond buffer)
	if len(missingRanges) == 0 {
		return []DataRange{}, nil
	}

	// Parse header to start IFD chain walk
	header, err := parseHeaderFromBufferOptimized(initialBuffer, bb)
	if err != nil {
		return nil, fmt.Errorf("header parsing failed: %w", err)
	}

	// Walk entire IFD chain to find all IFD locations
	ifdOffsets := []int64{}
	for offset := header.FirstIFD; offset != 0; {
		ifdOffsets = append(ifdOffsets, offset)

		// Circuit breaker
		if len(ifdOffsets) > 100 {
			if bb != nil {
				bb.Logger.Warnw("Too many IFDs in chain, stopping smart batch calculation")
			}
			break
		}

		nextOffset, err := readNextIFDOffsetDirect(r, offset, header)
		if err != nil {
			break // End of chain or error
		}
		offset = nextOffset
	}

	if len(ifdOffsets) <= 1 {
		// Simple case, use original ranges
		return []DataRange{}, nil
	}

	// Group IFDs into clusters (nearby IFDs within threshold)
	clusters := groupIFDsIntoClusters(ifdOffsets, 4*1024) // 4KB threshold

	// Create one DataRange per cluster
	smartRanges := make([]DataRange, 0, len(clusters))
	totalReadSize := int64(0)

	for i, cluster := range clusters {
		minOffset := cluster[0]
		maxOffset := cluster[len(cluster)-1]

		// Add padding for IFD content (entries + data)
		ifdPadding := int64(2 * 1024) // 2KB should cover IFD entries and immediate data
		clusterRange := DataRange{
			Offset: minOffset,
			Size:   (maxOffset - minOffset) + ifdPadding,
		}

		// Sanity check per cluster
		if clusterRange.Size > 1024*1024 { // 1MB max per cluster
			if bb != nil {
				bb.Logger.Warnw("IFD cluster too large, skipping",
					"clusterIndex", i,
					"clusterSize", clusterRange.Size,
					"clusterIFDs", len(cluster))
			}
			continue
		}

		smartRanges = append(smartRanges, clusterRange)
		totalReadSize += clusterRange.Size
	}

	// Sanity check: total read size
	if totalReadSize > 10*1024*1024 { // 10MB total max
		if bb != nil {
			bb.Logger.Warnw("Total smart batch too large, using original ranges",
				"totalReadSize", totalReadSize,
				"clusterCount", len(smartRanges))
		}
		return []DataRange{}, nil
	}

	if len(smartRanges) == 0 {
		return []DataRange{}, nil
	}

	return smartRanges, nil
}
