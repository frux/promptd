package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/frux/promptd/internal/release"
)

func main() {
	version := flag.String("version", "", "v-prefixed semantic release version")
	commit := flag.String("commit", "", "source commit identifier")
	epoch := flag.Int64("epoch", 0, "SOURCE_DATE_EPOCH timestamp")
	output := flag.String("output", "dist", "release output directory")
	flag.Parse()
	if flag.NArg() != 0 {
		fatalf("unexpected positional arguments: %v", flag.Args())
	}
	root, err := os.Getwd()
	if err != nil {
		fatalf("resolve repository root: %v", err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		fatalf("resolve repository root: %v", err)
	}
	resolvedOutput := *output
	if !filepath.IsAbs(resolvedOutput) {
		resolvedOutput = filepath.Join(root, resolvedOutput)
	}
	resolvedEpoch := *epoch
	if resolvedEpoch == 0 {
		if value := os.Getenv("SOURCE_DATE_EPOCH"); value != "" {
			resolvedEpoch, err = strconv.ParseInt(value, 10, 64)
			if err != nil {
				fatalf("parse SOURCE_DATE_EPOCH: %v", err)
			}
		}
	}
	if resolvedEpoch <= 0 {
		fatalf("--epoch or SOURCE_DATE_EPOCH must be a positive Unix timestamp")
	}

	artifacts, err := release.Build(context.Background(), release.Options{
		Root:       root,
		Output:     resolvedOutput,
		Version:    *version,
		Commit:     *commit,
		SourceDate: time.Unix(resolvedEpoch, 0).UTC(),
	})
	if err != nil {
		fatalf("package release: %v", err)
	}
	for _, artifact := range artifacts {
		fmt.Printf("%s  %s\n", artifact.SHA256, artifact.ArchivePath)
	}
}

func fatalf(format string, arguments ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", arguments...)
	os.Exit(1)
}
