package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"viewer/internal/batch/dedupe"
	cfgpkg "viewer/internal/config"
	"viewer/internal/storage"
)

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		printUsage()
		return fmt.Errorf("command is required")
	}

	switch args[0] {
	case "plan":
		return runPlan(ctx, args[1:])
	case "apply":
		return runApply(ctx, args[1:])
	case "-h", "--help", "help":
		printUsage()
		return nil
	default:
		printUsage()
		return fmt.Errorf("unknown command: %s", args[0])
	}
}

func runPlan(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	outPath := fs.String("out", "", "Output plan JSON path")
	limit := fs.Int("limit", 0, "Limit scanned source.zip objects (0 = all)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*outPath) == "" {
		return fmt.Errorf("--out is required")
	}

	cfg, err := cfgpkg.LoadS3Only()
	if err != nil {
		return err
	}
	store, err := storage.NewS3Store(ctx, cfg)
	if err != nil {
		return err
	}

	planner := dedupe.NewPlanner(store, cfg.S3Bucket)
	plan, summary, err := planner.BuildPlan(ctx, dedupe.PlanOptions{Limit: *limit})
	if err != nil {
		return err
	}
	if err := dedupe.WritePlanFile(*outPath, plan); err != nil {
		return err
	}

	fmt.Printf("plan written: %s\n", *outPath)
	fmt.Printf("scanned=%d duplicate_groups=%d keep_albums=%d delete_albums=%d delete_keys=%d\n",
		summary.ScannedSources,
		summary.DuplicateGroups,
		summary.KeptAlbums,
		summary.DeletedAlbums,
		summary.DeleteKeys,
	)
	return nil
}

func runApply(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	planPath := fs.String("plan", "", "Input plan JSON path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*planPath) == "" {
		return fmt.Errorf("--plan is required")
	}

	plan, err := dedupe.ReadPlanFile(*planPath)
	if err != nil {
		return err
	}

	cfg, err := cfgpkg.LoadS3Only()
	if err != nil {
		return err
	}
	if cfg.S3Bucket != plan.Bucket {
		return fmt.Errorf("plan bucket mismatch: plan=%s env=%s", plan.Bucket, cfg.S3Bucket)
	}

	store, err := storage.NewS3Store(ctx, cfg)
	if err != nil {
		return err
	}

	result, applyErr := dedupe.ApplyPlan(ctx, store, plan)
	fmt.Printf("apply attempted=%d deleted=%d failed=%d\n", result.Attempted, result.Deleted, len(result.Failed))
	for _, failure := range result.Failed {
		fmt.Printf("failed key=%s error=%s\n", failure.Key, failure.Error)
	}
	return applyErr
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `album-dedupe-cleaner removes duplicate albums by source.zip content.

Usage:
  album-dedupe-cleaner plan --out <plan.json> [--limit N]
  album-dedupe-cleaner apply --plan <plan.json>

Commands:
  plan   scan S3, detect duplicates, and write deletion plan
  apply  validate plan snapshot/fingerprint and execute deletions
`)
}
