package main

import (
	"fmt"
	"os"

	"github.com/climacell/go-middleware/backbone"
	tiff "github.com/climacell/tiff"
)

func main() {
	// Open test.tiff
	file, err := os.Open("test.tiff")
	if err != nil {
		fmt.Printf("Error opening file: %v\n", err)
		return
	}
	defer file.Close()

	// Create backbone
	bb := backbone.NewNoOpBackbone()

	// Test optimized reader
	reader, err := tiff.OpenReaderOptimized(file, bb, tiff.WithBufferSize(512*1024))
	if err != nil {
		fmt.Printf("Error opening optimized reader: %v\n", err)
		return
	}
	defer reader.Close()

	// Print debug info
	fmt.Printf("=== ADAPTIVE READER DEBUG ===\n")
	fmt.Printf("IOCalls: %d\n", reader.IOCalls)
	fmt.Printf("AdaptiveCalls: %d\n", reader.AdaptiveCalls)
	fmt.Printf("BytesRead: %d\n", reader.BytesRead)
	fmt.Printf("BufferUsed: %d\n", reader.BufferUsed)
	fmt.Printf("MetadataSize: %d\n", reader.MetadataSize)
	fmt.Printf("Efficiency: %.3f (%.1f%%)\n", reader.Efficiency, reader.Efficiency*100)

	// Show the calculation
	if reader.AdaptiveCalls == 0 {
		expectedEff := float64(reader.MetadataSize) / float64(reader.BytesRead)
		fmt.Printf("Expected efficiency (single I/O): %.3f = %d / %d\n", expectedEff, reader.MetadataSize, reader.BytesRead)
	} else {
		fmt.Printf("Expected efficiency (adaptive): 0.95 (95%%)\n")
	}
}
