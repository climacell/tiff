package main

import (
	"bytes"
	"fmt"
	"os"

	"github.com/climacell/go-middleware/backbone"
	"github.com/climacell/tiff"
)

func main() {
	filename := "tp_image0.tif"

	// Traditional reader
	fmt.Printf("=== TRADITIONAL READER ===\n")
	f1, err := os.Open(filename)
	if err != nil {
		fmt.Printf("Error opening file: %v\n", err)
		return
	}
	defer f1.Close()

	traditional, err := tiff.OpenReader(f1)
	if err != nil {
		fmt.Printf("Traditional reader error: %v\n", err)
		return
	}

	// Find the problematic entry
	var traditionalEntry *tiff.IFDEntry
	if len(traditional.Ifd) > 0 && len(traditional.Ifd[0]) > 0 {
		for _, entry := range traditional.Ifd[0][0].EntryMap {
			if entry.Tag == tiff.TagType_INGRPacketDataTag {
				traditionalEntry = entry
				break
			}
		}
	}

	if traditionalEntry != nil {
		fmt.Printf("Traditional INGRPacketDataTag:\n")
		fmt.Printf("  Offset: %d\n", traditionalEntry.Offset)
		fmt.Printf("  Count: %d\n", traditionalEntry.Count)
		fmt.Printf("  DataType: %v\n", traditionalEntry.DataType)
		fmt.Printf("  Data length: %d\n", len(traditionalEntry.Data))
		fmt.Printf("  First 16 bytes: %x\n", traditionalEntry.Data[:min(16, len(traditionalEntry.Data))])
	} else {
		fmt.Printf("Traditional: INGRPacketDataTag not found\n")
	}

	// Optimized reader (using same method as test)
	fmt.Printf("\n=== OPTIMIZED READER ===\n")
	data, err := os.ReadFile(filename)
	if err != nil {
		fmt.Printf("Error reading file: %v\n", err)
		return
	}

	bb := backbone.NewNoOpBackbone()
	// Enable debug-level logging to trace the issue
	// bb := backbone.NewBackbone(logger, statsd) // Would need proper logger setup

	// Test multiple buffer sizes like the test suite
	bufferSizes := []int{1 * 1024, 64 * 1024}

	for _, bufferSize := range bufferSizes {
		fmt.Printf("\n--- Buffer Size: %dKB ---\n", bufferSize/1024)
		optimized, err := tiff.OpenReaderOptimized(bytes.NewReader(data), bb, tiff.WithBufferSize(bufferSize))
		if err != nil {
			fmt.Printf("Optimized reader error: %v\n", err)
			continue
		}

		// Find the problematic entry
		var optimizedEntry *tiff.IFDEntry
		if len(optimized.Ifd) > 0 && len(optimized.Ifd[0]) > 0 {
			for _, entry := range optimized.Ifd[0][0].EntryMap {
				if entry.Tag == tiff.TagType_INGRPacketDataTag {
					optimizedEntry = entry
					break
				}
			}
		}

		if optimizedEntry != nil {
			fmt.Printf("Optimized INGRPacketDataTag:\n")
			fmt.Printf("  Offset: %d\n", optimizedEntry.Offset)
			fmt.Printf("  Count: %d\n", optimizedEntry.Count)
			fmt.Printf("  DataType: %v\n", optimizedEntry.DataType)
			fmt.Printf("  Data length: %d\n", len(optimizedEntry.Data))
			fmt.Printf("  First 16 bytes: %x\n", optimizedEntry.Data[:min(16, len(optimizedEntry.Data))])

			// Compare with traditional
			if traditionalEntry != nil {
				fmt.Printf("  Match with traditional: %v\n", bytes.Equal(traditionalEntry.Data, optimizedEntry.Data))
				if !bytes.Equal(traditionalEntry.Data, optimizedEntry.Data) {
					for j := 0; j < len(traditionalEntry.Data) && j < len(optimizedEntry.Data); j++ {
						if traditionalEntry.Data[j] != optimizedEntry.Data[j] {
							fmt.Printf("  First difference at byte %d: traditional=0x%02x, optimized=0x%02x\n",
								j, traditionalEntry.Data[j], optimizedEntry.Data[j])
							break
						}
					}
				}
			}
		} else {
			fmt.Printf("Optimized: INGRPacketDataTag not found\n")
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
