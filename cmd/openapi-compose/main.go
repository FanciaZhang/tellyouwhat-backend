package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/tellyouwhat/backend/internal/openapicompose"
)

func main() {
	platformPath := flag.String("platform", "", "canonical platform OpenAPI document")
	appPath := flag.String("app", "", "canonical App-specific OpenAPI document")
	outputPath := flag.String("output", "", "composed public OpenAPI output")
	flag.Parse()
	if *platformPath == "" || *appPath == "" || *outputPath == "" {
		fatalf("platform, app, and output are required")
	}
	platformSource, err := os.ReadFile(*platformPath)
	if err != nil {
		fatalf("read platform contract: %v", err)
	}
	appSource, err := os.ReadFile(*appPath)
	if err != nil {
		fatalf("read App contract: %v", err)
	}
	output, err := openapicompose.Compose(platformSource, appSource)
	if err != nil {
		fatalf("compose contract: %v", err)
	}
	if err := writeAtomic(*outputPath, output); err != nil {
		fatalf("write composed contract: %v", err)
	}
}

func writeAtomic(path string, value []byte) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".openapi-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := temporary.Write(value); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Chmod(0o644); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func fatalf(format string, values ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", values...)
	os.Exit(1)
}
